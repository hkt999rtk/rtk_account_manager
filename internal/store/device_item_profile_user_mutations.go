package store

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

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
	if err := authorizeProductUserMutationTx(ctx, tx, actor, cloud, product, platform); err != nil {
		return model.DeviceItemProfile{}, err
	}
	profile, err := mutate(tx)
	if err != nil {
		return model.DeviceItemProfile{}, err
	}
	return profile, tx.Commit(ctx)
}

func authorizeProductUserMutationTx(ctx context.Context, tx pgx.Tx, actor, cloud, product string, platform bool) error {
	if platform {
		if err := lockPlatformActorTx(ctx, tx, actor); err != nil {
			return err
		}
		if err := lockOperationalCloudTx(ctx, tx, cloud); err != nil {
			return err
		}
	} else {
		if err := lockBrandCloudCollaborationTx(ctx, tx, cloud, actor); err != nil {
			return err
		}
		var allowed bool
		var err error
		if product == "" {
			allowed, err = hasOrganizationPermission(ctx, tx, actor, cloud, "registry_device.manage")
			if err == nil && allowed {
				err = tx.QueryRow(ctx, `SELECT user_can_access_brand_cloud_product($1,$2,'')`, actor, cloud).Scan(&allowed)
			}
		} else {
			allowed, err = hasUserPermissionForResource(ctx, tx, actor, cloud, "registry_device.manage", ScopeTypeProduct, product)
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

func (s *Store) CreateDeviceItemProfileAsUser(ctx context.Context, in DeviceItemProfileCreateInput) (model.DeviceItemProfile, error) {
	return s.mutateDeviceItemProfileAsUser(ctx, stringValue(in.ActorUserID), in.BrandCloudID, "", in.PlatformOverride, func(tx pgx.Tx) (model.DeviceItemProfile, error) {
		options := slices.Clone(in.ServiceOptions)
		bindings, err := s.validateProductServiceSelectionTx(ctx, tx, options, in.CatalogRevision, time.Now().UTC())
		if err != nil {
			return model.DeviceItemProfile{}, err
		}
		profile, err := createDeviceItemProfileTx(ctx, tx, in, s.platformServiceProductWrites)
		if err != nil {
			return profile, err
		}
		if s.platformServiceProductWrites {
			if err := insertProductServiceGrantTx(ctx, tx, profile.ID, in.BrandCloudID, options, bindings, in.CatalogRevision, in.ActorUserID, profile.LogRetentionDays); err != nil {
				return model.DeviceItemProfile{}, err
			}
		}
		return profile, nil
	})
}

func (s *Store) UpdateDeviceItemProfileAsUser(ctx context.Context, in DeviceItemProfileUpdateInput) (model.DeviceItemProfile, error) {
	return s.mutateDeviceItemProfileAsUser(ctx, stringValue(in.ActorUserID), in.BrandCloudID, in.ProfileID, in.PlatformOverride, func(tx pgx.Tx) (model.DeviceItemProfile, error) {
		var current model.DeviceItemProfile
		var err error
		if s.platformServiceProductWrites {
			current, err = getDeviceItemProfile(ctx, tx, in.BrandCloudID, in.ProfileID, true)
			if err != nil {
				return model.DeviceItemProfile{}, err
			}
		}
		profile, err := updateDeviceItemProfileTx(ctx, tx, in, s.platformServiceProductWrites)
		if err != nil {
			return profile, err
		}
		if s.platformServiceProductWrites {
			oldOptions, options := slices.Clone(current.ServiceOptions), slices.Clone(profile.ServiceOptions)
			slices.Sort(oldOptions)
			slices.Sort(options)
			if !slices.Equal(oldOptions, options) || !sameRetention(current.LogRetentionDays, profile.LogRetentionDays) {
				bindings, catalogRevision, err := s.validateProductServiceUpdateTx(ctx, tx, profile.ID, in.BrandCloudID, oldOptions, options, in.CatalogRevision, time.Now().UTC())
				if err != nil {
					return model.DeviceItemProfile{}, err
				}
				if err := insertProductServiceGrantTx(ctx, tx, profile.ID, in.BrandCloudID, options, bindings, catalogRevision, in.ActorUserID, profile.LogRetentionDays); err != nil {
					return model.DeviceItemProfile{}, err
				}
			}
		}
		return profile, nil
	})
}

func sameRetention(a, b *int) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

// Existing selections retain their immutable binding even if their service is
// temporarily unavailable. Only newly added options require a live catalog.
func (s *Store) validateProductServiceUpdateTx(ctx context.Context, tx pgx.Tx, productID, cloudID string, oldOptions, options []string, observedRevision int64, now time.Time) ([]PlatformServiceCatalogOption, int64, error) {
	if validateProductServiceOptions(options) != nil || !slices.Contains(options, "mqtt") {
		return nil, 0, ErrClaimUnsupportedService
	}
	var raw []byte
	var catalogRevision int64
	err := tx.QueryRow(ctx, `SELECT bindings,catalog_revision FROM product_service_grants WHERE product_id=$1 AND brand_cloud_id=$2 ORDER BY revision DESC LIMIT 1`, productID, cloudID).Scan(&raw, &catalogRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, ErrConflict
	}
	if err != nil {
		return nil, 0, err
	}
	var oldBindings []PlatformServiceCatalogOption
	if err := json.Unmarshal(raw, &oldBindings); err != nil {
		return nil, 0, ErrConflict
	}
	oldSet := make(map[string]bool, len(oldOptions))
	for _, code := range oldOptions {
		oldSet[code] = true
	}
	bindings := make([]PlatformServiceCatalogOption, 0, len(options))
	var additions []string
	for _, code := range options {
		if !oldSet[code] {
			additions = append(additions, code)
			continue
		}
		for _, binding := range oldBindings {
			if binding.Code == code {
				bindings = append(bindings, binding)
				break
			}
		}
	}
	if len(additions) != 0 {
		if observedRevision < 1 {
			return nil, 0, ErrConflict
		}
		if _, err := lockCatalogRevision(ctx, tx, s.platformServiceEnvironment); err != nil {
			return nil, 0, err
		}
		catalog, err := readPlatformServiceOptions(ctx, tx, s.platformServiceEnvironment, now)
		if err != nil {
			return nil, 0, err
		}
		if catalog.CatalogRevision != observedRevision {
			return nil, 0, ErrConflict
		}
		for _, code := range additions {
			found := false
			for _, candidate := range catalog.Options {
				if candidate.Code == code {
					if !candidate.Selectable {
						return nil, 0, ErrConflict
					}
					bindings = append(bindings, candidate)
					found = true
					break
				}
			}
			if !found {
				return nil, 0, ErrClaimUnsupportedService
			}
		}
		catalogRevision = observedRevision
	}
	selected := make(map[string]bool, len(options))
	for _, code := range options {
		selected[code] = true
	}
	for _, binding := range bindings {
		for _, dependency := range binding.Requires {
			if !selected[dependency] {
				return nil, 0, ErrClaimUnsupportedService
			}
		}
	}
	slices.SortFunc(bindings, func(a, b PlatformServiceCatalogOption) int { return strings.Compare(a.Code, b.Code) })
	return bindings, catalogRevision, nil
}

func (s *Store) DisableDeviceItemProfileAsUser(ctx context.Context, cloud, product, actor string, platform bool) (model.DeviceItemProfile, error) {
	return s.mutateDeviceItemProfileAsUser(ctx, actor, cloud, product, platform, func(tx pgx.Tx) (model.DeviceItemProfile, error) {
		return disableDeviceItemProfileTx(ctx, tx, cloud, product, &actor)
	})
}
