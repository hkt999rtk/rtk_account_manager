package mailer

import (
	"context"
	"log"
)

type MessageKind string

const (
	MessageKindEmailVerification MessageKind = "email_verification"
	MessageKindPasswordReset     MessageKind = "password_reset"
)

type Message struct {
	Kind      MessageKind
	Recipient string
	Code      string
}

type Mailer interface {
	Send(ctx context.Context, msg Message) error
}

type LogMailer struct {
	logger *log.Logger
}

func NewLogMailer(logger *log.Logger) *LogMailer {
	if logger == nil {
		logger = log.Default()
	}
	return &LogMailer{logger: logger}
}

func (m *LogMailer) Send(_ context.Context, msg Message) error {
	m.logger.Printf("mailer: sent kind=%s recipient=%s code=%s", msg.Kind, msg.Recipient, msg.Code)
	return nil
}

type RecordingMailer struct {
	Sent []Message
}

func (m *RecordingMailer) Send(_ context.Context, msg Message) error {
	m.Sent = append(m.Sent, msg)
	return nil
}
