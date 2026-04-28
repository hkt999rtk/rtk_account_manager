package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"rtk_account_manager/internal/model"
)

type InboxProcessTransitionInput struct {
	MessageID             string
	MessageStatus         model.DeviceMessageInboxStatus
	AttemptCount          int
	LastError             *string
	ProcessedAt           *time.Time
	OperationStatus       *model.DeviceOperationStatus
	OperationResult       map[string]any
	OperationErrorCode    *string
	OperationErrorMessage *string
	OperationRetryable    *bool
	OperationCompletedAt  *time.Time
	OrganizationID        string
	DeviceID              string
	Projection            *DeviceProjectionInput
}

type InboxProcessTransitionResult struct {
	Message   model.DeviceMessageInbox
	Operation *model.DeviceOperation
	Device    *model.Device
}

func (s *Store) RecordInboxProcessTransition(ctx context.Context, in InboxProcessTransitionInput) (InboxProcessTransitionResult, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return InboxProcessTransitionResult{}, err
	}
	defer tx.Rollback(ctx)

	message, err := scanDeviceMessageInbox(tx.QueryRow(ctx, `
		UPDATE device_message_inbox
		SET status = $2,
			attempt_count = $3,
			last_error = $4,
			processed_at = $5
		WHERE message_id = $1
		RETURNING id::text, message_id, operation_id, correlation_id, causation_id, stream, message_type, schema_version, partition_key, payload, status, attempt_count, last_error, received_at, processed_at, created_at, updated_at
	`, in.MessageID, in.MessageStatus, in.AttemptCount, in.LastError, in.ProcessedAt))
	if errors.Is(err, pgx.ErrNoRows) {
		return InboxProcessTransitionResult{}, ErrNotFound
	}
	if err != nil {
		return InboxProcessTransitionResult{}, err
	}

	result := InboxProcessTransitionResult{Message: message}

	if in.OperationStatus != nil {
		resultPayload, err := marshalJSONMap(in.OperationResult)
		if err != nil {
			return InboxProcessTransitionResult{}, err
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
			return InboxProcessTransitionResult{}, ErrNotFound
		}
		if err != nil {
			return InboxProcessTransitionResult{}, err
		}
		if operation.OrganizationID != in.OrganizationID || operation.DeviceID != in.DeviceID {
			return InboxProcessTransitionResult{}, ErrConflict
		}
		result.Operation = &operation
	}

	if in.Projection != nil {
		projected, err := projectDeviceTx(ctx, tx, in.OrganizationID, in.DeviceID, *in.Projection)
		if err != nil {
			return InboxProcessTransitionResult{}, err
		}
		result.Device = &projected
	}

	if err := tx.Commit(ctx); err != nil {
		return InboxProcessTransitionResult{}, err
	}
	return result, nil
}
