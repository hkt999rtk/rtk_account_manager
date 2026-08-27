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
		{"brand_cloud_user_activation", Payload{RecipientEmail: "user@example.com", Token: "token", TenantSlug: "acme"}, "/brand-cloud/activate?tenant=acme"},
		{"password_reset", Payload{RecipientEmail: "user@example.com", Token: "token"}, "/reset-password"},
		{"brand_cloud_owner_transfer", Payload{RecipientEmail: "user@example.com", Token: "token"}, "/brand-cloud-owner-transfer/accept"},
		{"brand_cloud_membership_invitation", Payload{RecipientEmail: "user@example.com", Token: "token"}, "/brand-cloud-member-invitation/accept"},
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

func TestRendererBuildsDesignedAccountEmails(t *testing.T) {
	tests := []struct {
		name        string
		messageType string
		wantTitle   string
		wantCTA     string
		wantNotice  string
	}{
		{
			name:        "signup verification",
			messageType: "email_verification",
			wantTitle:   "Confirm your email address",
			wantCTA:     "Verify email address",
			wantNotice:  "create this account?",
		},
		{
			name:        "password reset",
			messageType: "password_reset",
			wantTitle:   "Reset your password",
			wantCTA:     "Choose a new password",
			wantNotice:  "Your password will remain unchanged.",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			message, err := (Renderer{BaseURL: "https://account.example.com"}).Render(
				"outbox-1",
				test.messageType,
				Payload{
					RecipientEmail: "user@example.com",
					Token:          "token with space",
					ExpiresAt:      "2026-07-25T11:00:00Z",
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{
				`<!doctype html>`,
				`<table role="presentation"`,
				`style="`,
				`@media only screen and (max-width: 620px)`,
				`Realtek Connect`,
				test.wantTitle,
				test.wantCTA,
				test.wantNotice,
				`This is an automated account email. Please do not reply.`,
				`2026-07-25T11:00:00Z`,
			} {
				if !strings.Contains(message.HTML, want) {
					t.Errorf("HTML does not contain %q:\n%s", want, message.HTML)
				}
			}
			if strings.Contains(message.HTML, "<img") || strings.Contains(message.HTML, "@font-face") {
				t.Errorf("HTML email unexpectedly depends on an external visual asset or font:\n%s", message.HTML)
			}
			if !strings.Contains(message.HTML, `token+with+space`) || !strings.Contains(message.Text, `token+with+space`) {
				t.Errorf("HTML and plain-text alternatives must both contain the action link: %+v", message)
			}
			if strings.Contains(message.HTML, `Button not working?`) || strings.Count(message.HTML, `token+with+space`) != 1 {
				t.Errorf("HTML email must keep the action URL only on the primary CTA: %s", message.HTML)
			}
			if !strings.Contains(message.Text, "Token: token with space") {
				t.Errorf("plain-text fallback does not contain the token: %q", message.Text)
			}
		})
	}
}

func TestAuthEmailHTMLEscapesContent(t *testing.T) {
	got := authEmailHTML(authEmailContent{
		Preheader: `<preview>`,
		Eyebrow:   `<account>`,
		Title:     `<title>`,
		Body:      `<script>alert(1)</script>`,
		CTA:       `<continue>`,
		Link:      `https://example.com/reset?token=a&next="bad"`,
		ExpiresAt: `<never>`,
		Notice:    `<notice>`,
	})
	for _, unsafe := range []string{"<script>", `<title></title>`, `next="bad"`} {
		if strings.Contains(got, unsafe) {
			t.Errorf("authEmailHTML did not escape %q:\n%s", unsafe, got)
		}
	}
	for _, escaped := range []string{"&lt;script&gt;", "token=a&amp;next=&#34;bad&#34;", "&lt;never&gt;"} {
		if !strings.Contains(got, escaped) {
			t.Errorf("authEmailHTML does not contain escaped value %q:\n%s", escaped, got)
		}
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
