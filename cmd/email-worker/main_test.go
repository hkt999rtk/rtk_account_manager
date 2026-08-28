package main

import (
	"testing"
	"time"

	"rtk_account_manager/internal/config"
	"rtk_account_manager/internal/emaildelivery"
)

func TestEmailDeliveryUsesSendMailHTTP(t *testing.T) {
	sender, renderer, err := emailDelivery(config.Config{
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
	if renderer.BaseURL != "https://account.example.com" {
		t.Fatalf("renderer = %+v", renderer)
	}
}
