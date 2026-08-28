package emaildelivery

import (
	"bytes"
	"errors"
	"fmt"
	"html"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/url"
	"strings"
	"time"
)

type Message struct {
	EnvelopeFrom string
	Recipient    string
	ReplyTo      string
	Subject      string
	Text         string
	HTML         string
	Data         []byte
}

type Renderer struct {
	From     string
	FromName string
	BaseURL  string
	Now      func() time.Time
}

func (r Renderer) Render(outboxID, messageType string, payload Payload) (Message, error) {
	to, err := mail.ParseAddress(strings.TrimSpace(payload.RecipientEmail))
	if err != nil {
		return Message{}, fmt.Errorf("invalid recipient: %w", err)
	}
	if hasHeaderBreak(outboxID) || hasHeaderBreak(messageType) {
		return Message{}, errors.New("invalid email header value")
	}
	subject, textBody, htmlBody, err := r.content(messageType, payload)
	if err != nil {
		return Message{}, err
	}
	message := Message{
		Recipient: to.Address,
		Subject:   subject,
		Text:      textBody,
		HTML:      htmlBody,
	}
	if strings.TrimSpace(r.From) == "" {
		return message, nil
	}
	from, err := mailbox(r.FromName, r.From)
	if err != nil {
		return Message{}, fmt.Errorf("invalid SMTP_FROM: %w", err)
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	domain := "realtekconnect.com"
	if parsed, parseErr := mail.ParseAddress(r.From); parseErr == nil {
		if at := strings.LastIndex(parsed.Address, "@"); at >= 0 {
			domain = parsed.Address[at+1:]
		}
	}
	messageID := fmt.Sprintf("<%s@%s>", strings.TrimSpace(outboxID), domain)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	textHeader := make(map[string][]string)
	textHeader["Content-Type"] = []string{`text/plain; charset="UTF-8"`}
	textHeader["Content-Transfer-Encoding"] = []string{"quoted-printable"}
	part, err := writer.CreatePart(textHeader)
	if err != nil {
		return Message{}, err
	}
	qp := quotedprintable.NewWriter(part)
	if _, err := qp.Write([]byte(textBody)); err != nil {
		return Message{}, err
	}
	if err := qp.Close(); err != nil {
		return Message{}, err
	}
	htmlHeader := make(map[string][]string)
	htmlHeader["Content-Type"] = []string{`text/html; charset="UTF-8"`}
	htmlHeader["Content-Transfer-Encoding"] = []string{"quoted-printable"}
	part, err = writer.CreatePart(htmlHeader)
	if err != nil {
		return Message{}, err
	}
	qp = quotedprintable.NewWriter(part)
	if _, err := qp.Write([]byte(htmlBody)); err != nil {
		return Message{}, err
	}
	if err := qp.Close(); err != nil {
		return Message{}, err
	}
	if err := writer.Close(); err != nil {
		return Message{}, err
	}

	var raw bytes.Buffer
	fmt.Fprintf(&raw, "Date: %s\r\n", now.Format(time.RFC1123Z))
	fmt.Fprintf(&raw, "Message-ID: %s\r\n", messageID)
	fmt.Fprintf(&raw, "From: %s\r\n", from.String())
	fmt.Fprintf(&raw, "To: %s\r\n", to.String())
	fmt.Fprintf(&raw, "Subject: %s\r\n", mime.QEncoding.Encode("UTF-8", subject))
	fmt.Fprint(&raw, "MIME-Version: 1.0\r\n")
	fmt.Fprintf(&raw, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", writer.Boundary())
	raw.Write(body.Bytes())
	message.EnvelopeFrom = from.Address
	message.Data = raw.Bytes()
	return message, nil
}

func mailbox(name, address string) (*mail.Address, error) {
	if hasHeaderBreak(name) || hasHeaderBreak(address) {
		return nil, errors.New("header line break is not allowed")
	}
	parsed, err := mail.ParseAddress(strings.TrimSpace(address))
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(name) != "" {
		parsed.Name = strings.TrimSpace(name)
	}
	return parsed, nil
}

func hasHeaderBreak(value string) bool {
	return strings.ContainsAny(value, "\r\n")
}

func (r Renderer) content(messageType string, payload Payload) (string, string, string, error) {
	var subject, intro string
	switch messageType {
	case "email_verification":
		subject, intro = "Verify your Realtek Connect account", "Verify your Realtek Connect account"
	case "login_activation":
		subject, intro = "Sign in to Realtek Connect", "Sign in to Realtek Connect"
	case "brand_cloud_user_activation":
		subject, intro = "Activate your Realtek Connect brand account", "Set a password to activate your Realtek Connect brand account"
	case "password_reset":
		subject, intro = "Reset your Realtek Connect password", "Reset your Realtek Connect password"
	case "brand_cloud_owner_transfer":
		subject, intro = "Accept Realtek Connect+ brand cloud ownership", "Accept the Realtek Connect+ brand cloud ownership transfer"
	case "brand_cloud_membership_invitation":
		subject, intro = "Join a Realtek Connect+ Brand Cloud", "Accept your Realtek Connect+ Brand Cloud membership invitation"
	case "quota_approved", "quota_declined":
		decision := strings.TrimPrefix(messageType, "quota_")
		subject = "Quota raise " + decision
		text := fmt.Sprintf("Quota raise decision: %s\r\nOrganization: %s (%s)\r\nRequested quota: %d\r\n",
			decision, payload.OrganizationName, payload.OrganizationID, payload.RequestedQuota)
		if payload.ApprovedQuota != nil {
			text += fmt.Sprintf("Approved quota: %d\r\n", *payload.ApprovedQuota)
		}
		if payload.DecisionReason != nil && strings.TrimSpace(*payload.DecisionReason) != "" {
			text += "Decision reason: " + strings.TrimSpace(*payload.DecisionReason) + "\r\n"
		}
		return subject, text, "<html><body><pre>" + html.EscapeString(text) + "</pre></body></html>", nil
	default:
		return "", "", "", fmt.Errorf("unsupported email message type %q", messageType)
	}
	link := r.authLink(messageType, payload.Token, payload.RecipientEmail, payload.TenantSlug)
	text := intro + ":\r\n\r\n" + link + "\r\n"
	if payload.Token != "" {
		text += "\r\nToken: " + payload.Token + "\r\n"
	}
	if payload.ExpiresAt != "" {
		text += "Expires: " + payload.ExpiresAt + "\r\n"
	}
	text += "\r\nIf you did not request this email, you can ignore it.\r\n"
	if messageType == "email_verification" {
		text = "Welcome to Realtek Connect. Verify your email address to finish creating your account:\r\n\r\n" + link + "\r\n"
		if payload.Token != "" {
			text += "\r\nToken: " + payload.Token + "\r\n"
		}
		if payload.ExpiresAt != "" {
			text += "Expires: " + payload.ExpiresAt + "\r\n"
		}
		text += "\r\nIf you did not create a Realtek Connect account, you can safely ignore this email.\r\n"
		return subject, text, authEmailHTML(authEmailContent{
			Preheader: "Verify your email to finish setting up your Realtek Connect account.",
			Eyebrow:   "ACCOUNT SETUP",
			Title:     "Confirm your email address",
			Body:      "You're one step away from Realtek Connect. Confirm this email address to activate your account and continue to your workspace.",
			CTA:       "Verify email address",
			Link:      link,
			ExpiresAt: payload.ExpiresAt,
			Notice:    "Didn't create this account? No action is needed. You can safely ignore this email.",
		}), nil
	}
	if messageType == "password_reset" {
		text = "We received a request to reset your Realtek Connect password. Choose a new password using this secure link:\r\n\r\n" + link + "\r\n"
		if payload.Token != "" {
			text += "\r\nToken: " + payload.Token + "\r\n"
		}
		if payload.ExpiresAt != "" {
			text += "Expires: " + payload.ExpiresAt + "\r\n"
		}
		text += "\r\nIf you did not request a password reset, ignore this email. Your password will remain unchanged.\r\n"
		return subject, text, authEmailHTML(authEmailContent{
			Preheader: "Use this secure link to choose a new Realtek Connect password.",
			Eyebrow:   "SECURITY REQUEST",
			Title:     "Reset your password",
			Body:      "We received a request to reset your Realtek Connect password. Use the secure link below to choose a new one.",
			CTA:       "Choose a new password",
			Link:      link,
			ExpiresAt: payload.ExpiresAt,
			Notice:    "Didn't request this? You can ignore this email. Your password will remain unchanged.",
		}), nil
	}
	htmlBody := "<html><body><p>" + html.EscapeString(intro) + ":</p><p><a href=\"" +
		html.EscapeString(link) + "\">Continue to Realtek Connect</a></p>"
	if payload.ExpiresAt != "" {
		htmlBody += "<p>Expires: " + html.EscapeString(payload.ExpiresAt) + "</p>"
	}
	htmlBody += "<p>If you did not request this email, you can ignore it.</p></body></html>"
	return subject, text, htmlBody, nil
}

type authEmailContent struct {
	Preheader string
	Eyebrow   string
	Title     string
	Body      string
	CTA       string
	Link      string
	ExpiresAt string
	Notice    string
}

func authEmailHTML(content authEmailContent) string {
	escape := html.EscapeString
	var b strings.Builder
	b.WriteString(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="x-apple-disable-message-reformatting">
  <meta name="color-scheme" content="light only">
  <title>`)
	b.WriteString(escape(content.Title))
	b.WriteString(`</title>
  <style>
    @media only screen and (max-width: 620px) {
      .email-shell { width: 100% !important; }
      .mobile-pad { padding-left: 24px !important; padding-right: 24px !important; }
      .mobile-title { font-size: 30px !important; line-height: 36px !important; }
      .mobile-button { display: block !important; text-align: center !important; }
    }
  </style>
</head>
<body style="margin:0; padding:0; width:100%; background-color:#eef5f9; color:#183247; font-family:Arial, Helvetica, sans-serif; -webkit-text-size-adjust:100%; -ms-text-size-adjust:100%;">
  <div style="display:none; max-height:0; overflow:hidden; opacity:0; color:transparent; mso-hide:all;">`)
	b.WriteString(escape(content.Preheader))
	b.WriteString(`&#847;&zwnj;&nbsp;&#847;&zwnj;&nbsp;&#847;&zwnj;&nbsp;</div>
  <table role="presentation" width="100%" cellspacing="0" cellpadding="0" border="0" style="width:100%; border-collapse:collapse; background-color:#eef5f9;">
    <tr>
      <td align="center" style="padding:32px 12px;">
        <table role="presentation" class="email-shell" width="600" cellspacing="0" cellpadding="0" border="0" style="width:600px; max-width:600px; border-collapse:separate; background-color:#ffffff; border:1px solid #dbe5ec; border-radius:16px; overflow:hidden;">
          <tr>
            <td class="mobile-pad" style="padding:24px 48px; background-color:#ffffff; border-bottom:1px solid #e5edf2;">
              <table role="presentation" width="100%" cellspacing="0" cellpadding="0" border="0" style="width:100%; border-collapse:collapse;">
                <tr>
                  <td valign="middle" style="font-size:0;">
                    <table role="presentation" cellspacing="0" cellpadding="0" border="0" style="border-collapse:collapse;">
                      <tr>
                        <td align="center" valign="middle" width="36" height="36" style="width:36px; height:36px; background-color:#0068b7; border-radius:9px; color:#ffffff; font-size:16px; line-height:36px; font-weight:700;">R</td>
                        <td style="padding-left:12px; color:#183247; font-size:17px; line-height:22px; font-weight:700; letter-spacing:-0.2px;">Realtek Connect</td>
                      </tr>
                    </table>
                  </td>
                  <td align="right" valign="middle" style="color:#0068b7; font-size:11px; line-height:16px; font-weight:700; letter-spacing:1.4px;">SECURE ACCOUNT</td>
                </tr>
              </table>
            </td>
          </tr>
          <tr>
            <td class="mobile-pad" style="padding:48px 48px 20px 48px;">
              <div style="color:#0068b7; font-size:12px; line-height:18px; font-weight:700; letter-spacing:1.7px;">`)
	b.WriteString(escape(content.Eyebrow))
	b.WriteString(`</div>
              <h1 class="mobile-title" style="margin:12px 0 18px 0; color:#183247; font-size:38px; line-height:44px; font-weight:700; letter-spacing:-1px;">`)
	b.WriteString(escape(content.Title))
	b.WriteString(`</h1>
              <p style="margin:0; color:#52677a; font-size:16px; line-height:26px;">`)
	b.WriteString(escape(content.Body))
	b.WriteString(`</p>
            </td>
          </tr>
          <tr>
            <td class="mobile-pad" style="padding:12px 48px 32px 48px;">
              <table role="presentation" cellspacing="0" cellpadding="0" border="0" style="border-collapse:separate;">
                <tr>
                  <td align="center" bgcolor="#0068b7" style="background-color:#0068b7; border-radius:8px;">
                    <a class="mobile-button" href="`)
	b.WriteString(escape(content.Link))
	b.WriteString(`" style="display:inline-block; padding:15px 24px; color:#ffffff; font-size:15px; line-height:20px; font-weight:700; text-decoration:none; border:1px solid #0068b7; border-radius:8px;">`)
	b.WriteString(escape(content.CTA))
	b.WriteString(` &nbsp;&rarr;</a>
                  </td>
                </tr>
              </table>`)
	if strings.TrimSpace(content.ExpiresAt) != "" {
		b.WriteString(`
              <p style="margin:16px 0 0 0; color:#6f8292; font-size:13px; line-height:20px;">For your security, this link expires at <strong style="color:#3c566a;">`)
		b.WriteString(escape(content.ExpiresAt))
		b.WriteString(`</strong>.</p>`)
	}
	b.WriteString(`
            </td>
          </tr>
          <tr>
            <td class="mobile-pad" style="padding:0 48px 40px 48px;">
              <table role="presentation" width="100%" cellspacing="0" cellpadding="0" border="0" style="width:100%; border-collapse:separate; background-color:#f4f9fb; border:1px solid #dbeaf1; border-radius:10px;">
                <tr>
                  <td style="padding:18px 20px; color:#52677a; font-size:13px; line-height:20px;">
                    <strong style="display:block; margin-bottom:4px; color:#183247; font-size:13px;">Security note</strong>`)
	b.WriteString(escape(content.Notice))
	b.WriteString(`
                  </td>
                </tr>
              </table>
            </td>
          </tr>
          <tr>
            <td class="mobile-pad" style="padding:24px 48px 30px 48px; background-color:#183247;">
              <p style="margin:0 0 6px 0; color:#ffffff; font-size:13px; line-height:19px; font-weight:700;">Realtek Connect</p>
              <p style="margin:0; color:#9fb5c4; font-size:11px; line-height:17px;">This is an automated account email. Please do not reply.</p>
            </td>
          </tr>
        </table>
      </td>
    </tr>
  </table>
</body>
</html>`)
	return b.String()
}

func (r Renderer) authLink(messageType, token, recipientEmail, tenantSlug string) string {
	base := strings.TrimRight(strings.TrimSpace(r.BaseURL), "/")
	path := "/login/activate"
	switch messageType {
	case "email_verification":
		path = "/signup/verify"
	case "password_reset":
		path = "/reset-password"
	case "brand_cloud_owner_transfer":
		path = "/brand-cloud-owner-transfer/accept"
	case "brand_cloud_membership_invitation":
		path = "/brand-cloud-member-invitation/accept"
	case "brand_cloud_user_activation":
		path = "/brand-cloud/activate"
	}
	u, err := url.Parse(base + path)
	if err != nil {
		return ""
	}
	q := u.Query()
	q.Set("token", token)
	if messageType == "password_reset" && strings.TrimSpace(recipientEmail) != "" {
		q.Set("email", strings.TrimSpace(recipientEmail))
	}
	if messageType == "brand_cloud_user_activation" {
		q.Set("tenant", strings.TrimSpace(tenantSlug))
	}
	u.RawQuery = q.Encode()
	return u.String()
}
