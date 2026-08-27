#!/usr/bin/env python3
"""Run the opt-in local SMTP + IMAP signup activation E2E."""

from __future__ import annotations

import argparse
import base64
import json
import os
import pathlib
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time
import uuid
from urllib.error import URLError
from urllib.request import urlopen


class E2EError(RuntimeError):
    pass


def parse_dotenv(path: pathlib.Path) -> dict[str, str]:
    values: dict[str, str] = {}
    if not path.exists():
        return values
    for raw_line in path.read_text(encoding="utf-8").splitlines():
        line = raw_line.strip()
        if not line or line.startswith("#"):
            continue
        if line.startswith("export "):
            line = line[7:].strip()
        if "=" not in line:
            continue
        key, value = line.split("=", 1)
        key, value = key.strip(), value.strip()
        if len(value) >= 2 and value[0] == value[-1] and value[0] in "\"'":
            value = value[1:-1]
        if key:
            values[key] = value
    return values


def required(values: dict[str, str], name: str) -> str:
    value = os.environ.get(name, values.get(name, "")).strip()
    if not value:
        raise E2EError(f"missing required test setting: {name}")
    return value


def free_port() -> int:
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        return int(listener.getsockname()[1])


def wait_http(url: str, process: subprocess.Popen[bytes], timeout: int = 90) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise E2EError("a local service exited during startup")
        try:
            with urlopen(url, timeout=2) as response:
                if response.status < 500:
                    return
        except (OSError, URLError):
            pass
        time.sleep(1)
    raise E2EError("a local service did not become ready before timeout")


def stop_process(process: subprocess.Popen[bytes]) -> None:
    if process.poll() is not None:
        return
    try:
        os.killpg(process.pid, signal.SIGTERM)
        process.wait(timeout=10)
    except (ProcessLookupError, subprocess.TimeoutExpired):
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass


def command_exists(name: str) -> None:
    if shutil.which(name) is None:
        raise E2EError(f"required local command is unavailable: {name}")


def run_checked(
    args: list[str],
    *,
    cwd: pathlib.Path,
    env: dict[str, str],
    stdout: object = subprocess.DEVNULL,
    timeout: int = 180,
) -> None:
    result = subprocess.run(
        args,
        cwd=cwd,
        env=env,
        stdin=subprocess.DEVNULL,
        stdout=stdout,
        stderr=subprocess.STDOUT,
        timeout=timeout,
        check=False,
    )
    if result.returncode != 0:
        raise E2EError(f"local command failed: {args[0]} {args[1]}")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "--admin-repo",
        default="../rtk_cloud_admin",
        help="path to the rtk_cloud_admin repository",
    )
    parser.add_argument(
        "--env-file",
        default=str(pathlib.Path.home() / ".env"),
        help="test credential dotenv file",
    )
    args = parser.parse_args()

    if os.environ.get("RUN_LIVE_EMAIL_E2E") != "1":
        print("SKIP: set RUN_LIVE_EMAIL_E2E=1 to run the local live email E2E.")
        return 0

    account_repo = pathlib.Path(__file__).resolve().parents[1]
    admin_repo = pathlib.Path(args.admin_repo).expanduser().resolve()
    admin_web = admin_repo / "web"
    if not (admin_repo / "go.mod").is_file() or not (
        admin_web / "package.json"
    ).is_file():
        raise E2EError("--admin-repo does not point to rtk_cloud_admin")

    for command in ("docker", "go", "node", "npm", "python3"):
        command_exists(command)
    if not (admin_web / "node_modules" / "playwright").is_dir():
        raise E2EError("Admin Console Playwright is missing; run npm ci in web/")

    dotenv = parse_dotenv(pathlib.Path(args.env_file).expanduser())
    required_names = (
        "SMTP_SERVER",
        "SMTP_PORT",
        "SMTP_EMAIL_ADDR",
        "SMTP_EMAIL_PASSWORD",
        "SMTP_ENCRYPTION",
        "IMAP_SERVER",
        "IMAP_EMAIL_ADDR",
        "IMAP_EMAIL_PASSWORD",
        "IMAP_EMAIL_PORT",
        "IMAP_EMAIL_SECURITY",
        "IMAP_EMAIL_FOLDER",
    )
    settings = {name: required(dotenv, name) for name in required_names}

    postgres_port, account_port, admin_port = free_port(), free_port(), free_port()
    account_url = f"http://127.0.0.1:{account_port}"
    admin_url = f"http://127.0.0.1:{admin_port}"
    container = f"rtk-email-e2e-{uuid.uuid4().hex[:12]}"
    processes: list[subprocess.Popen[bytes]] = []

    with tempfile.TemporaryDirectory(prefix="rtk-email-signup-e2e-") as raw_temp:
        temp_dir = pathlib.Path(raw_temp)
        log_files: list[object] = []
        try:
            subprocess.run(
                [
                    "docker",
                    "run",
                    "--rm",
                    "-d",
                    "--name",
                    container,
                    "-e",
                    "POSTGRES_DB=rtk_account_manager",
                    "-e",
                    "POSTGRES_USER=rtk",
                    "-e",
                    "POSTGRES_PASSWORD=rtk_password",
                    "-p",
                    f"127.0.0.1:{postgres_port}:5432",
                    "postgres:16-alpine",
                ],
                check=True,
                stdin=subprocess.DEVNULL,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )
            deadline = time.monotonic() + 60
            while time.monotonic() < deadline:
                ready = subprocess.run(
                    [
                        "docker",
                        "exec",
                        container,
                        "pg_isready",
                        "-U",
                        "rtk",
                        "-d",
                        "rtk_account_manager",
                    ],
                    check=False,
                    stdout=subprocess.DEVNULL,
                    stderr=subprocess.DEVNULL,
                )
                if ready.returncode == 0:
                    break
                time.sleep(1)
            else:
                raise E2EError("temporary PostgreSQL did not become ready")

            database_url = (
                f"postgres://rtk:rtk_password@127.0.0.1:{postgres_port}/"
                "rtk_account_manager?sslmode=disable"
            )
            service_env = {
                **os.environ,
                "GOWORK": "off",
                "DATABASE_URL": database_url,
                "JWT_ACCESS_SECRET": base64.urlsafe_b64encode(os.urandom(32)).decode(),
                "JWT_REFRESH_SECRET": base64.urlsafe_b64encode(os.urandom(32)).decode(),
                "ACCOUNT_MANAGER_ENV": "local",
                "ACCOUNT_MANAGER_LOG_LEVEL": "error",
                "AUTH_TOKEN_DELIVERY": "smtp",
                "AUTH_TOKEN_BASE_URL": admin_url,
                "SMTP_HOST": settings["SMTP_SERVER"],
                "SMTP_PORT": settings["SMTP_PORT"],
                "SMTP_USERNAME": settings["SMTP_EMAIL_ADDR"],
                "SMTP_PASSWORD": settings["SMTP_EMAIL_PASSWORD"],
                "SMTP_FROM": "no-reply@realtekconnect.com",
                "SMTP_FROM_NAME": "Realtek Connect",
                "SMTP_ENCRYPTION": settings["SMTP_ENCRYPTION"].lower(),
                "EMAIL_OUTBOX_ENCRYPTION_KEY": base64.b64encode(
                    os.urandom(32)
                ).decode(),
                "EMAIL_OUTBOX_POLL_INTERVAL": "1s",
                "EMAIL_OUTBOX_BATCH_SIZE": "5",
                "PORT": str(account_port),
            }
            service_env.update(
                {
                    key: value
                    for key, value in settings.items()
                    if key.startswith("IMAP_")
                }
            )

            print("Applying migrations to temporary local PostgreSQL...")
            run_checked(
                ["go", "run", "./cmd/migrate"],
                cwd=account_repo,
                env=service_env,
            )

            print("Building and starting local Account Manager and Admin Console...")
            run_checked(
                ["npm", "run", "build", "--silent"],
                cwd=admin_web,
                env=os.environ.copy(),
            )

            def start(
                command: list[str], cwd: pathlib.Path, env: dict[str, str], name: str
            ) -> subprocess.Popen[bytes]:
                log = open(temp_dir / f"{name}.log", "wb")
                log_files.append(log)
                process = subprocess.Popen(
                    command,
                    cwd=cwd,
                    env=env,
                    stdin=subprocess.DEVNULL,
                    stdout=log,
                    stderr=subprocess.STDOUT,
                    start_new_session=True,
                )
                processes.append(process)
                return process

            account_process = start(
                ["go", "run", "./cmd/server"],
                account_repo,
                service_env,
                "account-manager",
            )
            worker_process = start(
                ["go", "run", "./cmd/email-worker"],
                account_repo,
                service_env,
                "email-worker",
            )
            admin_env = {
                **os.environ,
                "GOWORK": "off",
                "PORT": str(admin_port),
                "DATABASE_PATH": str(temp_dir / "admin.db"),
                "ACCOUNT_MANAGER_BASE_URL": account_url,
                "CUSTOMER_PASSWORD_LOGIN_ENABLED": "true",
                "CLOUD_ADMIN_LOG_LEVEL": "error",
            }
            admin_process = start(
                ["go", "run", "./cmd/server"],
                admin_repo,
                admin_env,
                "admin-console",
            )
            wait_http(f"{account_url}/v1/health", account_process)
            wait_http(f"{admin_url}/healthz", admin_process)
            if worker_process.poll() is not None:
                raise E2EError("local email worker exited during startup")

            browser_env = {
                **service_env,
                "EMAIL_E2E_ADMIN_BASE_URL": admin_url,
                "EMAIL_E2E_ACCOUNT_MANAGER_BASE_URL": account_url,
                "EMAIL_E2E_SIGNUP_PASSWORD": base64.urlsafe_b64encode(
                    os.urandom(18)
                ).decode(),
                "EMAIL_E2E_IMAP_HELPER": str(
                    account_repo / "scripts" / "email_signup_imap.py"
                ),
                "EMAIL_E2E_EXPECTED_FROM": "no-reply@realtekconnect.com",
            }
            print("Running browser signup and waiting for the IMAP delivery...")
            run_checked(
                ["node", "scripts/email-signup-live-e2e.mjs"],
                cwd=admin_web,
                env=browser_env,
                stdout=None,
                timeout=300,
            )

            query = subprocess.run(
                [
                    "docker",
                    "exec",
                    container,
                    "psql",
                    "-U",
                    "rtk",
                    "-d",
                    "rtk_account_manager",
                    "-Atc",
                    (
                        "SELECT status || '|' || attempt_count || '|' || "
                        "(sent_at IS NOT NULL)::text || '|' || "
                        "(payload_nonce IS NULL)::text || '|' || "
                        "(payload_ciphertext IS NULL)::text "
                        "FROM email_outbox WHERE message_type='email_verification' "
                        "ORDER BY created_at DESC LIMIT 1"
                    ),
                ],
                check=True,
                capture_output=True,
                text=True,
                timeout=20,
            ).stdout.strip()
            if query != "sent|1|true|true|true":
                raise E2EError("email outbox did not reach the expected sent state")

            print("Local SMTP + IMAP account activation E2E completed successfully.")
            return 0
        finally:
            for process in reversed(processes):
                stop_process(process)
            for log in log_files:
                log.close()
            subprocess.run(
                ["docker", "stop", container],
                check=False,
                stdin=subprocess.DEVNULL,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                timeout=20,
            )


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (E2EError, subprocess.TimeoutExpired, subprocess.CalledProcessError) as exc:
        print(f"Local email E2E failed: {exc}", file=sys.stderr)
        raise SystemExit(1)
