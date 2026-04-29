package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"rtk_account_manager/internal/channel"
	"rtk_account_manager/internal/model"
)

const projectionMetadataPrefix = "video_cloud_"

func PendingProvisionMetadata(videoCloudDevid, activityID string) map[string]any {
	return map[string]any{
		model.DeviceMetadataVideoCloudDevid:            videoCloudDevid,
		model.DeviceMetadataVideoCloudActivityID:       activityID,
		model.DeviceMetadataVideoCloudActivationStatus: model.VideoCloudActivationStatusPending,
		model.DeviceMetadataVideoCloudActivatedAt:      nil,
		model.DeviceMetadataVideoCloudDeactivatedAt:    nil,
		model.DeviceMetadataVideoCloudLastError:        nil,
	}
}

type DeviceProjectionInput struct {
	Metadata      map[string]any
	Status        *model.DeviceStatus
	LastSeenAt    *time.Time
	AllowDisabled bool
}

func ProvisionSucceededProjection(payload channel.DeviceProvisionSucceededPayload) DeviceProjectionInput {
	return DeviceProjectionInput{
		Metadata: map[string]any{
			model.DeviceMetadataVideoCloudDevid:            payload.VideoCloudDevid,
			model.DeviceMetadataVideoCloudActivityID:       payload.ActivityID,
			model.DeviceMetadataVideoCloudActivationStatus: model.VideoCloudActivationStatusActivated,
			model.DeviceMetadataVideoCloudActivatedAt:      payload.ActivatedAt.UTC(),
			model.DeviceMetadataVideoCloudLastError:        nil,
		},
	}
}

func ProvisionFailedProjection(payload channel.DeviceProvisionFailedPayload) DeviceProjectionInput {
	return DeviceProjectionInput{
		Metadata: map[string]any{
			model.DeviceMetadataVideoCloudDevid:            payload.VideoCloudDevid,
			model.DeviceMetadataVideoCloudActivityID:       payload.ActivityID,
			model.DeviceMetadataVideoCloudActivationStatus: model.VideoCloudActivationStatusFailed,
			model.DeviceMetadataVideoCloudLastError: map[string]any{
				"code":       payload.ErrorCode,
				"message":    payload.ErrorMessage,
				"retryable":  payload.Retryable,
				"failed_at":  payload.FailedAt.UTC(),
				"operation":  channel.MessageTypeDeviceProvisionFailed,
				"service":    channel.ServiceRealtekVideoCloud,
				"updated_at": payload.FailedAt.UTC(),
			},
		},
	}
}

func DeactivateSucceededProjection(payload channel.DeviceDeactivateSucceededPayload) DeviceProjectionInput {
	return DeviceProjectionInput{
		AllowDisabled: true,
		Metadata: map[string]any{
			model.DeviceMetadataVideoCloudDevid:            payload.VideoCloudDevid,
			model.DeviceMetadataVideoCloudActivationStatus: model.VideoCloudActivationStatusDeactivated,
			model.DeviceMetadataVideoCloudDeactivatedAt:    payload.DeactivatedAt.UTC(),
			model.DeviceMetadataVideoCloudLastError:        nil,
		},
	}
}

func DeactivateFailedProjection(payload channel.DeviceDeactivateFailedPayload) DeviceProjectionInput {
	return DeviceProjectionInput{
		AllowDisabled: true,
		Metadata: map[string]any{
			model.DeviceMetadataVideoCloudDevid: payload.VideoCloudDevid,
			model.DeviceMetadataVideoCloudLastError: map[string]any{
				"code":       payload.ErrorCode,
				"message":    payload.ErrorMessage,
				"retryable":  payload.Retryable,
				"failed_at":  payload.FailedAt.UTC(),
				"operation":  channel.MessageTypeDeviceDeactivateFailed,
				"service":    channel.ServiceRealtekVideoCloud,
				"updated_at": payload.FailedAt.UTC(),
			},
		},
	}
}

func OnlineChangedProjection(payload channel.DeviceOnlineChangedPayload) DeviceProjectionInput {
	status := model.DeviceStatus(payload.Status)
	lastSeenAt := payload.LastSeenAt.UTC()
	return DeviceProjectionInput{
		Metadata: map[string]any{
			model.DeviceMetadataVideoCloudDevid: payload.VideoCloudDevid,
		},
		Status:     &status,
		LastSeenAt: &lastSeenAt,
	}
}

func MetadataChangedProjection(payload channel.DeviceMetadataChangedPayload) DeviceProjectionInput {
	return DeviceProjectionInput{
		Metadata: selectProjectionMetadata(payload.Metadata),
	}
}

func (s *Store) MergeDeviceMetadata(ctx context.Context, orgID, deviceID string, patch map[string]any) (model.Device, error) {
	return s.ProjectDevice(ctx, orgID, deviceID, DeviceProjectionInput{Metadata: patch})
}

func (s *Store) ProjectDevice(ctx context.Context, orgID, deviceID string, in DeviceProjectionInput) (model.Device, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.Device{}, err
	}
	defer tx.Rollback(ctx)

	projected, err := projectDeviceTx(ctx, tx, orgID, deviceID, in)
	if err != nil {
		return model.Device{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return model.Device{}, err
	}
	return projected, nil
}

func projectDeviceTx(ctx context.Context, tx pgx.Tx, orgID, deviceID string, in DeviceProjectionInput) (model.Device, error) {
	device, err := scanDevice(tx.QueryRow(ctx, `
		SELECT id::text, organization_id::text, name, category, serial_number, mac_address, manufacturer, model, status, last_seen_at, metadata, created_at, updated_at, disabled_at
		FROM devices
		WHERE organization_id = $1 AND id = $2
		FOR UPDATE
	`, orgID, deviceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Device{}, ErrNotFound
	}
	if err != nil {
		return model.Device{}, err
	}

	if device.DisabledAt != nil && (!in.AllowDisabled || in.Status != nil || in.LastSeenAt != nil) {
		return model.Device{}, ErrDisabled
	}

	metadata, err := json.Marshal(applyProjectionMetadata(device.Metadata, in.Metadata))
	if err != nil {
		return model.Device{}, err
	}

	status := device.Status
	if in.Status != nil {
		status = *in.Status
	}

	lastSeenAt := device.LastSeenAt
	if in.LastSeenAt != nil {
		lastSeenAt = in.LastSeenAt
	}

	projected, err := scanDevice(tx.QueryRow(ctx, `
		UPDATE devices
		SET status = $3, last_seen_at = $4, metadata = $5, updated_at = now()
		WHERE organization_id = $1 AND id = $2
		RETURNING id::text, organization_id::text, name, category, serial_number, mac_address, manufacturer, model, status, last_seen_at, metadata, created_at, updated_at, disabled_at
	`, orgID, deviceID, status, lastSeenAt, metadata))
	if err != nil {
		return model.Device{}, err
	}
	return projected, nil
}

func selectProjectionMetadata(metadata map[string]any) map[string]any {
	if len(metadata) == 0 {
		return nil
	}

	selected := make(map[string]any)
	for key, value := range metadata {
		if strings.HasPrefix(key, projectionMetadataPrefix) {
			selected[key] = value
		}
	}
	if len(selected) == 0 {
		return nil
	}
	return selected
}

func applyProjectionMetadata(existing, patch map[string]any) map[string]any {
	merged := make(map[string]any, len(existing)+len(patch))
	for key, value := range defaultMetadata(existing) {
		merged[key] = value
	}
	for key, value := range selectProjectionMetadata(patch) {
		if value == nil {
			delete(merged, key)
			continue
		}
		merged[key] = value
	}
	return merged
}
