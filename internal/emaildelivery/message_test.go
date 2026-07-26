package emaildelivery

import (
	"io"
	"mime/quotedprintable"
	"strings"
	"testing"
	"time"
)

func TestRendererBuildsAllTemplates(t *testing.T) {
	now := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	renderer := Renderer{
		From: "no-reply@realtekconnect.com", FromName: "Realtek Connect",
		BaseURL: "https://account.realtekconnect.com", Now: func() time.Time { return now },
	}
	approved := 25
	reason := "Approved for launch"
	tests := []struct {
		messageType string
		payload     Payload
		want        string
	}{
		{"email_verification", Payload{RecipientEmail: "user@example.com", Token: "token", ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)}, "/signup/verify"},
		{"login_activation", Payload{RecipientEmail: "user@example.com", Token: "token"}, "/login/activate"},
		{"password_reset", Payload{RecipientEmail: "user@example.com", Token: "token"}, "/reset-password"},
		{"brand_cloud_owner_transfer", Payload{RecipientEmail: "user@example.com", Token: "token"}, "/brand-cloud-owner-transfer/accept"},
		{"quota_approved", Payload{RecipientEmail: "user@example.com", OrganizationName: "Acme", OrganizationID: "org-1", RequestedQuota: 20, ApprovedQuota: &approved, DecisionReason: &reason}, "Approved quota: 25"},
		{"quota_declined", Payload{RecipientEmail: "user@example.com", OrganizationName: "Acme", OrganizationID: "org-1", RequestedQuota: 20, DecisionReason: &reason}, "Quota raise decision: declined"},
	}
	for _, test := range tests {
		t.Run(test.messageType, func(t *testing.T) {
			message, err := renderer.Render("outbox-1", test.messageType, test.payload)
			if err != nil {
				t.Fatal(err)
			}
			body := string(message.Data)
			decodedBytes, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(body)))
			if err != nil {
				t.Fatal(err)
			}
			decoded := string(decodedBytes)
			if message.Subject == "" || message.Text == "" || message.HTML == "" {
				t.Fatalf("structured message fields are incomplete: %+v", message)
			}
			for _, want := range []string{
				"Message-ID: <outbox-1@realtekconnect.com>",
				`From: "Realtek Connect" <no-reply@realtekconnect.com>`,
				"multipart/alternative",
				test.want,
			} {
				if !strings.Contains(decoded, want) {
					t.Fatalf("message does not contain %q:\n%s", want, decoded)
				}
			}
		})
	}
}

func TestRendererBuildsStructuredMessageWithoutSMTPFrom(t *testing.T) {
	message, err := (Renderer{BaseURL: "https://account.example.com"}).Render(
		"outbox-1",
		"email_verification",
		Payload{RecipientEmail: "user@example.com", Token: "token"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if message.Recipient != "user@example.com" ||
		!strings.Contains(message.Subject, "Verify") ||
		!strings.Contains(message.Text, "/signup/verify?token=token") ||
		!strings.Contains(message.HTML, "/signup/verify?token=token") {
		t.Fatalf("message = %+v", message)
	}
	if message.EnvelopeFrom != "" || len(message.Data) != 0 {
		t.Fatalf("sendmail_http message unexpectedly contains SMTP envelope: %+v", message)
	}
}

func TestRendererRejectsHeaderInjection(t *testing.T) {
	renderer := Renderer{From: "no-reply@realtekconnect.com", BaseURL: "https://example.com"}
	if _, err := renderer.Render("id\r\nBcc: victim@example.com", "email_verification", Payload{
		RecipientEmail: "user@example.com", Token: "token",
	}); err == nil {
		t.Fatal("header injection unexpectedly accepted")
	}
	if _, err := (Renderer{From: "no-reply@realtekconnect.com", FromName: "Sender\r\nBcc: victim@example.com"}).Render(
		"id", "email_verification", Payload{RecipientEmail: "user@example.com", Token: "token"},
	); err == nil {
		t.Fatal("from-name injection unexpectedly accepted")
	}
}
