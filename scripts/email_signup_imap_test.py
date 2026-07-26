#!/usr/bin/env python3

import importlib.util
import pathlib
import unittest
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


if __name__ == "__main__":
    unittest.main()
