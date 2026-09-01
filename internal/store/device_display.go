package store

import (
	"context"
	"strings"
	"unicode/utf8"

	"rtk_account_manager/internal/model"
)

// DeviceDisplayPatch deliberately excludes hardware identity, claim material,
// service metadata and Product binding. The legacy full-record update cannot
// safely serve a display-only editor because it replaces omitted columns.
type DeviceDisplayPatch struct {
	Name  *string `json:"name"`
	Model *string `json:"model"`
}

func (s *Store) PatchProductDeviceDisplay(ctx context.Context, actor, cloud, product, device string, in DeviceDisplayPatch) (model.Device, error) {
	if in.Name == nil && in.Model == nil {
		return model.Device{}, ErrConflict
	}
	if in.Name != nil {
		v := strings.TrimSpace(*in.Name)
		if v == "" || utf8.RuneCountInString(v) > 255 {
			return model.Device{}, ErrConflict
		}
		in.Name = &v
	}
	if in.Model != nil {
		v := strings.TrimSpace(*in.Model)
		if utf8.RuneCountInString(v) > 255 {
			return model.Device{}, ErrConflict
		}
		in.Model = &v
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.Device{}, err
	}
	defer tx.Rollback(ctx)
	if err = lockBrandCloudCollaborationTx(ctx, tx, cloud, actor); err != nil {
		return model.Device{}, err
	}
	d, err := getDeviceForUpdateTx(ctx, tx, cloud, device)
	if err != nil {
		return model.Device{}, err
	}
	if d.DeviceItemProfileID == nil || *d.DeviceItemProfileID != product {
		return model.Device{}, ErrNotFound
	}
	if d.DisabledAt != nil {
		return model.Device{}, ErrDisabled
	}
	allowed, err := hasUserDevicePermission(ctx, tx, actor, cloud, "registry_device.manage", device)
	if err != nil {
		return model.Device{}, err
	}
	if !allowed {
		return model.Device{}, ErrNotFound
	}
	d, err = s.scanDevice(tx.QueryRow(ctx, `UPDATE devices SET name=COALESCE($3,name),model=COALESCE($4,model),updated_at=now()
	 WHERE organization_id::text=$1 AND id::text=$2 AND device_item_profile_id::text=$5 AND disabled_at IS NULL
	 RETURNING id::text,organization_id::text,name,category,serial_number,mac_address,manufacturer,model,status,last_seen_at,metadata,created_at,updated_at,disabled_at,device_item_profile_id::text`, cloud, device, in.Name, in.Model, product))
	if err != nil {
		return model.Device{}, err
	}
	if err = createAuditEventTx(ctx, tx, AuditEventInput{ActorUserID: &actor, OrganizationID: &cloud, EventType: "device.display.updated", SubjectType: "device", SubjectID: device, Payload: map[string]any{"product_id": product}}); err != nil {
		return model.Device{}, err
	}
	return d, tx.Commit(ctx)
}
