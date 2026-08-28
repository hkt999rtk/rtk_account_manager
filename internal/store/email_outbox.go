package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"rtk_account_manager/internal/emaildelivery"
	"rtk_account_manager/internal/model"
)

type EmailOutboxInput struct {
	IdempotencyKey  string
	MessageType     string
	TemplateVersion int
	Payload         emaildelivery.Payload
	ExpiresAt       *time.Time
}

type EmailOutboxTransitionInput struct {
	ID           string
	FromAttempt  int
	Status       model.EmailOutboxStatus
	AttemptCount int
	AvailableAt  time.Time
	LastError    *string
	SentAt       *time.Time
	ClearPayload bool
}

type EmailOutboxCounts struct {
	Pending          int64
	Retrying         int64
	Sent             int64
	DeadLettered     int64
	Expired          int64
	OldestPendingAge time.Duration
	DeliveryLatency  time.Duration
}

var ErrEmailOutboxEncryptionUnavailable = errors.New("email outbox encryption is not configured")

func (s *Store) enqueueEmailTx(ctx context.Context, tx pgx.Tx, in EmailOutboxInput) error {
	if s.emailOutboxCipher == nil {
		return ErrEmailOutboxEncryptionUnavailable
	}
	nonce, ciphertext, err := s.emailOutboxCipher.Encrypt(in.Payload)
	if err != nil {
		return err
	}
	if in.TemplateVersion <= 0 {
		in.TemplateVersion = 1
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO email_outbox (
			idempotency_key, message_type, template_version,
			payload_nonce, payload_ciphertext, expires_at
		)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (idempotency_key) DO NOTHING
	`, in.IdempotencyKey, in.MessageType, in.TemplateVersion, nonce, ciphertext, in.ExpiresAt)
	return err
}

func (s *Store) EnqueueEmail(ctx context.Context, in EmailOutboxInput) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := s.enqueueEmailTx(ctx, tx, in); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) ClaimEmailOutboxReady(ctx context.Context, now, leaseUntil time.Time, limit int) ([]model.EmailOutbox, error) {
	rows, err := s.db.Query(ctx, `
		WITH candidates AS (
			SELECT id
			FROM email_outbox
			WHERE (
				status IN ('pending', 'retrying') AND available_at <= $1
			) OR (
				status = 'sending' AND lease_until <= $1
			)
			ORDER BY available_at ASC, created_at ASC
			FOR UPDATE SKIP LOCKED
			LIMIT $2
		), claimed AS (
			UPDATE email_outbox AS outbox
			SET status = 'sending', lease_until = $3
			FROM candidates
			WHERE outbox.id = candidates.id
			RETURNING outbox.id::text, outbox.idempotency_key, outbox.message_type,
				outbox.template_version, outbox.payload_nonce, outbox.payload_ciphertext,
				outbox.status, outbox.attempt_count, outbox.available_at, outbox.lease_until,
				outbox.last_error, outbox.expires_at, outbox.sent_at,
				outbox.created_at, outbox.updated_at
		)
		SELECT * FROM claimed ORDER BY created_at ASC
	`, now, limit, leaseUntil)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var messages []model.EmailOutbox
	for rows.Next() {
		message, err := scanEmailOutbox(rows)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

func (s *Store) TransitionEmailOutbox(ctx context.Context, in EmailOutboxTransitionInput) (bool, error) {
	tag, err := s.db.Exec(ctx, `
		UPDATE email_outbox
		SET status = $3,
		    attempt_count = $4,
		    available_at = $5,
		    lease_until = NULL,
		    last_error = $6,
		    sent_at = $7,
		    payload_nonce = CASE WHEN $8 THEN NULL ELSE payload_nonce END,
		    payload_ciphertext = CASE WHEN $8 THEN NULL ELSE payload_ciphertext END
		WHERE id = $1
		  AND status = 'sending'
		  AND attempt_count = $2
	`, in.ID, in.FromAttempt, in.Status, in.AttemptCount, in.AvailableAt, in.LastError, in.SentAt, in.ClearPayload)
	return tag.RowsAffected() == 1, err
}

func (s *Store) ListEmailOutbox(ctx context.Context, status model.EmailOutboxStatus, limit int) ([]model.EmailOutbox, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Query(ctx, `
		SELECT id::text, idempotency_key, message_type, template_version,
			payload_nonce, payload_ciphertext, status, attempt_count,
			available_at, lease_until, last_error, expires_at, sent_at,
			created_at, updated_at
		FROM email_outbox
		WHERE ($1 = '' OR status = $1)
		ORDER BY created_at DESC
		LIMIT $2
	`, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var messages []model.EmailOutbox
	for rows.Next() {
		message, err := scanEmailOutbox(rows)
		if err != nil {
			return nil, err
		}
		message.PayloadNonce = nil
		message.PayloadCiphertext = nil
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

func (s *Store) RequeueEmailOutbox(ctx context.Context, id string, now time.Time) (bool, error) {
	tag, err := s.db.Exec(ctx, `
		UPDATE email_outbox
		SET status = 'retrying', attempt_count = 0, available_at = $2, lease_until = NULL, last_error = NULL
		WHERE id = $1
		  AND status = 'dead_lettered'
		  AND payload_nonce IS NOT NULL
		  AND payload_ciphertext IS NOT NULL
		  AND (expires_at IS NULL OR expires_at > $2)
	`, id, now)
	return tag.RowsAffected() == 1, err
}

func (s *Store) GetEmailOutboxCounts(ctx context.Context, now time.Time) (EmailOutboxCounts, error) {
	var result EmailOutboxCounts
	var oldestSeconds, deliveryLatencySeconds float64
	err := s.db.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE status = 'pending'),
			count(*) FILTER (WHERE status = 'retrying'),
			count(*) FILTER (WHERE status = 'sent'),
			count(*) FILTER (WHERE status = 'dead_lettered'),
			count(*) FILTER (WHERE status = 'expired'),
			COALESCE(EXTRACT(EPOCH FROM ($1 - min(created_at) FILTER (
				WHERE status IN ('pending', 'retrying', 'sending')
			))), 0),
			COALESCE(avg(EXTRACT(EPOCH FROM (sent_at - created_at))) FILTER (
				WHERE status = 'sent' AND sent_at IS NOT NULL
			), 0)
		FROM email_outbox
	`, now).Scan(&result.Pending, &result.Retrying, &result.Sent, &result.DeadLettered, &result.Expired, &oldestSeconds, &deliveryLatencySeconds)
	result.OldestPendingAge = time.Duration(oldestSeconds * float64(time.Second))
	result.DeliveryLatency = time.Duration(deliveryLatencySeconds * float64(time.Second))
	return result, err
}

func scanEmailOutbox(row rowScanner) (model.EmailOutbox, error) {
	var message model.EmailOutbox
	err := row.Scan(
		&message.ID,
		&message.IdempotencyKey,
		&message.MessageType,
		&message.TemplateVersion,
		&message.PayloadNonce,
		&message.PayloadCiphertext,
		&message.Status,
		&message.AttemptCount,
		&message.AvailableAt,
		&message.LeaseUntil,
		&message.LastError,
		&message.ExpiresAt,
		&message.SentAt,
		&message.CreatedAt,
		&message.UpdatedAt,
	)
	if err != nil {
		return model.EmailOutbox{}, fmt.Errorf("scan email outbox: %w", err)
	}
	return message, nil
}
