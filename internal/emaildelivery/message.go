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
	case "password_reset":
		subject, intro = "Reset your Realtek Connect password", "Reset your Realtek Connect password"
	case "brand_cloud_owner_transfer":
		subject, intro = "Accept Realtek Connect+ brand cloud ownership", "Accept the Realtek Connect+ brand cloud ownership transfer"
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
	link := r.authLink(messageType, payload.Token)
	text := intro + ":\r\n\r\n" + link + "\r\n"
	if payload.Token != "" {
		text += "\r\nToken: " + payload.Token + "\r\n"
	}
	if payload.ExpiresAt != "" {
		text += "Expires: " + payload.ExpiresAt + "\r\n"
	}
	text += "\r\nIf you did not request this email, you can ignore it.\r\n"
	htmlBody := "<html><body><p>" + html.EscapeString(intro) + ":</p><p><a href=\"" +
		html.EscapeString(link) + "\">Continue to Realtek Connect</a></p>"
	if payload.ExpiresAt != "" {
		htmlBody += "<p>Expires: " + html.EscapeString(payload.ExpiresAt) + "</p>"
	}
	htmlBody += "<p>If you did not request this email, you can ignore it.</p></body></html>"
	return subject, text, htmlBody, nil
}

func (r Renderer) authLink(messageType, token string) string {
	base := strings.TrimRight(strings.TrimSpace(r.BaseURL), "/")
	path := "/login/activate"
	switch messageType {
	case "email_verification":
		path = "/signup/verify"
	case "password_reset":
		path = "/reset-password"
	case "brand_cloud_owner_transfer":
		path = "/brand-cloud-owner-transfer/accept"
	}
	u, err := url.Parse(base + path)
	if err != nil {
		return ""
	}
	q := u.Query()
	q.Set("token", token)
	u.RawQuery = q.Encode()
	return u.String()
}
