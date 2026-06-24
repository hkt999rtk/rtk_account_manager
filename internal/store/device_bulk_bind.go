package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"rtk_account_manager/internal/model"
)

type BulkBindDeviceStatus string

const (
	BulkBindDeviceStatusCreated  BulkBindDeviceStatus = "created"
	BulkBindDeviceStatusExisting BulkBindDeviceStatus = "existing"
	BulkBindDeviceStatusFailed   BulkBindDeviceStatus = "failed"
)

type BulkBindDeviceInput struct {
	DeviceName      string
	Category        model.DeviceCategory
	VideoCloudDevid string
	ActivityID      string
	ClipPublicKey   string
	ServiceOptions  []string
}

type BulkBindDeviceItemResult struct {
	VideoCloudDevid string               `json:"video_cloud_devid"`
	Status          BulkBindDeviceStatus `json:"status"`
	Device          model.Device         `json:"device,omitempty"`
	ProvisionInput  DeviceProvisionInput `json:"provision_input,omitempty"`
	ErrorCode       string               `json:"error_code,omitempty"`
	ErrorMessage    string               `json:"error_message,omitempty"`
}

type BulkBindDevicesResult struct {
	Status    string                     `json:"status"`
	Requested int                        `json:"requested"`
	Created   int                        `json:"created"`
	Existing  int                        `json:"existing"`
	Failed    int                        `json:"failed"`
	Results   []BulkBindDeviceItemResult `json:"results"`
}

func (s *Store) BulkBindDevices(ctx context.Context, orgID string, items []BulkBindDeviceInput) (BulkBindDevicesResult, error) {
	result := BulkBindDevicesResult{
		Status:    "completed",
		Requested: len(items),
		Results:   make([]BulkBindDeviceItemResult, len(items)),
	}
	if len(items) == 0 {
		return result, nil
	}
	if err := s.ensureOrganizationExists(ctx, orgID); err != nil {
		return BulkBindDevicesResult{}, err
	}

	normalized := make([]BulkBindDeviceInput, len(items))
	videoCloudDevids := make([]string, 0, len(items))
	seen := map[string]struct{}{}
	for i, item := range items {
		item.DeviceName = strings.TrimSpace(item.DeviceName)
		item.VideoCloudDevid = strings.TrimSpace(item.VideoCloudDevid)
		item.ActivityID = strings.TrimSpace(item.ActivityID)
		item.ClipPublicKey = strings.TrimSpace(item.ClipPublicKey)
		item.ServiceOptions = canonicalStringSlice(item.ServiceOptions)
		normalized[i] = item
		result.Results[i].VideoCloudDevid = item.VideoCloudDevid
		if err := validateBulkBindDeviceInput(item); err != nil {
			result.Results[i] = failedBulkBindResult(item.VideoCloudDevid, "invalid_item", err.Error())
			result.Failed++
			continue
		}
		if _, exists := seen[item.VideoCloudDevid]; exists {
			result.Results[i] = failedBulkBindResult(item.VideoCloudDevid, "duplicate_in_request", "video_cloud_devid appears more than once in this request")
			result.Failed++
			continue
		}
		seen[item.VideoCloudDevid] = struct{}{}
		videoCloudDevids = append(videoCloudDevids, item.VideoCloudDevid)
	}

	existing, err := s.getActiveDevicesByVideoCloudDevid(ctx, orgID, videoCloudDevids)
	if err != nil {
		return BulkBindDevicesResult{}, err
	}
	for i, item := range normalized {
		if result.Results[i].Status == BulkBindDeviceStatusFailed {
			continue
		}
		if device, ok := existing[item.VideoCloudDevid]; ok {
			result.Results[i] = bulkBindDeviceResult(item, BulkBindDeviceStatusExisting, device)
			result.Existing++
			continue
		}
		device, err := s.createBulkBoundDevice(ctx, orgID, item)
		if err != nil {
			if isUniqueViolation(err) {
				device, lookupErr := s.getActiveDeviceByVideoCloudDevid(ctx, orgID, item.VideoCloudDevid)
				if lookupErr == nil {
					result.Results[i] = bulkBindDeviceResult(item, BulkBindDeviceStatusExisting, device)
					result.Existing++
					continue
				}
			}
			result.Results[i] = failedBulkBindResult(item.VideoCloudDevid, "create_failed", err.Error())
			result.Failed++
			continue
		}
		result.Results[i] = bulkBindDeviceResult(item, BulkBindDeviceStatusCreated, device)
		result.Created++
	}
	return result, nil
}

func validateBulkBindDeviceInput(item BulkBindDeviceInput) error {
	if item.DeviceName == "" {
		return errors.New("device_name is required")
	}
	switch item.Category {
	case model.DeviceCategoryIPCamera, model.DeviceCategoryMQTT, model.DeviceCategoryGeneric:
	default:
		return ErrClaimUnsupportedCategory
	}
	if item.VideoCloudDevid == "" {
		return errors.New("video_cloud_devid is required")
	}
	if item.ActivityID == "" {
		return errors.New("activity_id is required")
	}
	if item.ClipPublicKey == "" {
		return errors.New("clip_public_key is required")
	}
	if len(item.ServiceOptions) == 0 {
		return errors.New("service_options must include at least one option")
	}
	return validateClaimServiceOptions(item.ServiceOptions)
}

func canonicalStringSlice(raw []string) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, item := range raw {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if _, exists := seen[item]; exists {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

func failedBulkBindResult(videoCloudDevid, code, message string) BulkBindDeviceItemResult {
	return BulkBindDeviceItemResult{
		VideoCloudDevid: videoCloudDevid,
		Status:          BulkBindDeviceStatusFailed,
		ErrorCode:       code,
		ErrorMessage:    message,
	}
}

func bulkBindDeviceResult(item BulkBindDeviceInput, status BulkBindDeviceStatus, device model.Device) BulkBindDeviceItemResult {
	return BulkBindDeviceItemResult{
		VideoCloudDevid: item.VideoCloudDevid,
		Status:          status,
		Device:          device,
		ProvisionInput: DeviceProvisionInput{
			VideoCloudDevid: item.VideoCloudDevid,
			ActivityID:      item.ActivityID,
			ClipPublicKey:   item.ClipPublicKey,
			ServiceOptions:  item.ServiceOptions,
		},
	}
}

func (s *Store) ensureOrganizationExists(ctx context.Context, orgID string) error {
	var exists bool
	if err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM organizations WHERE id = $1)`, orgID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	return nil
}

func (s *Store) getActiveDevicesByVideoCloudDevid(ctx context.Context, orgID string, videoCloudDevids []string) (map[string]model.Device, error) {
	devices := map[string]model.Device{}
	if len(videoCloudDevids) == 0 {
		return devices, nil
	}
	rows, err := s.db.Query(ctx, `
		SELECT id::text, organization_id::text, name, category, serial_number, mac_address, manufacturer, model, status, last_seen_at, metadata, created_at, updated_at, disabled_at
		FROM devices
		WHERE organization_id = $1
		  AND disabled_at IS NULL
		  AND metadata->>$2 = ANY($3::text[])
		ORDER BY created_at ASC
	`, orgID, model.DeviceMetadataVideoCloudDevid, videoCloudDevids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		device, err := scanDeviceRows(rows)
		if err != nil {
			return nil, err
		}
		videoCloudDevid, _ := device.Metadata[model.DeviceMetadataVideoCloudDevid].(string)
		if _, exists := devices[videoCloudDevid]; !exists {
			devices[videoCloudDevid] = device
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return devices, nil
}

func (s *Store) getActiveDeviceByVideoCloudDevid(ctx context.Context, orgID, videoCloudDevid string) (model.Device, error) {
	device, err := scanDevice(s.db.QueryRow(ctx, `
		SELECT id::text, organization_id::text, name, category, serial_number, mac_address, manufacturer, model, status, last_seen_at, metadata, created_at, updated_at, disabled_at
		FROM devices
		WHERE organization_id = $1
		  AND disabled_at IS NULL
		  AND metadata->>$2 = $3
		ORDER BY created_at ASC
		LIMIT 1
	`, orgID, model.DeviceMetadataVideoCloudDevid, videoCloudDevid))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Device{}, ErrNotFound
	}
	return device, err
}

func (s *Store) createBulkBoundDevice(ctx context.Context, orgID string, item BulkBindDeviceInput) (model.Device, error) {
	metadata := map[string]any{
		model.DeviceMetadataVideoCloudDevid:         item.VideoCloudDevid,
		model.DeviceMetadataVideoCloudActivityID:    item.ActivityID,
		model.DeviceMetadataVideoCloudClipPublicKey: item.ClipPublicKey,
		model.DeviceMetadataServiceOptions:          item.ServiceOptions,
	}
	rawMetadata, err := json.Marshal(metadata)
	if err != nil {
		return model.Device{}, err
	}
	return scanDevice(s.db.QueryRow(ctx, `
		INSERT INTO devices (organization_id, name, category, metadata)
		VALUES ($1, $2, $3, $4)
		RETURNING id::text, organization_id::text, name, category, serial_number, mac_address, manufacturer, model, status, last_seen_at, metadata, created_at, updated_at, disabled_at
	`, orgID, item.DeviceName, item.Category, rawMetadata))
}
