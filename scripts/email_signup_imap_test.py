#!/usr/bin/env python3

import importlib.util
import pathlib
import unittest
from unittest import mock
from email.message import EmailMessage


MODULE_PATH = pathlib.Path(__file__).with_name("email_signup_imap.py")
SPEC = importlib.util.spec_from_file_location("email_signup_imap", MODULE_PATH)
assert SPEC and SPEC.loader
imap_helper = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(imap_helper)


def message_bytes(
    *,
    sender="Realtek Connect <no-reply@realtekconnect.com>",
    recipient="test@example.com",
    subject=imap_helper.EXPECTED_SUBJECT,
    text_url="http://127.0.0.1:18082/signup/verify?token=secret-token",
    html_url=None,
):
    message = EmailMessage()
    message["From"] = sender
    message["To"] = recipient
    message["Subject"] = subject
    message["Message-ID"] = "<outbox-id@realtekconnect.com>"
    message.set_content(f"Open {text_url}")
    message.add_alternative(
        f'<html><body><a href="{html_url or text_url}">Verify</a></body></html>',
        subtype="html",
    )
    return message.as_bytes()


class MessageInspectionTest(unittest.TestCase):
    def inspect(self, raw):
        return imap_helper.inspect_message(
            raw,
            "no-reply@realtekconnect.com",
            "test@example.com",
            "http://127.0.0.1:18082",
        )

    def test_extracts_one_url_from_text_and_html(self):
        result = self.inspect(message_bytes())
        self.assertIsNotNone(result)
        self.assertEqual(
            result["url"],
            "http://127.0.0.1:18082/signup/verify?token=secret-token",
        )
        self.assertTrue(result["message_id_present"])
        self.assertTrue(result["multipart_alternative"])

    def test_decodes_unicode_and_quoted_printable(self):
        raw = message_bytes(recipient="測試 <test@example.com>")
        self.assertIsNotNone(self.inspect(raw))

    def test_accepts_a_plus_address_recipient(self):
        result = imap_helper.inspect_message(
            message_bytes(recipient="imap-test01+run-123@example.com"),
            "no-reply@realtekconnect.com",
            "imap-test01+run-123@example.com",
            "http://127.0.0.1:18082",
        )
        self.assertIsNotNone(result)

    def test_accepts_brand_owner_activation_subject_and_path(self):
        with mock.patch.dict(
            "os.environ",
            {
                "EMAIL_E2E_EXPECTED_SUBJECT": "Activate your Realtek Connect brand account",
                "EMAIL_E2E_EXPECTED_PATH": "/brand-cloud/activate",
            },
        ):
            result = imap_helper.inspect_message(
                message_bytes(
                    recipient="imap-test01+load-run-b01@example.com",
                    subject="Activate your Realtek Connect brand account",
                    text_url="https://admin.example.com/brand-cloud/activate?tenant=load-run-b01&token=secret-token",
                ),
                "no-reply@realtekconnect.com",
                "imap-test01+load-run-b01@example.com",
                "https://admin.example.com",
            )
        self.assertIsNotNone(result)

    def test_ignores_wrong_sender_recipient_or_subject(self):
        self.assertIsNone(self.inspect(message_bytes(sender="other@example.com")))
        self.assertIsNone(self.inspect(message_bytes(recipient="other@example.com")))
        self.assertIsNone(self.inspect(message_bytes(subject="Other subject")))

    def test_ignores_message_for_another_base_url(self):
        self.assertIsNone(
            self.inspect(message_bytes(text_url="https://example.com/wrong"))
        )

    def test_rejects_multiple_matching_urls(self):
        with self.assertRaisesRegex(
            imap_helper.IMAPTestError, "one matching URL"
        ):
            self.inspect(
                message_bytes(
                    html_url=(
                        "http://127.0.0.1:18082/signup/verify?token=other-token"
                    )
                )
            )

    def test_security_modes(self):
        self.assertEqual(imap_helper._security_mode("SSL/TLS"), "ssl")
        self.assertEqual(imap_helper._security_mode("STARTTLS"), "starttls")
        with self.assertRaisesRegex(
            imap_helper.IMAPTestError, "SSL/TLS or STARTTLS"
        ):
            imap_helper._security_mode("none")

    def test_connect_host_override_preserves_tls_server_name(self):
        context = mock.Mock()
        wrapped = mock.Mock()
        context.wrap_socket.return_value = wrapped
        raw = mock.Mock()
        client = imap_helper._IMAP4SSLWithConnectHost.__new__(
            imap_helper._IMAP4SSLWithConnectHost
        )
        client._connect_host = "192.0.2.10"
        client.host = "mail.example.com"
        client.port = 993
        client.ssl_context = context
        with mock.patch.object(
            imap_helper.socket, "create_connection", return_value=raw
        ) as connect:
            result = client._create_socket(15)
        connect.assert_called_once_with(("192.0.2.10", 993), timeout=15)
        context.wrap_socket.assert_called_once_with(
            raw, server_hostname="mail.example.com"
        )
        self.assertIs(result, wrapped)


class VerificationPollingTest(unittest.TestCase):
    def test_ignores_older_uid_until_new_verification_arrives(self):
        client = mock.Mock()
        client.uid.side_effect = [
            ("OK", [b"56"]),
            ("OK", [b"56 57"]),
            ("OK", [(b"57 (BODY[])", message_bytes())]),
        ]
        with mock.patch.dict("os.environ", {
            "EMAIL_E2E_SIGNUP_EMAIL": "test@example.com",
            "AUTH_TOKEN_BASE_URL": "http://127.0.0.1:18082",
        }), mock.patch.object(imap_helper, "_connect", return_value=client), \
                mock.patch.object(imap_helper, "_select"), \
                mock.patch.object(imap_helper.time, "monotonic", side_effect=[0, 1, 2]), \
                mock.patch.object(imap_helper.time, "sleep") as sleep:
            result = imap_helper.wait_for_verification(57, 30)

        self.assertEqual(result["uid"], 57)
        self.assertEqual(client.uid.call_args_list, [
            mock.call("search", None, "UID 57:*"),
            mock.call("search", None, "UID 57:*"),
            mock.call("fetch", b"57", "(BODY.PEEK[])"),
        ])
        sleep.assert_called_once_with(5)
        client.logout.assert_called_once()

    def test_old_mail_alone_times_out_without_fetching_it(self):
        client = mock.Mock()
        client.uid.return_value = ("OK", [b"56"])
        with mock.patch.dict("os.environ", {
            "EMAIL_E2E_SIGNUP_EMAIL": "test@example.com",
            "AUTH_TOKEN_BASE_URL": "http://127.0.0.1:18082",
        }), mock.patch.object(imap_helper, "_connect", return_value=client), \
                mock.patch.object(imap_helper, "_select"), \
                mock.patch.object(imap_helper.time, "monotonic", side_effect=[0, 1, 31]), \
                mock.patch.object(imap_helper.time, "sleep"):
            with self.assertRaisesRegex(imap_helper.IMAPTestError, "did not arrive"):
                imap_helper.wait_for_verification(57, 30)

        client.uid.assert_called_once_with("search", None, "UID 57:*")
        client.logout.assert_called_once()


if __name__ == "__main__":
    unittest.main()
