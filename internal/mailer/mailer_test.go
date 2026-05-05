package mailer

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
)

func TestLogMailerWritesStructuredEntry(t *testing.T) {
	var buf bytes.Buffer
	m := NewLogMailer(log.New(&buf, "", 0))

	if err := m.Send(context.Background(), Message{
		Kind:      MessageKindEmailVerification,
		Recipient: "user@example.com",
		Code:      "123456",
	}); err != nil {
		t.Fatalf("send: %v", err)
	}

	got := buf.String()
	for _, want := range []string{"email_verification", "user@example.com", "123456"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected log to contain %q, got %q", want, got)
		}
	}
}

func TestRecordingMailerCapturesMessages(t *testing.T) {
	m := &RecordingMailer{}
	if err := m.Send(context.Background(), Message{Kind: MessageKindPasswordReset, Recipient: "a@b", Code: "999"}); err != nil {
		t.Fatal(err)
	}
	if len(m.Sent) != 1 || m.Sent[0].Code != "999" {
		t.Fatalf("expected one recorded message, got %+v", m.Sent)
	}
}
