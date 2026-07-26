package main

import (
	"testing"
	"time"

	"rtk_account_manager/internal/config"
	"rtk_account_manager/internal/emaildelivery"
)

func TestEmailDeliverySelectsSendMailHTTP(t *testing.T) {
	sender, renderer, err := emailDelivery(config.Config{
		AuthTokenDelivery:       "sendmail_http",
		AuthTokenBaseURL:        "https://account.example.com",
		SendMailHTTPBaseURL:     "https://sm.example.com",
		SendMailHTTPBearerToken: "opaque-token",
		SendMailHTTPTimeout:     time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sender.(*emaildelivery.SendMailHTTPClient); !ok {
		t.Fatalf("sender = %T", sender)
	}
	if renderer.BaseURL != "https://account.example.com" || renderer.From != "" {
		t.Fatalf("renderer = %+v", renderer)
	}
}

func TestEmailDeliveryKeepsSMTPCompatibility(t *testing.T) {
	sender, renderer, err := emailDelivery(config.Config{
		AuthTokenDelivery: "smtp",
		AuthTokenBaseURL:  "https://account.example.com",
		SMTPHost:          "smtp.example.com",
		SMTPPort:          "587",
		SMTPFrom:          "no-reply@example.com",
		SMTPFromName:      "Example",
		SMTPEncryption:    "starttls",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sender.(*emaildelivery.SMTPClient); !ok {
		t.Fatalf("sender = %T", sender)
	}
	if renderer.From != "no-reply@example.com" || renderer.FromName != "Example" {
		t.Fatalf("renderer = %+v", renderer)
	}
}
