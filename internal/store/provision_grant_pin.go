package store

import (
	"context"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"rtk_account_manager/internal/model"
)

type provisionGrantPin struct {
	revision int64
	digest   string
}

// Resolve historical authority from immutable Claim Token, issued factory
// admission, and successful lifecycle records. Device metadata is user-editable
// and therefore cannot pin a grant by itself.
func trustedProvisionGrantPinTx(ctx context.Context, tx pgx.Tx, device model.Device, outbox map[string]any) (*provisionGrantPin, error) {
	devid, ok := outbox["video_cloud_devid"].(string)
	if !ok || strings.TrimSpace(devid) == "" {
		return nil, ErrConflict
	}
	var pin *provisionGrantPin
	read := func(rows pgx.Rows) error {
		defer rows.Close()
		for rows.Next() {
			var product, revisionText, digest *string
			if err := rows.Scan(&product, &revisionText, &digest); err != nil {
				return err
			}
			if product == nil || revisionText == nil || digest == nil || *product != *device.DeviceItemProfileID || !handoffEvidenceDigest.MatchString(*digest) {
				return ErrConflict
			}
			revision, err := strconv.ParseInt(*revisionText, 10, 64)
			if err != nil || revision < 1 {
				return ErrConflict
			}
			if pin == nil {
				pin = &provisionGrantPin{revision: revision, digest: *digest}
			} else if pin.revision != revision || pin.digest != *digest {
				return ErrConflict
			}
		}
		return rows.Err()
	}
	rows, err := tx.Query(ctx, `SELECT CASE WHEN t.video_cloud_devid=$3 THEN t.device_item_profile_id::text END,
		t.metadata->>'product_service_revision',t.metadata->>'service_grant_sha256'
		FROM device_claims c JOIN device_claim_tokens t ON t.id=c.claim_token_id
		WHERE c.device_id=$1 AND c.organization_id=$2`, device.ID, device.OrganizationID, devid)
	if err != nil {
		return nil, err
	}
	if err := read(rows); err != nil {
		return nil, err
	}
	rows, err = tx.Query(ctx, `SELECT r.device_item_profile_id::text,r.product_service_revision::text,g.snapshot_sha256
		FROM factory_enrollment_reservations f JOIN factory_production_runs r ON r.id=f.production_run_id
		LEFT JOIN product_service_grants g ON g.product_id=r.device_item_profile_id
			AND g.brand_cloud_id=r.brand_cloud_id AND g.revision=r.product_service_revision
		WHERE f.device_id=$1 AND f.status='issued' AND r.brand_cloud_id=$2`, devid, device.OrganizationID)
	if err != nil {
		return nil, err
	}
	if err := read(rows); err != nil {
		return nil, err
	}
	rows, err = tx.Query(ctx, `SELECT CASE WHEN m.payload->>'video_cloud_devid'=$3 THEN m.payload->>'product_id' END,
		m.payload->>'product_service_revision',m.payload->>'service_grant_sha256'
		FROM device_operations o JOIN device_message_outbox m ON m.operation_id=o.operation_id
		WHERE o.device_id=$1 AND o.organization_id=$2 AND o.operation_type='provision' AND o.status='succeeded'`, device.ID, device.OrganizationID, devid)
	if err != nil {
		return nil, err
	}
	if err := read(rows); err != nil {
		return nil, err
	}
	return pin, nil
}
