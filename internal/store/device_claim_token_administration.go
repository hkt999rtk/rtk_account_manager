package store

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"rtk_account_manager/internal/model"
)

func (s *Store) CreateDeviceClaimTokenAsPlatform(ctx context.Context, in DeviceClaimTokenCreateInput) (model.DeviceClaimToken, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.DeviceClaimToken{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockPlatformActorTx(ctx, tx, stringValue(in.CreatedBy)); err != nil {
		return model.DeviceClaimToken{}, err
	}
	if err := lockClaimTokenCloudsTx(ctx, tx, in.OrganizationID, in.DeviceItemProfileID); err != nil {
		return model.DeviceClaimToken{}, err
	}
	token, err := createDeviceClaimTokenTx(ctx, tx, in)
	if err != nil {
		return model.DeviceClaimToken{}, err
	}
	if err := auditClaimTokenAdministrationTx(ctx, tx, "device_claim_token_created", stringValue(in.CreatedBy), token); err != nil {
		return model.DeviceClaimToken{}, err
	}
	return token, tx.Commit(ctx)
}

func (s *Store) RevokeDeviceClaimTokenAsPlatform(ctx context.Context, actor, tokenID string, now time.Time) (model.DeviceClaimToken, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.DeviceClaimToken{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockPlatformActorTx(ctx, tx, actor); err != nil {
		return model.DeviceClaimToken{}, err
	}
	var org, product *string
	err = tx.QueryRow(ctx, `SELECT organization_id::text,device_item_profile_id::text FROM device_claim_tokens WHERE id::text=$1`, tokenID).Scan(&org, &product)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DeviceClaimToken{}, ErrNotFound
	}
	if err != nil {
		return model.DeviceClaimToken{}, err
	}
	if err := lockClaimTokenCloudsTx(ctx, tx, org, product); err != nil {
		return model.DeviceClaimToken{}, err
	}
	token, err := getClaimTokenForUpdateTx(ctx, tx, tokenID)
	if err != nil {
		return model.DeviceClaimToken{}, err
	}
	if stringValue(token.OrganizationID) != stringValue(org) || stringValue(token.DeviceItemProfileID) != stringValue(product) {
		return model.DeviceClaimToken{}, ErrConflict
	}
	if token.RevokedAt != nil {
		return token, tx.Commit(ctx)
	}
	token, err = revokeDeviceClaimToken(ctx, tx, tokenID, now)
	if err != nil {
		return model.DeviceClaimToken{}, err
	}
	if err := auditClaimTokenAdministrationTx(ctx, tx, "device_claim_token_revoked", actor, token); err != nil {
		return model.DeviceClaimToken{}, err
	}
	return token, tx.Commit(ctx)
}

// Actor is already locked. Resolve scope without resource locks, lock clouds in
// sorted order, then revalidate the Product under lock before the token lock.
func lockClaimTokenCloudsTx(ctx context.Context, tx pgx.Tx, org, product *string) error {
	var manufacturer string
	if product != nil {
		err := tx.QueryRow(ctx, `SELECT brand_cloud_id::text FROM device_item_profiles WHERE id::text=$1`, *product).Scan(&manufacturer)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
	}
	clouds := []string{stringValue(org), manufacturer}
	slices.Sort(clouds)
	for i, cloud := range clouds {
		if cloud == "" || (i > 0 && cloud == clouds[i-1]) {
			continue
		}
		if err := lockOperationalCloudTx(ctx, tx, cloud); err != nil {
			return err
		}
	}
	if product != nil {
		var current string
		err := tx.QueryRow(ctx, `SELECT brand_cloud_id::text FROM device_item_profiles WHERE id::text=$1 FOR UPDATE`, *product).Scan(&current)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if current != manufacturer {
			return ErrConflict
		}
		if org != nil && *org != manufacturer {
			var kind model.OrganizationKind
			if err := tx.QueryRow(ctx, `SELECT organization_kind FROM organizations WHERE id::text=$1`, *org).Scan(&kind); err != nil {
				return err
			}
			if kind == model.OrganizationKindBrandCloud {
				return ErrConflict
			}
		}
	}
	return nil
}

func auditClaimTokenAdministrationTx(ctx context.Context, tx pgx.Tx, event, actor string, token model.DeviceClaimToken) error {
	return createAuditEventTx(ctx, tx, AuditEventInput{EventType: event, ActorUserID: &actor, OrganizationID: token.OrganizationID, SubjectType: "device_claim_token", SubjectID: token.ID, Payload: map[string]any{"device_item_profile_id": token.DeviceItemProfileID}})
}
