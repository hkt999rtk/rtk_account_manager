package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"rtk_account_manager/internal/model"
)

// Human mutations reauthorize within their write transaction. Middleware alone
// cannot stop an admitted request from writing after transfer/membership removal. The
// actor -> organization -> device lock order matches the ownership coordinator.
// Internal producer methods still require their separate lifecycle fences.
func (s *Store) mutateDeviceAsUser(ctx context.Context, actor, org, device, event string, mutate func(pgx.Tx) (model.Device, error)) (model.Device, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.Device{}, err
	}
	defer tx.Rollback(ctx)
	if err := authorizeDeviceUserMutationTx(ctx, tx, actor, org, device, "registry_device.manage"); err != nil {
		return model.Device{}, err
	}
	result, err := mutate(tx)
	if err != nil {
		return model.Device{}, err
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{ActorUserID: &actor, OrganizationID: &org, EventType: event, SubjectType: "device", SubjectID: result.ID}); err != nil {
		return model.Device{}, err
	}
	return result, tx.Commit(ctx)
}

func authorizeDeviceUserMutationTx(ctx context.Context, tx pgx.Tx, actor, org, device, permission string) error {
	var platformAdmin bool
	err := tx.QueryRow(ctx, `SELECT COALESCE(platform_admin,false) FROM users
	 WHERE id::text=$1 AND disabled_at IS NULL AND signup_pending_verification=false FOR UPDATE`, actor).Scan(&platformAdmin)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	var kind model.OrganizationKind
	err = tx.QueryRow(ctx, `SELECT organization_kind FROM organizations WHERE id::text=$1 AND deleted_at IS NULL FOR UPDATE`, org).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if device != "" {
		if _, err := getDeviceForUpdateTx(ctx, tx, org, device); err != nil {
			return err
		}
	}
	// Keep legacy customer-organization platform administration. Platform
	// capabilities never substitute for Brand Cloud membership/owner authority.
	if kind == model.OrganizationKindBrandCloud || !platformAdmin {
		var allowed bool
		if device == "" {
			allowed, err = hasOrganizationPermission(ctx, tx, actor, org, permission)
		} else {
			allowed, err = hasUserDevicePermission(ctx, tx, actor, org, permission, device)
		}
		if err != nil {
			return err
		}
		if !allowed {
			return ErrNotFound
		}
	}
	return nil
}

func (s *Store) CreateDeviceAsUser(ctx context.Context, actor, org string, in DeviceInput) (model.Device, error) {
	return s.mutateDeviceAsUser(ctx, actor, org, "", "device.created", func(tx pgx.Tx) (model.Device, error) {
		if product := stringValue(in.DeviceItemProfileID); product != "" {
			if err := authorizeProductUserMutationTx(ctx, tx, actor, org, product, false); err != nil {
				return model.Device{}, err
			}
			profile, err := getDeviceItemProfileByID(ctx, tx, product)
			if err != nil {
				return model.Device{}, err
			}
			if profile.BrandCloudID != org || profile.Status != model.DeviceItemProfileStatusActive {
				return model.Device{}, ErrNotFound
			}
		}
		return createDeviceTx(ctx, tx, org, in)
	})
}

func (s *Store) UpdateDeviceAsUser(ctx context.Context, actor, org, device string, in DeviceInput) (model.Device, error) {
	return s.mutateDeviceAsUser(ctx, actor, org, device, "device.updated", func(tx pgx.Tx) (model.Device, error) {
		return updateDevice(ctx, tx, org, device, in)
	})
}

func (s *Store) UpdateDeviceStatusAsUser(ctx context.Context, actor, org, device string, status model.DeviceStatus, lastSeenAt *time.Time) (model.Device, error) {
	return s.mutateDeviceAsUser(ctx, actor, org, device, "device.status.updated", func(tx pgx.Tx) (model.Device, error) {
		return updateDeviceStatus(ctx, tx, org, device, status, lastSeenAt)
	})
}

func (s *Store) DeleteDeviceAsUser(ctx context.Context, actor, org, device string) error {
	_, err := s.mutateDeviceAsUser(ctx, actor, org, device, "device.deleted", func(tx pgx.Tx) (model.Device, error) {
		_, err := tx.Exec(ctx, `UPDATE devices SET status='disabled',disabled_at=COALESCE(disabled_at,now()),updated_at=now()
		 WHERE organization_id::text=$1 AND id::text=$2`, org, device)
		return model.Device{ID: device}, err
	})
	return err
}
