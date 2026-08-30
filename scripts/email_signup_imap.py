#!/usr/bin/env python3
"""Test-only IMAP helper for the live signup email E2E.

The CLI intentionally emits the verification URL only on stdout so the caller
can capture it without placing it in service logs or persistent artifacts.
"""

from __future__ import annotations

import argparse
import email
import html
import imaplib
import json
import os
import re
import socket
import ssl
import subprocess
import sys
import time
from email.header import decode_header, make_header
from email.message import Message
from email.utils import getaddresses
from urllib.parse import parse_qs, urlparse


DEFAULT_EXPECTED_SUBJECT = "Verify your Realtek Connect account"
EXPECTED_SUBJECT = DEFAULT_EXPECTED_SUBJECT
URL_RE = re.compile(r"https?://[^\s<>\"']+")


class IMAPTestError(RuntimeError):
    pass


class _IMAP4SSLWithConnectHost(imaplib.IMAP4_SSL):
    """Connect to an override address while preserving TLS SNI validation."""

    def __init__(
        self,
        host: str,
        connect_host: str,
        port: int,
        *,
        ssl_context: ssl.SSLContext,
        timeout: float,
    ):
        self._connect_host = connect_host
        super().__init__(host, port, ssl_context=ssl_context, timeout=timeout)

    def _create_socket(self, timeout: float | None) -> socket.socket:
        sock = socket.create_connection(
            (self._connect_host, self.port), timeout=timeout
        )
        return self.ssl_context.wrap_socket(sock, server_hostname=self.host)


def _required_env(name: str) -> str:
    value = os.environ.get(name, "").strip()
    if not value:
        raise IMAPTestError(f"missing required test setting: {name}")
    return value


def _security_mode(value: str) -> str:
    normalized = re.sub(r"[^a-z]", "", value.lower())
    if normalized in {"ssl", "tls", "ssltls"}:
        return "ssl"
    if normalized == "starttls":
        return "starttls"
    raise IMAPTestError("IMAP_EMAIL_SECURITY must be SSL/TLS or STARTTLS")


def _connect() -> imaplib.IMAP4:
    host = _required_env("IMAP_SERVER")
    port_text = _required_env("IMAP_EMAIL_PORT")
    username = _required_env("IMAP_EMAIL_ADDR")
    password = _required_env("IMAP_EMAIL_PASSWORD")
    try:
        port = int(port_text)
    except ValueError as exc:
        raise IMAPTestError("IMAP_EMAIL_PORT must be an integer") from exc
    context = _ssl_context()
    mode = _security_mode(_required_env("IMAP_EMAIL_SECURITY"))
    connect_host = os.environ.get("IMAP_CONNECT_HOST", "").strip() or host
    try:
        if mode == "ssl":
            client: imaplib.IMAP4 = _IMAP4SSLWithConnectHost(
                host,
                connect_host,
                port,
                ssl_context=context,
                timeout=15,
            )
        else:
            client = imaplib.IMAP4(connect_host, port, timeout=15)
            status, _ = client.starttls(ssl_context=context)
            if status != "OK":
                raise IMAPTestError("IMAP STARTTLS was rejected")
        status, _ = client.login(username, password)
        if status != "OK":
            raise IMAPTestError("IMAP authentication failed")
        return client
    except (OSError, ssl.SSLError, imaplib.IMAP4.error) as exc:
        raise IMAPTestError("IMAP connection, TLS, or authentication failed") from exc


def _ssl_context() -> ssl.SSLContext:
    context = ssl.create_default_context()
    if sys.platform != "darwin":
        return context
    certificates: list[str] = []
    for keychain in (
        "/System/Library/Keychains/SystemRootCertificates.keychain",
        "/Library/Keychains/System.keychain",
    ):
        try:
            result = subprocess.run(
                ["security", "find-certificate", "-a", "-p", keychain],
                check=True,
                capture_output=True,
                text=True,
                timeout=15,
            )
        except (OSError, subprocess.SubprocessError):
            continue
        certificates.append(result.stdout)
    if not certificates:
        return context
    try:
        context.load_verify_locations(cadata="\n".join(certificates))
    except ssl.SSLError as exc:
        raise IMAPTestError("macOS system CA certificates could not be loaded") from exc
    return context


def _select(client: imaplib.IMAP4) -> str:
    folder = _required_env("IMAP_EMAIL_FOLDER")
    status, _ = client.select(folder, readonly=True)
    if status != "OK":
        raise IMAPTestError("IMAP folder could not be opened read-only")
    return folder


def snapshot_uidnext() -> int:
    client = _connect()
    try:
        folder = _select(client)
        status, data = client.status(folder, "(UIDNEXT)")
        if status != "OK":
            raise IMAPTestError("IMAP UIDNEXT is unavailable")
        match = re.search(rb"UIDNEXT\s+(\d+)", b" ".join(data or []))
        if not match:
            raise IMAPTestError("IMAP UIDNEXT response is invalid")
        return int(match.group(1))
    finally:
        try:
            client.logout()
        except imaplib.IMAP4.error:
            pass


def _decoded_header(message: Message, name: str) -> str:
    value = message.get(name, "")
    try:
        return str(make_header(decode_header(value)))
    except (LookupError, UnicodeError):
        return value


def _addresses(message: Message, names: tuple[str, ...]) -> set[str]:
    values: list[str] = []
    for name in names:
        values.extend(message.get_all(name, []))
    return {address.lower() for _, address in getaddresses(values) if address}


def _body_texts(message: Message) -> list[str]:
    texts: list[str] = []
    parts = message.walk() if message.is_multipart() else [message]
    for part in parts:
        if part.get_content_maintype() == "multipart":
            continue
        if part.get_content_type() not in {"text/plain", "text/html"}:
            continue
        payload = part.get_payload(decode=True)
        if payload is None:
            continue
        charset = part.get_content_charset() or "utf-8"
        texts.append(payload.decode(charset, errors="replace"))
    return texts


def inspect_message(
    raw_message: bytes,
    expected_from: str,
    expected_to: str,
    expected_base_url: str,
) -> dict[str, object] | None:
    message = email.message_from_bytes(raw_message)
    expected_subject = os.environ.get(
        "EMAIL_E2E_EXPECTED_SUBJECT", DEFAULT_EXPECTED_SUBJECT
    ).strip()
    expected_path = os.environ.get(
        "EMAIL_E2E_EXPECTED_PATH", "/signup/verify"
    ).strip()
    if _decoded_header(message, "Subject").strip() != expected_subject:
        return None
    if expected_from.lower() not in _addresses(message, ("From",)):
        return None
    if expected_to.lower() not in _addresses(
        message, ("To", "Cc", "Delivered-To", "X-Original-To")
    ):
        return None
    if not message.get("Message-ID"):
        raise IMAPTestError("verification email is missing Message-ID")
    if message.get_content_type() != "multipart/alternative":
        raise IMAPTestError("verification email is not multipart/alternative")

    base = urlparse(expected_base_url)
    candidates: set[str] = set()
    content_types: set[str] = set()
    for part in message.walk():
        if part.get_content_type() in {"text/plain", "text/html"}:
            content_types.add(part.get_content_type())
    for body in _body_texts(message):
        for candidate in URL_RE.findall(html.unescape(body)):
            parsed = urlparse(candidate.rstrip(".,);"))
            if (
                parsed.scheme == base.scheme
                and parsed.netloc == base.netloc
                and parsed.path.rstrip("/") == expected_path.rstrip("/")
                and parse_qs(parsed.query).get("token", [""])[0]
            ):
                candidates.add(candidate.rstrip(".,);"))
    if content_types != {"text/plain", "text/html"}:
        raise IMAPTestError("verification email must contain text and HTML parts")
    if not candidates:
        return None
    if len(candidates) != 1:
        raise IMAPTestError("verification email must contain one matching URL")
    return {
        "url": next(iter(candidates)),
        "message_id_present": True,
        "multipart_alternative": True,
        "text_part_present": True,
        "html_part_present": True,
    }


def _raw_from_fetch(data: list[object] | None) -> bytes | None:
    for item in data or []:
        if isinstance(item, tuple) and len(item) >= 2 and isinstance(item[1], bytes):
            return item[1]
    return None


def wait_for_verification(uid_start: int, timeout_seconds: int) -> dict[str, object]:
    expected_from = os.environ.get(
        "EMAIL_E2E_EXPECTED_FROM", "no-reply@realtekconnect.com"
    ).strip()
    # A live staging run signs up with a unique plus-address while polling the
    # shared test mailbox.  Local E2E keeps the historical behaviour and uses
    # the IMAP login address when no explicit recipient is supplied.
    expected_to = os.environ.get("EMAIL_E2E_SIGNUP_EMAIL", "").strip()
    if not expected_to:
        expected_to = _required_env("IMAP_EMAIL_ADDR")
    expected_base_url = _required_env("AUTH_TOKEN_BASE_URL").rstrip("/")
    deadline = time.monotonic() + timeout_seconds
    client = _connect()
    try:
        _select(client)
        while time.monotonic() < deadline:
            status, data = client.uid("search", None, f"UID {uid_start}:*")
            if status != "OK":
                raise IMAPTestError("IMAP UID search failed")
            uids = (data[0] if data else b"").split()
            for uid in uids:
                # IMAP sequence ranges are inclusive in either direction:
                # UIDNEXT:* can return the previous highest UID until mail arrives.
                if int(uid) < uid_start:
                    continue
                status, fetched = client.uid("fetch", uid, "(BODY.PEEK[])")
                if status != "OK":
                    continue
                raw = _raw_from_fetch(fetched)
                if raw is None:
                    continue
                result = inspect_message(
                    raw, expected_from, expected_to, expected_base_url
                )
                if result is not None:
                    result["uid"] = int(uid)
                    return result
            time.sleep(5)
        raise IMAPTestError("verification email did not arrive before timeout")
    finally:
        try:
            client.logout()
        except imaplib.IMAP4.error:
            pass


def main() -> int:
    parser = argparse.ArgumentParser()
    subparsers = parser.add_subparsers(dest="command", required=True)
    subparsers.add_parser("snapshot")
    wait_parser = subparsers.add_parser("wait")
    wait_parser.add_argument("--uid-start", type=int, required=True)
    wait_parser.add_argument("--timeout", type=int, default=180)
    args = parser.parse_args()
    try:
        if args.command == "snapshot":
            result: dict[str, object] = {"uid_next": snapshot_uidnext()}
        else:
            result = wait_for_verification(args.uid_start, args.timeout)
        json.dump(result, sys.stdout, separators=(",", ":"))
        sys.stdout.write("\n")
        return 0
    except IMAPTestError as exc:
        print(f"IMAP live E2E failed: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
