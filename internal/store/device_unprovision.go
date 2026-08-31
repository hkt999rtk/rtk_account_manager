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
	if in.PlatformOverride {
		cloud, err := authorizePlatformDeviceUnprovisionTx(ctx, tx, in.ActorUserID, in.DeviceID)
		if err != nil {
			return DeviceUnprovisionResult{}, err
		}
		in.OrganizationID = cloud
	} else if err := authorizeDeviceUserMutationTx(ctx, tx, in.ActorUserID, in.OrganizationID, in.DeviceID, "device.unprovision"); err != nil {
		return DeviceUnprovisionResult{}, err
	}

	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		reason = "user_unprovision"
	}

	device, err := getDeviceForUpdateTx(ctx, tx, in.OrganizationID, in.DeviceID)
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

// Lock actor -> cloud -> device, matching ordinary writes and ownership commit.
// The first device lookup only resolves scope; the caller rechecks it under lock.
func authorizePlatformDeviceUnprovisionTx(ctx context.Context, tx pgx.Tx, actor, device string) (string, error) {
	if err := lockPlatformActorTx(ctx, tx, actor); err != nil {
		return "", err
	}
	var cloud string
	err := tx.QueryRow(ctx, `SELECT organization_id::text FROM devices WHERE id::text=$1`, device).Scan(&cloud)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return cloud, lockOperationalCloudTx(ctx, tx, cloud)
}

func lockPlatformActorTx(ctx context.Context, tx pgx.Tx, actor string) error {
	var admin bool
	err := tx.QueryRow(ctx, `SELECT COALESCE(platform_admin,false) FROM users
		WHERE id::text=$1 AND disabled_at IS NULL AND NOT signup_pending_verification FOR UPDATE`, actor).Scan(&admin)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !admin) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return nil
}

// Used by independently authenticated platform/App actors, not a human
// membership substitute. An active cloud still requires its designated owner.
func lockOperationalCloudTx(ctx context.Context, tx pgx.Tx, cloud string) error {
	var kind model.OrganizationKind
	err := tx.QueryRow(ctx, `SELECT organization_kind FROM organizations WHERE id::text=$1 AND deleted_at IS NULL FOR UPDATE`, cloud).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if kind == model.OrganizationKindBrandCloud {
		var active bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organization_members m WHERE m.organization_id::text=$1 AND m.role='owner'
			AND user_can_access_brand_cloud(m.user_id::text,$1))`, cloud).Scan(&active); err != nil {
			return err
		}
		if !active {
			return ErrNotFound
		}
	}
	return nil
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
