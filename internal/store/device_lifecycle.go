package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"rtk_account_manager/internal/channel"
	"rtk_account_manager/internal/model"
)

type DeviceLifecycleOperationInput struct {
	OperationID       string
	CorrelationID     string
	MessageID         string
	OrganizationID    string
	DeviceID          string
	OperationType     model.DeviceOperationType
	RequestedBy       *string
	RequestPayload    map[string]any
	OutboxMessageType string
	OutboxPayload     map[string]any
	MetadataPatch     map[string]any
	AllowDisabled     bool
	Now               time.Time
}

type DeviceLifecycleOperationResult struct {
	Device    model.Device
	Operation model.DeviceOperation
	Message   model.DeviceMessageOutbox
	Created   bool
}

type DeviceDeactivationOperationInput struct {
	OperationID    string
	CorrelationID  string
	MessageID      string
	OrganizationID string
	DeviceID       string
	RequestedBy    *string
	Reason         string
	Now            time.Time
}

func (s *Store) StartDeviceLifecycleOperation(ctx context.Context, in DeviceLifecycleOperationInput) (DeviceLifecycleOperationResult, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return DeviceLifecycleOperationResult{}, err
	}
	defer tx.Rollback(ctx)

	device, err := getDeviceForUpdateTx(ctx, tx, in.OrganizationID, in.DeviceID)
	if err != nil {
		return DeviceLifecycleOperationResult{}, err
	}
	return startDeviceLifecycleOperationTx(ctx, tx, device, in)
}

func (s *Store) StartDeviceDeactivationOperation(ctx context.Context, in DeviceDeactivationOperationInput) (DeviceLifecycleOperationResult, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return DeviceLifecycleOperationResult{}, err
	}
	defer tx.Rollback(ctx)

	device, err := getDeviceForUpdateTx(ctx, tx, in.OrganizationID, in.DeviceID)
	if err != nil {
		return DeviceLifecycleOperationResult{}, err
	}

	videoCloudDevid, ok := lifecycleMetadataString(device.Metadata, model.DeviceMetadataVideoCloudDevid)
	if !ok {
		return DeviceLifecycleOperationResult{}, ErrNotProvisioned
	}

	return startDeviceLifecycleOperationTx(ctx, tx, device, DeviceLifecycleOperationInput{
		OperationID:       in.OperationID,
		CorrelationID:     in.CorrelationID,
		MessageID:         in.MessageID,
		OrganizationID:    in.OrganizationID,
		DeviceID:          in.DeviceID,
		OperationType:     model.DeviceOperationTypeDeactivate,
		RequestedBy:       in.RequestedBy,
		RequestPayload:    map[string]any{"video_cloud_devid": videoCloudDevid, "reason": in.Reason},
		OutboxMessageType: string(channel.MessageTypeDeviceDeactivateRequested),
		OutboxPayload: map[string]any{
			"org_id":            in.OrganizationID,
			"account_device_id": in.DeviceID,
			"video_cloud_devid": videoCloudDevid,
			"requested_by":      stringValue(in.RequestedBy),
			"reason":            in.Reason,
		},
		AllowDisabled: true,
		Now:           in.Now,
	})
}

func startDeviceLifecycleOperationTx(ctx context.Context, tx pgx.Tx, device model.Device, in DeviceLifecycleOperationInput) (DeviceLifecycleOperationResult, error) {
	if !in.AllowDisabled && device.DisabledAt != nil {
		return DeviceLifecycleOperationResult{}, ErrDisabled
	}

	operation, created, err := createOrGetDeviceOperationTx(ctx, tx, in)
	if err != nil {
		return DeviceLifecycleOperationResult{}, err
	}

	var message model.DeviceMessageOutbox
	if created {
		message, err = createOutboxMessageTx(ctx, tx, operation, in)
	} else {
		message, err = getLatestOutboxMessageByOperationTx(ctx, tx, operation.OperationID)
	}
	if err != nil {
		return DeviceLifecycleOperationResult{}, err
	}

	if created && len(in.MetadataPatch) > 0 {
		device, err = projectDeviceTx(ctx, tx, device.OrganizationID, device.ID, DeviceProjectionInput{
			Metadata:      in.MetadataPatch,
			AllowDisabled: in.AllowDisabled,
		})
		if err != nil {
			return DeviceLifecycleOperationResult{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return DeviceLifecycleOperationResult{}, err
	}

	return DeviceLifecycleOperationResult{
		Device:    device,
		Operation: operation,
		Message:   message,
		Created:   created,
	}, nil
}

func (s *Store) GetLatestDeviceOperationByType(ctx context.Context, orgID, deviceID string, operationType model.DeviceOperationType) (model.DeviceOperation, error) {
	operation, err := scanDeviceOperation(s.db.QueryRow(ctx, `
		SELECT id::text, operation_id, correlation_id, organization_id::text, device_id::text, operation_type, status, requested_by, request_payload, result_payload, error_code, error_message, retryable, created_at, updated_at, completed_at
		FROM device_operations
		WHERE organization_id = $1 AND device_id = $2 AND operation_type = $3
		ORDER BY created_at DESC
		LIMIT 1
	`, orgID, deviceID, operationType))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DeviceOperation{}, ErrNotFound
	}
	return operation, err
}

func (s *Store) GetLatestOutboxMessageByOperationID(ctx context.Context, operationID string) (model.DeviceMessageOutbox, error) {
	message, err := scanDeviceMessageOutbox(s.db.QueryRow(ctx, `
		SELECT id::text, message_id, operation_id, correlation_id, causation_id, stream, message_type, schema_version, partition_key, payload, status, attempt_count, last_error, available_at, published_at, created_at, updated_at
		FROM device_message_outbox
		WHERE operation_id = $1
		ORDER BY created_at DESC
		LIMIT 1
	`, operationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DeviceMessageOutbox{}, ErrNotFound
	}
	return message, err
}

func getDeviceForUpdateTx(ctx context.Context, tx pgx.Tx, orgID, deviceID string) (model.Device, error) {
	device, err := scanDevice(tx.QueryRow(ctx, `
		SELECT id::text, organization_id::text, name, category, serial_number, mac_address, manufacturer, model, status, last_seen_at, metadata, created_at, updated_at, disabled_at, device_item_profile_id::text
		FROM devices
		WHERE organization_id = $1 AND id = $2
		FOR UPDATE
	`, orgID, deviceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Device{}, ErrNotFound
	}
	return device, err
}

func createOrGetDeviceOperationTx(ctx context.Context, tx pgx.Tx, in DeviceLifecycleOperationInput) (model.DeviceOperation, bool, error) {
	requestPayload, err := marshalJSONMap(in.RequestPayload)
	if err != nil {
		return model.DeviceOperation{}, false, err
	}

	row := tx.QueryRow(ctx, `
		INSERT INTO device_operations (
			operation_id,
			correlation_id,
			organization_id,
			device_id,
			operation_type,
			status,
			requested_by,
			request_payload,
			result_payload
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, '{}'::jsonb)
		ON CONFLICT (operation_id) DO NOTHING
		RETURNING id::text, operation_id, correlation_id, organization_id::text, device_id::text, operation_type, status, requested_by, request_payload, result_payload, error_code, error_message, retryable, created_at, updated_at, completed_at
	`, in.OperationID, in.CorrelationID, in.OrganizationID, in.DeviceID, in.OperationType, model.DeviceOperationStatusPending, in.RequestedBy, requestPayload)

	operation, err := scanDeviceOperation(row)
	if err == nil {
		return operation, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return model.DeviceOperation{}, false, err
	}

	existing, err := scanDeviceOperation(tx.QueryRow(ctx, `
		SELECT id::text, operation_id, correlation_id, organization_id::text, device_id::text, operation_type, status, requested_by, request_payload, result_payload, error_code, error_message, retryable, created_at, updated_at, completed_at
		FROM device_operations
		WHERE operation_id = $1
	`, in.OperationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DeviceOperation{}, false, ErrNotFound
	}
	if err != nil {
		return model.DeviceOperation{}, false, err
	}
	if err := compareOperationCreate(existing, DeviceOperationCreateInput{
		CorrelationID:  in.CorrelationID,
		OrganizationID: in.OrganizationID,
		DeviceID:       in.DeviceID,
		OperationType:  in.OperationType,
		RequestPayload: in.RequestPayload,
	}, requestPayload); err != nil {
		return model.DeviceOperation{}, false, err
	}
	return existing, false, nil
}

func createOutboxMessageTx(ctx context.Context, tx pgx.Tx, operation model.DeviceOperation, in DeviceLifecycleOperationInput) (model.DeviceMessageOutbox, error) {
	payload, err := marshalJSONMap(in.OutboxPayload)
	if err != nil {
		return model.DeviceMessageOutbox{}, err
	}

	message, err := scanDeviceMessageOutbox(tx.QueryRow(ctx, `
		INSERT INTO device_message_outbox (
			message_id,
			operation_id,
			correlation_id,
			stream,
			message_type,
			schema_version,
			partition_key,
			payload,
			status,
			attempt_count,
			available_at
		)
		VALUES ($1, $2, $3, 'account.video.commands', $4, '1.0', $5, $6::jsonb, 'pending', 0, $7)
		RETURNING id::text, message_id, operation_id, correlation_id, causation_id, stream, message_type, schema_version, partition_key, payload, status, attempt_count, last_error, available_at, published_at, created_at, updated_at
	`, in.MessageID, operation.OperationID, operation.CorrelationID, in.OutboxMessageType, operation.DeviceID, payload, in.Now))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DeviceMessageOutbox{}, ErrNotFound
	}
	return message, err
}

func getLatestOutboxMessageByOperationTx(ctx context.Context, tx pgx.Tx, operationID string) (model.DeviceMessageOutbox, error) {
	message, err := scanDeviceMessageOutbox(tx.QueryRow(ctx, `
		SELECT id::text, message_id, operation_id, correlation_id, causation_id, stream, message_type, schema_version, partition_key, payload, status, attempt_count, last_error, available_at, published_at, created_at, updated_at
		FROM device_message_outbox
		WHERE operation_id = $1
		ORDER BY created_at DESC
		LIMIT 1
	`, operationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DeviceMessageOutbox{}, ErrNotFound
	}
	return message, err
}

func lifecycleMetadataString(metadata map[string]any, key string) (string, bool) {
	value, ok := metadata[key]
	if !ok {
		return "", false
	}
	text, ok := value.(string)
	if !ok {
		return "", false
	}
	text = strings.TrimSpace(text)
	return text, text != ""
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
