package main

import (
	"testing"

	"rtk_account_manager/internal/config"
)

func TestSMTPConfigBuildsSubmissionAddress(t *testing.T) {
	addr, auth, err := smtpConfig(config.Config{
		SMTPHost:     "mail.example.test",
		SMTPPort:     "587",
		SMTPFrom:     "no-reply@example.test",
		SMTPUsername: "smtp-user",
		SMTPPassword: "smtp-pass",
	})
	if err != nil {
		t.Fatal(err)
	}
	if addr != "mail.example.test:587" {
		t.Fatalf("addr = %q, want mail.example.test:587", addr)
	}
	if auth == nil {
		t.Fatal("expected SMTP auth")
	}
}

func TestSMTPConfigRequiresHostAndFrom(t *testing.T) {
	if _, _, err := smtpConfig(config.Config{SMTPFrom: "no-reply@example.test"}); err == nil {
		t.Fatal("expected missing host error")
	}
	if _, _, err := smtpConfig(config.Config{SMTPHost: "mail.example.test"}); err == nil {
		t.Fatal("expected missing from error")
	}
}
