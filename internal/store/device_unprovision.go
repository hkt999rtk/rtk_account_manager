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

type DeviceUnprovisionInput struct {
	OrganizationID   string
	DeviceID         string
	ActorUserID      string
	Reason           string
	Evidence         map[string]any
	PlatformOverride bool
	Now              time.Time
}

type DeviceUnprovisionResult struct {
	DeviceID         string
	OrganizationID   string
	VideoCloudDevid  string
	ClaimTokenID     *string
	ClaimID          *string
	UnprovisionedAt  time.Time
	PlatformOverride bool
	Operation        model.DeviceOperation
	Message          model.DeviceMessageOutbox
}

func (s *Store) UnprovisionDevice(ctx context.Context, in DeviceUnprovisionInput) (DeviceUnprovisionResult, error) {
	if strings.TrimSpace(in.ActorUserID) == "" {
		return DeviceUnprovisionResult{}, ErrNotFound
	}
	if in.PlatformOverride {
		if strings.TrimSpace(in.Reason) == "" || len(in.Evidence) == 0 {
			return DeviceUnprovisionResult{}, ErrClaimEvidenceRequired
		}
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return DeviceUnprovisionResult{}, err
	}
	defer tx.Rollback(ctx)

	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		reason = "user_unprovision"
	}

	device, err := getDeviceForUnprovisionTx(ctx, tx, in.OrganizationID, in.DeviceID, in.PlatformOverride)
	if err != nil {
		return DeviceUnprovisionResult{}, err
	}
	if device.DisabledAt != nil {
		return DeviceUnprovisionResult{}, ErrDisabled
	}
	videoCloudDevid, ok := lifecycleMetadataString(device.Metadata, model.DeviceMetadataVideoCloudDevid)
	if !ok {
		return DeviceUnprovisionResult{}, ErrNotProvisioned
	}

	claim, err := getResolvedClaimByDeviceForUpdateTx(ctx, tx, device.ID)
	if err != nil {
		return DeviceUnprovisionResult{}, err
	}

	operationID := "unprovision-" + device.ID
	messageID := "unprovision-message-" + device.ID
	lifecycleInput := DeviceLifecycleOperationInput{
		OperationID:    operationID,
		CorrelationID:  operationID,
		MessageID:      messageID,
		OrganizationID: device.OrganizationID,
		DeviceID:       device.ID,
		OperationType:  model.DeviceOperationTypeUnprovision,
		RequestedBy:    &in.ActorUserID,
		RequestPayload: map[string]any{
			"video_cloud_devid": videoCloudDevid,
			"reason":            reason,
			"platform_override": in.PlatformOverride,
			"unprovisioned_at":  now,
		},
		OutboxMessageType: string(channel.MessageTypeDeviceUnprovisionRequested),
		OutboxPayload: map[string]any{
			"org_id":            device.OrganizationID,
			"account_device_id": device.ID,
			"video_cloud_devid": videoCloudDevid,
			"requested_by":      in.ActorUserID,
			"reason":            reason,
			"platform_override": in.PlatformOverride,
			"unprovisioned_at":  now,
		},
		AllowDisabled: true,
		Now:           now,
	}
	operation, created, err := createOrGetDeviceOperationTx(ctx, tx, lifecycleInput)
	if err != nil {
		return DeviceUnprovisionResult{}, err
	}
	var message model.DeviceMessageOutbox
	if created {
		message, err = createOutboxMessageTx(ctx, tx, operation, lifecycleInput)
	} else {
		message, err = getLatestOutboxMessageByOperationTx(ctx, tx, operation.OperationID)
	}
	if err != nil {
		return DeviceUnprovisionResult{}, err
	}

	if err := createAuditEventTx(ctx, tx, AuditEventInput{
		EventType:      "device_unprovisioned",
		ActorUserID:    &in.ActorUserID,
		OrganizationID: &device.OrganizationID,
		SubjectType:    "device",
		SubjectID:      device.ID,
		Payload: map[string]any{
			"device_id":              device.ID,
			"organization_id":        device.OrganizationID,
			"claim_id":               claim.ID,
			"claim_token_id":         claim.TokenID,
			"claimed_by":             claim.ClaimedBy,
			"video_cloud_devid":      videoCloudDevid,
			"device_category":        device.Category,
			"previous_device_status": device.Status,
			"reason":                 reason,
			"evidence":               defaultMetadata(in.Evidence),
			"platform_override":      in.PlatformOverride,
			"unprovisioned_at":       now,
		},
	}); err != nil {
		return DeviceUnprovisionResult{}, err
	}

	tag, err := tx.Exec(ctx, `
		DELETE FROM devices
		WHERE id = $1 AND organization_id = $2
	`, device.ID, device.OrganizationID)
	if err != nil {
		return DeviceUnprovisionResult{}, err
	}
	if tag.RowsAffected() == 0 {
		return DeviceUnprovisionResult{}, ErrNotFound
	}

	if err := tx.Commit(ctx); err != nil {
		return DeviceUnprovisionResult{}, err
	}

	return DeviceUnprovisionResult{
		DeviceID:         device.ID,
		OrganizationID:   device.OrganizationID,
		VideoCloudDevid:  videoCloudDevid,
		ClaimTokenID:     &claim.TokenID,
		ClaimID:          &claim.ID,
		UnprovisionedAt:  now,
		PlatformOverride: in.PlatformOverride,
		Operation:        operation,
		Message:          message,
	}, nil
}

func getDeviceForUnprovisionTx(ctx context.Context, tx pgx.Tx, orgID, deviceID string, platformOverride bool) (model.Device, error) {
	query := `
		SELECT id::text, organization_id::text, name, category, serial_number, mac_address, manufacturer, model, status, last_seen_at, metadata, created_at, updated_at, disabled_at, device_item_profile_id::text
		FROM devices
		WHERE organization_id = $1 AND id = $2
		FOR UPDATE
	`
	args := []any{orgID, deviceID}
	if platformOverride {
		query = `
			SELECT id::text, organization_id::text, name, category, serial_number, mac_address, manufacturer, model, status, last_seen_at, metadata, created_at, updated_at, disabled_at, device_item_profile_id::text
			FROM devices
			WHERE id = $1
			FOR UPDATE
		`
		args = []any{deviceID}
	}
	device, err := scanDevice(tx.QueryRow(ctx, query, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Device{}, ErrNotFound
	}
	return device, err
}

func getResolvedClaimByDeviceForUpdateTx(ctx context.Context, tx pgx.Tx, deviceID string) (model.DeviceClaim, error) {
	claim, err := scanDeviceClaim(tx.QueryRow(ctx, `
		SELECT id::text, claim_token_id::text, organization_id::text, device_id::text, claimed_by::text, status, provision_input, created_at, updated_at
		FROM device_claims
		WHERE device_id = $1 AND status = 'resolved'
		ORDER BY created_at DESC
		LIMIT 1
		FOR UPDATE
	`, deviceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DeviceClaim{}, ErrNotProvisioned
	}
	return claim, err
}
