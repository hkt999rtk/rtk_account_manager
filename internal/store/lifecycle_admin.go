package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"rtk_account_manager/internal/model"
)

type OutboxMessageDetail struct {
	Message   model.DeviceMessageOutbox `json:"message"`
	Operation *model.DeviceOperation    `json:"operation,omitempty"`
}

type InboxMessageDetail struct {
	Message   model.DeviceMessageInbox `json:"message"`
	Operation *model.DeviceOperation   `json:"operation,omitempty"`
}

func (s *Store) ListOutboxMessagesByStatus(ctx context.Context, statuses []model.DeviceMessageOutboxStatus, limit, offset int) ([]model.DeviceMessageOutbox, error) {
	if len(statuses) == 0 {
		return []model.DeviceMessageOutbox{}, nil
	}

	rows, err := s.db.Query(ctx, `
		SELECT id::text, message_id, operation_id, correlation_id, causation_id, stream, message_type, schema_version, partition_key, payload, status, attempt_count, last_error, available_at, published_at, created_at, updated_at
		FROM device_message_outbox
		WHERE status = ANY($1)
		ORDER BY updated_at DESC, created_at DESC, message_id ASC
		LIMIT $2 OFFSET $3
	`, outboxStatusesToStrings(statuses), limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	messages := make([]model.DeviceMessageOutbox, 0)
	for rows.Next() {
		message, err := scanDeviceMessageOutbox(rows)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return messages, nil
}

func (s *Store) ListInboxMessagesByStatus(ctx context.Context, statuses []model.DeviceMessageInboxStatus, limit, offset int) ([]model.DeviceMessageInbox, error) {
	if len(statuses) == 0 {
		return []model.DeviceMessageInbox{}, nil
	}

	rows, err := s.db.Query(ctx, `
		SELECT id::text, message_id, operation_id, correlation_id, causation_id, stream, message_type, schema_version, partition_key, payload, status, attempt_count, last_error, received_at, processed_at, created_at, updated_at
		FROM device_message_inbox
		WHERE status = ANY($1)
		ORDER BY updated_at DESC, created_at DESC, message_id ASC
		LIMIT $2 OFFSET $3
	`, inboxStatusesToStrings(statuses), limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	messages := make([]model.DeviceMessageInbox, 0)
	for rows.Next() {
		message, err := scanDeviceMessageInbox(rows)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return messages, nil
}

func (s *Store) GetOutboxMessageDetail(ctx context.Context, messageID string) (OutboxMessageDetail, error) {
	message, err := s.GetOutboxMessage(ctx, messageID)
	if err != nil {
		return OutboxMessageDetail{}, err
	}

	operation, err := s.getDeviceOperationOptional(ctx, message.OperationID)
	if err != nil {
		return OutboxMessageDetail{}, err
	}

	return OutboxMessageDetail{
		Message:   message,
		Operation: operation,
	}, nil
}

func (s *Store) GetInboxMessageDetail(ctx context.Context, messageID string) (InboxMessageDetail, error) {
	message, err := s.GetInboxMessage(ctx, messageID)
	if err != nil {
		return InboxMessageDetail{}, err
	}

	operation, err := s.getDeviceOperationOptional(ctx, message.OperationID)
	if err != nil {
		return InboxMessageDetail{}, err
	}

	return InboxMessageDetail{
		Message:   message,
		Operation: operation,
	}, nil
}

func (s *Store) RequeueOutboxMessage(ctx context.Context, messageID string, availableAt time.Time) (model.DeviceMessageOutbox, *model.DeviceOperation, bool, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.DeviceMessageOutbox{}, nil, false, err
	}
	defer tx.Rollback(ctx)

	message, err := scanDeviceMessageOutbox(tx.QueryRow(ctx, `
		SELECT id::text, message_id, operation_id, correlation_id, causation_id, stream, message_type, schema_version, partition_key, payload, status, attempt_count, last_error, available_at, published_at, created_at, updated_at
		FROM device_message_outbox
		WHERE message_id = $1
		FOR UPDATE
	`, messageID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DeviceMessageOutbox{}, nil, false, ErrNotFound
	}
	if err != nil {
		return model.DeviceMessageOutbox{}, nil, false, err
	}

	operation, err := scanDeviceOperation(tx.QueryRow(ctx, `
		SELECT id::text, operation_id, correlation_id, organization_id::text, device_id::text, operation_type, status, requested_by, request_payload, result_payload, error_code, error_message, retryable, created_at, updated_at, completed_at
		FROM device_operations
		WHERE operation_id = $1
		FOR UPDATE
	`, message.OperationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DeviceMessageOutbox{}, nil, false, ErrNotFound
	}
	if err != nil {
		return model.DeviceMessageOutbox{}, nil, false, err
	}

	if isCompletedLifecycleOperationStatus(operation.Status, operation.OperationType) {
		return model.DeviceMessageOutbox{}, nil, false, ErrConflict
	}

	switch message.Status {
	case model.DeviceMessageOutboxStatusPending:
		if err := tx.Commit(ctx); err != nil {
			return model.DeviceMessageOutbox{}, nil, false, err
		}
		return message, &operation, false, nil
	case model.DeviceMessageOutboxStatusRetrying, model.DeviceMessageOutboxStatusDeadLettered:
	default:
		return model.DeviceMessageOutbox{}, nil, false, ErrConflict
	}

	availableAt = availableAt.UTC().Truncate(time.Microsecond)
	message, err = scanDeviceMessageOutbox(tx.QueryRow(ctx, `
		UPDATE device_message_outbox
		SET status = 'pending',
			attempt_count = 0,
			last_error = NULL,
			available_at = $2,
			published_at = NULL
		WHERE message_id = $1
		RETURNING id::text, message_id, operation_id, correlation_id, causation_id, stream, message_type, schema_version, partition_key, payload, status, attempt_count, last_error, available_at, published_at, created_at, updated_at
	`, messageID, availableAt))
	if err != nil {
		return model.DeviceMessageOutbox{}, nil, false, err
	}

	operation, err = scanDeviceOperation(tx.QueryRow(ctx, `
		UPDATE device_operations
		SET status = 'pending',
			result_payload = '{}'::jsonb,
			error_code = NULL,
			error_message = NULL,
			retryable = NULL,
			completed_at = NULL
		WHERE operation_id = $1
		RETURNING id::text, operation_id, correlation_id, organization_id::text, device_id::text, operation_type, status, requested_by, request_payload, result_payload, error_code, error_message, retryable, created_at, updated_at, completed_at
	`, operation.OperationID))
	if err != nil {
		return model.DeviceMessageOutbox{}, nil, false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return model.DeviceMessageOutbox{}, nil, false, err
	}
	return message, &operation, true, nil
}

func (s *Store) RequeueInboxMessage(ctx context.Context, messageID string) (model.DeviceMessageInbox, *model.DeviceOperation, bool, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.DeviceMessageInbox{}, nil, false, err
	}
	defer tx.Rollback(ctx)

	message, err := scanDeviceMessageInbox(tx.QueryRow(ctx, `
		SELECT id::text, message_id, operation_id, correlation_id, causation_id, stream, message_type, schema_version, partition_key, payload, status, attempt_count, last_error, received_at, processed_at, created_at, updated_at
		FROM device_message_inbox
		WHERE message_id = $1
		FOR UPDATE
	`, messageID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DeviceMessageInbox{}, nil, false, ErrNotFound
	}
	if err != nil {
		return model.DeviceMessageInbox{}, nil, false, err
	}

	operation, err := getDeviceOperationOptionalTx(ctx, tx, message.OperationID)
	if err != nil {
		return model.DeviceMessageInbox{}, nil, false, err
	}

	switch message.Status {
	case model.DeviceMessageInboxStatusRetrying:
		if err := tx.Commit(ctx); err != nil {
			return model.DeviceMessageInbox{}, nil, false, err
		}
		return message, operation, false, nil
	case model.DeviceMessageInboxStatusDeadLettered:
	default:
		return model.DeviceMessageInbox{}, nil, false, ErrConflict
	}

	message, err = scanDeviceMessageInbox(tx.QueryRow(ctx, `
		UPDATE device_message_inbox
		SET status = 'retrying',
			attempt_count = 0,
			last_error = NULL,
			processed_at = NULL
		WHERE message_id = $1
		RETURNING id::text, message_id, operation_id, correlation_id, causation_id, stream, message_type, schema_version, partition_key, payload, status, attempt_count, last_error, received_at, processed_at, created_at, updated_at
	`, messageID))
	if err != nil {
		return model.DeviceMessageInbox{}, nil, false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return model.DeviceMessageInbox{}, nil, false, err
	}
	return message, operation, true, nil
}

func (s *Store) getDeviceOperationOptional(ctx context.Context, operationID string) (*model.DeviceOperation, error) {
	operation, err := s.GetDeviceOperation(ctx, operationID)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &operation, nil
}

func getDeviceOperationOptionalTx(ctx context.Context, tx pgx.Tx, operationID string) (*model.DeviceOperation, error) {
	operation, err := scanDeviceOperation(tx.QueryRow(ctx, `
		SELECT id::text, operation_id, correlation_id, organization_id::text, device_id::text, operation_type, status, requested_by, request_payload, result_payload, error_code, error_message, retryable, created_at, updated_at, completed_at
		FROM device_operations
		WHERE operation_id = $1
		FOR UPDATE
	`, operationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &operation, nil
}

func outboxStatusesToStrings(statuses []model.DeviceMessageOutboxStatus) []string {
	values := make([]string, 0, len(statuses))
	for _, status := range statuses {
		values = append(values, string(status))
	}
	return values
}

func inboxStatusesToStrings(statuses []model.DeviceMessageInboxStatus) []string {
	values := make([]string, 0, len(statuses))
	for _, status := range statuses {
		values = append(values, string(status))
	}
	return values
}

func isCompletedLifecycleOperationStatus(status model.DeviceOperationStatus, operationType model.DeviceOperationType) bool {
	if operationType != model.DeviceOperationTypeProvision && operationType != model.DeviceOperationTypeDeactivate {
		return false
	}
	return status == model.DeviceOperationStatusSucceeded || status == model.DeviceOperationStatusFailed
}
