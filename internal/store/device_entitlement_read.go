package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"rtk_account_manager/internal/channel"
	"rtk_account_manager/internal/model"
)

// GetDeviceEntitlementReadModel distinguishes accepted snapshots from grants
// acknowledged by Video Cloud. It does not treat an outbox 202 as applied.
func (s *Store) GetDeviceEntitlementReadModel(ctx context.Context, cloudID, deviceID string) (*int64, *int64, *model.DeviceEntitlementResult, error) {
	var applied, pending *int64
	var result *model.DeviceEntitlementResult
	var revision int64
	var operationID, status, errorCode string
	var retryable bool
	err := s.db.QueryRow(ctx, `SELECT s.product_service_revision,s.operation_id,o.status,COALESCE(o.error_code,''),COALESCE(o.retryable,false)
		FROM device_entitlement_snapshots s JOIN device_operations o ON o.operation_id=s.operation_id
		WHERE s.organization_id=$1 AND s.account_device_id=$2 ORDER BY s.revision DESC LIMIT 1`, cloudID, deviceID).
		Scan(&revision, &operationID, &status, &errorCode, &retryable)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil, err
	}
	if err == nil {
		result = &model.DeviceEntitlementResult{OperationID: operationID, Status: status, ErrorCode: errorCode, Retryable: retryable}
		if status != "succeeded" && status != "failed" && status != "dead_lettered" {
			pending = &revision
		}
	}
	var confirmed int64
	err = s.db.QueryRow(ctx, `SELECT s.product_service_revision FROM device_entitlement_snapshots s
		JOIN device_operations o ON o.operation_id=s.operation_id
		WHERE s.organization_id=$1 AND s.account_device_id=$2 AND o.status='succeeded'
		AND o.result_payload->>'platform_entitlement_revision'=s.revision::text
		ORDER BY s.revision DESC LIMIT 1`, cloudID, deviceID).Scan(&confirmed)
	if err == nil {
		applied = &confirmed
		return applied, pending, result, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil, err
	}
	var raw []byte
	err = s.db.QueryRow(ctx, `SELECT m.payload FROM device_operations o JOIN device_message_outbox m ON m.operation_id=o.operation_id
		WHERE o.organization_id=$1 AND o.device_id=$2 AND o.operation_type='provision' AND o.status='succeeded'
		AND m.message_type='DeviceProvisionRequested' ORDER BY o.completed_at DESC NULLS LAST,o.created_at DESC LIMIT 1`, cloudID, deviceID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, pending, result, nil
	}
	if err != nil {
		return nil, nil, nil, err
	}
	var provision channel.DeviceProvisionRequestedPayload
	if json.Unmarshal(raw, &provision) != nil || provision.ProductServiceRevision == nil ||
		provision.OrgID != cloudID || provision.AccountDeviceID != deviceID {
		return nil, pending, result, nil
	}
	return provision.ProductServiceRevision, pending, result, nil
}
