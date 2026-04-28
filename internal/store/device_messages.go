package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"rtk_account_manager/internal/model"
)

type DeviceOperationCreateInput struct {
	OperationID    string
	CorrelationID  string
	OrganizationID string
	DeviceID       string
	OperationType  model.DeviceOperationType
	Status         model.DeviceOperationStatus
	RequestedBy    *string
	RequestPayload map[string]any
	ResultPayload  map[string]any
	ErrorCode      *string
	ErrorMessage   *string
	Retryable      *bool
	CompletedAt    *time.Time
}

type DeviceOperationUpdateInput struct {
	Status        model.DeviceOperationStatus
	ResultPayload map[string]any
	ErrorCode     *string
	ErrorMessage  *string
	Retryable     *bool
	CompletedAt   *time.Time
}

type DeviceMessageOutboxCreateInput struct {
	MessageID     string
	OperationID   string
	CorrelationID string
	CausationID   *string
	Stream        string
	MessageType   string
	SchemaVersion string
	PartitionKey  string
	Payload       map[string]any
	Status        model.DeviceMessageOutboxStatus
	AttemptCount  int
	LastError     *string
	AvailableAt   time.Time
	PublishedAt   *time.Time
}

type DeviceMessageOutboxUpdateInput struct {
	Status       model.DeviceMessageOutboxStatus
	AttemptCount int
	LastError    *string
	AvailableAt  time.Time
	PublishedAt  *time.Time
}

type DeviceMessageInboxCreateInput struct {
	MessageID     string
	OperationID   string
	CorrelationID string
	CausationID   *string
	Stream        string
	MessageType   string
	SchemaVersion string
	PartitionKey  string
	Payload       map[string]any
	Status        model.DeviceMessageInboxStatus
	AttemptCount  int
	LastError     *string
	ReceivedAt    time.Time
	ProcessedAt   *time.Time
}

type DeviceMessageInboxUpdateInput struct {
	Status       model.DeviceMessageInboxStatus
	AttemptCount int
	LastError    *string
	ProcessedAt  *time.Time
}

func (s *Store) CreateOrGetDeviceOperation(ctx context.Context, in DeviceOperationCreateInput) (model.DeviceOperation, bool, error) {
	requestPayload, err := marshalJSONMap(in.RequestPayload)
	if err != nil {
		return model.DeviceOperation{}, false, err
	}
	resultPayload, err := marshalJSONMap(in.ResultPayload)
	if err != nil {
		return model.DeviceOperation{}, false, err
	}

	row := s.db.QueryRow(ctx, `
		INSERT INTO device_operations (
			operation_id,
			correlation_id,
			organization_id,
			device_id,
			operation_type,
			status,
			requested_by,
			request_payload,
			result_payload,
			error_code,
			error_message,
			retryable,
			completed_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9::jsonb, $10, $11, $12, $13)
		ON CONFLICT (operation_id) DO NOTHING
		RETURNING id::text, operation_id, correlation_id, organization_id::text, device_id::text, operation_type, status, requested_by, request_payload, result_payload, error_code, error_message, retryable, created_at, updated_at, completed_at
	`, in.OperationID, in.CorrelationID, in.OrganizationID, in.DeviceID, in.OperationType, in.Status, in.RequestedBy, requestPayload, resultPayload, in.ErrorCode, in.ErrorMessage, in.Retryable, in.CompletedAt)

	operation, err := scanDeviceOperation(row)
	if err == nil {
		return operation, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return model.DeviceOperation{}, false, err
	}

	existing, err := s.GetDeviceOperation(ctx, in.OperationID)
	if err != nil {
		return model.DeviceOperation{}, false, err
	}
	if err := compareOperationCreate(existing, in, requestPayload); err != nil {
		return model.DeviceOperation{}, false, err
	}
	return existing, false, nil
}

func (s *Store) GetDeviceOperation(ctx context.Context, operationID string) (model.DeviceOperation, error) {
	operation, err := scanDeviceOperation(s.db.QueryRow(ctx, `
		SELECT id::text, operation_id, correlation_id, organization_id::text, device_id::text, operation_type, status, requested_by, request_payload, result_payload, error_code, error_message, retryable, created_at, updated_at, completed_at
		FROM device_operations
		WHERE operation_id = $1
	`, operationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DeviceOperation{}, ErrNotFound
	}
	return operation, err
}

func (s *Store) UpdateDeviceOperation(ctx context.Context, operationID string, in DeviceOperationUpdateInput) (model.DeviceOperation, error) {
	resultPayload, err := marshalJSONMap(in.ResultPayload)
	if err != nil {
		return model.DeviceOperation{}, err
	}

	operation, err := scanDeviceOperation(s.db.QueryRow(ctx, `
		UPDATE device_operations
		SET status = $2,
			result_payload = $3::jsonb,
			error_code = $4,
			error_message = $5,
			retryable = $6,
			completed_at = $7
		WHERE operation_id = $1
		RETURNING id::text, operation_id, correlation_id, organization_id::text, device_id::text, operation_type, status, requested_by, request_payload, result_payload, error_code, error_message, retryable, created_at, updated_at, completed_at
	`, operationID, in.Status, resultPayload, in.ErrorCode, in.ErrorMessage, in.Retryable, in.CompletedAt))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DeviceOperation{}, ErrNotFound
	}
	return operation, err
}

func (s *Store) CreateOutboxMessage(ctx context.Context, in DeviceMessageOutboxCreateInput) (model.DeviceMessageOutbox, error) {
	payload, err := marshalJSONMap(in.Payload)
	if err != nil {
		return model.DeviceMessageOutbox{}, err
	}

	message, err := scanDeviceMessageOutbox(s.db.QueryRow(ctx, `
		INSERT INTO device_message_outbox (
			message_id,
			operation_id,
			correlation_id,
			causation_id,
			stream,
			message_type,
			schema_version,
			partition_key,
			payload,
			status,
			attempt_count,
			last_error,
			available_at,
			published_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10, $11, $12, $13, $14)
		RETURNING id::text, message_id, operation_id, correlation_id, causation_id, stream, message_type, schema_version, partition_key, payload, status, attempt_count, last_error, available_at, published_at, created_at, updated_at
	`, in.MessageID, in.OperationID, in.CorrelationID, in.CausationID, in.Stream, in.MessageType, in.SchemaVersion, in.PartitionKey, payload, in.Status, in.AttemptCount, in.LastError, in.AvailableAt, in.PublishedAt))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DeviceMessageOutbox{}, ErrNotFound
	}
	return message, err
}

func (s *Store) GetOutboxMessage(ctx context.Context, messageID string) (model.DeviceMessageOutbox, error) {
	message, err := scanDeviceMessageOutbox(s.db.QueryRow(ctx, `
		SELECT id::text, message_id, operation_id, correlation_id, causation_id, stream, message_type, schema_version, partition_key, payload, status, attempt_count, last_error, available_at, published_at, created_at, updated_at
		FROM device_message_outbox
		WHERE message_id = $1
	`, messageID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DeviceMessageOutbox{}, ErrNotFound
	}
	return message, err
}

func (s *Store) ListOutboxMessagesReady(ctx context.Context, readyBefore time.Time, limit int) ([]model.DeviceMessageOutbox, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id::text, message_id, operation_id, correlation_id, causation_id, stream, message_type, schema_version, partition_key, payload, status, attempt_count, last_error, available_at, published_at, created_at, updated_at
		FROM device_message_outbox
		WHERE status IN ('pending', 'retrying') AND available_at <= $1
		ORDER BY available_at ASC, created_at ASC
		LIMIT $2
	`, readyBefore, limit)
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

func (s *Store) UpdateOutboxMessage(ctx context.Context, messageID string, in DeviceMessageOutboxUpdateInput) (model.DeviceMessageOutbox, error) {
	message, err := scanDeviceMessageOutbox(s.db.QueryRow(ctx, `
		UPDATE device_message_outbox
		SET status = $2,
			attempt_count = $3,
			last_error = $4,
			available_at = $5,
			published_at = $6
		WHERE message_id = $1
		RETURNING id::text, message_id, operation_id, correlation_id, causation_id, stream, message_type, schema_version, partition_key, payload, status, attempt_count, last_error, available_at, published_at, created_at, updated_at
	`, messageID, in.Status, in.AttemptCount, in.LastError, in.AvailableAt, in.PublishedAt))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DeviceMessageOutbox{}, ErrNotFound
	}
	return message, err
}

func (s *Store) CreateOrGetInboxMessage(ctx context.Context, in DeviceMessageInboxCreateInput) (model.DeviceMessageInbox, bool, error) {
	payload, err := marshalJSONMap(in.Payload)
	if err != nil {
		return model.DeviceMessageInbox{}, false, err
	}

	row := s.db.QueryRow(ctx, `
		INSERT INTO device_message_inbox (
			message_id,
			operation_id,
			correlation_id,
			causation_id,
			stream,
			message_type,
			schema_version,
			partition_key,
			payload,
			status,
			attempt_count,
			last_error,
			received_at,
			processed_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10, $11, $12, $13, $14)
		ON CONFLICT (message_id) DO NOTHING
		RETURNING id::text, message_id, operation_id, correlation_id, causation_id, stream, message_type, schema_version, partition_key, payload, status, attempt_count, last_error, received_at, processed_at, created_at, updated_at
	`, in.MessageID, in.OperationID, in.CorrelationID, in.CausationID, in.Stream, in.MessageType, in.SchemaVersion, in.PartitionKey, payload, in.Status, in.AttemptCount, in.LastError, in.ReceivedAt, in.ProcessedAt)

	message, err := scanDeviceMessageInbox(row)
	if err == nil {
		return message, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return model.DeviceMessageInbox{}, false, err
	}

	existing, err := s.GetInboxMessage(ctx, in.MessageID)
	if err != nil {
		return model.DeviceMessageInbox{}, false, err
	}
	if err := compareInboxCreate(existing, in, payload); err != nil {
		return model.DeviceMessageInbox{}, false, err
	}
	return existing, false, nil
}

func (s *Store) GetInboxMessage(ctx context.Context, messageID string) (model.DeviceMessageInbox, error) {
	message, err := scanDeviceMessageInbox(s.db.QueryRow(ctx, `
		SELECT id::text, message_id, operation_id, correlation_id, causation_id, stream, message_type, schema_version, partition_key, payload, status, attempt_count, last_error, received_at, processed_at, created_at, updated_at
		FROM device_message_inbox
		WHERE message_id = $1
	`, messageID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DeviceMessageInbox{}, ErrNotFound
	}
	return message, err
}

func (s *Store) UpdateInboxMessage(ctx context.Context, messageID string, in DeviceMessageInboxUpdateInput) (model.DeviceMessageInbox, error) {
	message, err := scanDeviceMessageInbox(s.db.QueryRow(ctx, `
		UPDATE device_message_inbox
		SET status = $2,
			attempt_count = $3,
			last_error = $4,
			processed_at = $5
		WHERE message_id = $1
		RETURNING id::text, message_id, operation_id, correlation_id, causation_id, stream, message_type, schema_version, partition_key, payload, status, attempt_count, last_error, received_at, processed_at, created_at, updated_at
	`, messageID, in.Status, in.AttemptCount, in.LastError, in.ProcessedAt))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DeviceMessageInbox{}, ErrNotFound
	}
	return message, err
}

func compareOperationCreate(existing model.DeviceOperation, in DeviceOperationCreateInput, requestPayload []byte) error {
	if existing.CorrelationID != in.CorrelationID ||
		existing.OrganizationID != in.OrganizationID ||
		existing.DeviceID != in.DeviceID ||
		existing.OperationType != in.OperationType ||
		!sameJSONMap(existing.RequestPayload, requestPayload) {
		return ErrConflict
	}
	return nil
}

func compareInboxCreate(existing model.DeviceMessageInbox, in DeviceMessageInboxCreateInput, payload []byte) error {
	if existing.OperationID != in.OperationID ||
		existing.CorrelationID != in.CorrelationID ||
		!sameStringPtr(existing.CausationID, in.CausationID) ||
		existing.Stream != in.Stream ||
		existing.MessageType != in.MessageType ||
		existing.SchemaVersion != in.SchemaVersion ||
		existing.PartitionKey != in.PartitionKey ||
		!sameJSONMap(existing.Payload, payload) {
		return ErrConflict
	}
	return nil
}

func sameStringPtr(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func sameJSONMap(existing map[string]any, want []byte) bool {
	got, err := marshalJSONMap(existing)
	if err != nil {
		return false
	}
	return bytes.Equal(got, want)
}

func marshalJSONMap(value map[string]any) ([]byte, error) {
	if value == nil {
		value = map[string]any{}
	}
	return json.Marshal(value)
}

func scanDeviceOperation(row rowScanner) (model.DeviceOperation, error) {
	var operation model.DeviceOperation
	var requestPayload []byte
	var resultPayload []byte
	err := row.Scan(
		&operation.ID,
		&operation.OperationID,
		&operation.CorrelationID,
		&operation.OrganizationID,
		&operation.DeviceID,
		&operation.OperationType,
		&operation.Status,
		&operation.RequestedBy,
		&requestPayload,
		&resultPayload,
		&operation.ErrorCode,
		&operation.ErrorMessage,
		&operation.Retryable,
		&operation.CreatedAt,
		&operation.UpdatedAt,
		&operation.CompletedAt,
	)
	if err != nil {
		return model.DeviceOperation{}, err
	}
	requestMap, err := unmarshalJSONMap(requestPayload)
	if err != nil {
		return model.DeviceOperation{}, err
	}
	resultMap, err := unmarshalJSONMap(resultPayload)
	if err != nil {
		return model.DeviceOperation{}, err
	}
	operation.RequestPayload = requestMap
	operation.ResultPayload = resultMap
	return operation, nil
}

func scanDeviceMessageOutbox(row rowScanner) (model.DeviceMessageOutbox, error) {
	var message model.DeviceMessageOutbox
	var payload []byte
	err := row.Scan(
		&message.ID,
		&message.MessageID,
		&message.OperationID,
		&message.CorrelationID,
		&message.CausationID,
		&message.Stream,
		&message.MessageType,
		&message.SchemaVersion,
		&message.PartitionKey,
		&payload,
		&message.Status,
		&message.AttemptCount,
		&message.LastError,
		&message.AvailableAt,
		&message.PublishedAt,
		&message.CreatedAt,
		&message.UpdatedAt,
	)
	if err != nil {
		return model.DeviceMessageOutbox{}, err
	}
	message.Payload, err = unmarshalJSONMap(payload)
	if err != nil {
		return model.DeviceMessageOutbox{}, err
	}
	return message, nil
}

func scanDeviceMessageInbox(row rowScanner) (model.DeviceMessageInbox, error) {
	var message model.DeviceMessageInbox
	var payload []byte
	err := row.Scan(
		&message.ID,
		&message.MessageID,
		&message.OperationID,
		&message.CorrelationID,
		&message.CausationID,
		&message.Stream,
		&message.MessageType,
		&message.SchemaVersion,
		&message.PartitionKey,
		&payload,
		&message.Status,
		&message.AttemptCount,
		&message.LastError,
		&message.ReceivedAt,
		&message.ProcessedAt,
		&message.CreatedAt,
		&message.UpdatedAt,
	)
	if err != nil {
		return model.DeviceMessageInbox{}, err
	}
	message.Payload, err = unmarshalJSONMap(payload)
	if err != nil {
		return model.DeviceMessageInbox{}, err
	}
	return message, nil
}

func unmarshalJSONMap(value []byte) (map[string]any, error) {
	if len(value) == 0 {
		return map[string]any{}, nil
	}
	var decoded map[string]any
	if err := json.Unmarshal(value, &decoded); err != nil {
		return nil, err
	}
	if decoded == nil {
		return map[string]any{}, nil
	}
	return decoded, nil
}
