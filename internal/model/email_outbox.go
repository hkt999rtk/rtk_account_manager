package model

import "time"

type EmailOutboxStatus string

const (
	EmailOutboxStatusPending      EmailOutboxStatus = "pending"
	EmailOutboxStatusSending      EmailOutboxStatus = "sending"
	EmailOutboxStatusRetrying     EmailOutboxStatus = "retrying"
	EmailOutboxStatusSent         EmailOutboxStatus = "sent"
	EmailOutboxStatusDeadLettered EmailOutboxStatus = "dead_lettered"
	EmailOutboxStatusExpired      EmailOutboxStatus = "expired"
)

type EmailOutbox struct {
	ID                string
	IdempotencyKey    string
	MessageType       string
	TemplateVersion   int
	PayloadNonce      []byte
	PayloadCiphertext []byte
	Status            EmailOutboxStatus
	AttemptCount      int
	AvailableAt       time.Time
	LeaseUntil        *time.Time
	LastError         *string
	ExpiresAt         *time.Time
	SentAt            *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}
