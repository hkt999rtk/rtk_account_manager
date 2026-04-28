package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"rtk_account_manager/internal/model"
)

type OutboxPublishTransitionInput struct {
	MessageID             string
	ExpectedMessageStatus model.DeviceMessageOutboxStatus
	ExpectedAttemptCount  int
	ExpectedAvailableAt   time.Time
	MessageStatus         model.DeviceMessageOutboxStatus
	AttemptCount          int
	LastError             *string
	AvailableAt           time.Time
	PublishedAt           *time.Time
	OperationStatus       model.DeviceOperationStatus
	OperationErrorCode    *string
	OperationErrorMessage *string
	OperationRetryable    *bool
	OperationCompletedAt  *time.Time
}

type OutboxPublishTransitionResult struct {
	Message   model.DeviceMessageOutbox
	Operation model.DeviceOperation
}

func (s *Store) RecordOutboxPublishTransition(ctx context.Context, in OutboxPublishTransitionInput) (OutboxPublishTransitionResult, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return OutboxPublishTransitionResult{}, err
	}
	defer tx.Rollback(ctx)

	message, err := scanDeviceMessageOutbox(tx.QueryRow(ctx, `
		UPDATE device_message_outbox
		SET status = $2,
			attempt_count = $6,
			last_error = $7,
			available_at = $8,
			published_at = $9
		WHERE message_id = $1
			AND status = $3
			AND attempt_count = $4
			AND available_at = $5
		RETURNING id::text, message_id, operation_id, correlation_id, causation_id, stream, message_type, schema_version, partition_key, payload, status, attempt_count, last_error, available_at, published_at, created_at, updated_at
	`, in.MessageID, in.MessageStatus, in.ExpectedMessageStatus, in.ExpectedAttemptCount, in.ExpectedAvailableAt, in.AttemptCount, in.LastError, in.AvailableAt, in.PublishedAt))
	if errors.Is(err, pgx.ErrNoRows) {
		return OutboxPublishTransitionResult{}, classifyOutboxPublishTransitionError(ctx, tx, in.MessageID)
	}
	if err != nil {
		return OutboxPublishTransitionResult{}, err
	}

	resultPayload, err := marshalJSONMap(nil)
	if err != nil {
		return OutboxPublishTransitionResult{}, err
	}

	operation, err := scanDeviceOperation(tx.QueryRow(ctx, `
		UPDATE device_operations
		SET status = $2,
			result_payload = $3::jsonb,
			error_code = $4,
			error_message = $5,
			retryable = $6,
			completed_at = $7
		WHERE operation_id = $1
		RETURNING id::text, operation_id, correlation_id, organization_id::text, device_id::text, operation_type, status, requested_by, request_payload, result_payload, error_code, error_message, retryable, created_at, updated_at, completed_at
	`, message.OperationID, in.OperationStatus, resultPayload, in.OperationErrorCode, in.OperationErrorMessage, in.OperationRetryable, in.OperationCompletedAt))
	if errors.Is(err, pgx.ErrNoRows) {
		return OutboxPublishTransitionResult{}, ErrNotFound
	}
	if err != nil {
		return OutboxPublishTransitionResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return OutboxPublishTransitionResult{}, err
	}

	return OutboxPublishTransitionResult{
		Message:   message,
		Operation: operation,
	}, nil
}

func classifyOutboxPublishTransitionError(ctx context.Context, tx pgx.Tx, messageID string) error {
	var exists bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1
			FROM device_message_outbox
			WHERE message_id = $1
		)
	`, messageID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	return ErrConflict
}
