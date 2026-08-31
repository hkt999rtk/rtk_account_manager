package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"rtk_account_manager/internal/model"
)

// Only these reauthorizing entrypoints are exposed to human HTTP handlers.
// Low-level store methods remain for trusted bootstrap/fixture persistence.
func (s *Store) mutateDeviceItemProfileAsUser(ctx context.Context, actor, cloud, product string, platform bool, mutate func(pgx.Tx) (model.DeviceItemProfile, error)) (model.DeviceItemProfile, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.DeviceItemProfile{}, err
	}
	defer tx.Rollback(ctx)
	if platform {
		if err := lockPlatformActorTx(ctx, tx, actor); err != nil {
			return model.DeviceItemProfile{}, err
		}
		if err := lockOperationalCloudTx(ctx, tx, cloud); err != nil {
			return model.DeviceItemProfile{}, err
		}
	} else {
		if err := lockBrandCloudCollaborationTx(ctx, tx, cloud, actor); err != nil {
			return model.DeviceItemProfile{}, err
		}
		var allowed bool
		if product == "" {
			allowed, err = hasOrganizationPermission(ctx, tx, actor, cloud, "registry_device.manage")
			if err == nil && allowed {
				err = tx.QueryRow(ctx, `SELECT user_can_access_brand_cloud_product($1,$2,'')`, actor, cloud).Scan(&allowed)
			}
		} else {
			allowed, err = hasUserPermissionForResource(ctx, tx, actor, cloud, "registry_device.manage", ScopeTypeProduct, product)
		}
		if err != nil {
			return model.DeviceItemProfile{}, err
		}
		if !allowed {
			return model.DeviceItemProfile{}, ErrNotFound
		}
	}
	profile, err := mutate(tx)
	if err != nil {
		return model.DeviceItemProfile{}, err
	}
	return profile, tx.Commit(ctx)
}

func (s *Store) CreateDeviceItemProfileAsUser(ctx context.Context, in DeviceItemProfileCreateInput) (model.DeviceItemProfile, error) {
	return s.mutateDeviceItemProfileAsUser(ctx, stringValue(in.ActorUserID), in.BrandCloudID, "", in.PlatformOverride, func(tx pgx.Tx) (model.DeviceItemProfile, error) { return createDeviceItemProfileTx(ctx, tx, in) })
}

func (s *Store) UpdateDeviceItemProfileAsUser(ctx context.Context, in DeviceItemProfileUpdateInput) (model.DeviceItemProfile, error) {
	return s.mutateDeviceItemProfileAsUser(ctx, stringValue(in.ActorUserID), in.BrandCloudID, in.ProfileID, in.PlatformOverride, func(tx pgx.Tx) (model.DeviceItemProfile, error) { return updateDeviceItemProfileTx(ctx, tx, in) })
}

func (s *Store) DisableDeviceItemProfileAsUser(ctx context.Context, cloud, product, actor string, platform bool) (model.DeviceItemProfile, error) {
	return s.mutateDeviceItemProfileAsUser(ctx, actor, cloud, product, platform, func(tx pgx.Tx) (model.DeviceItemProfile, error) {
		return disableDeviceItemProfileTx(ctx, tx, cloud, product, &actor)
	})
}
