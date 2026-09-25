package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

type LabDeviceRetirement struct {
	DeviceID    string `json:"device_id"`
	OperationID string `json:"operation_id"`
	Status      string `json:"status"`
}

// BeginLabDeviceRetirement closes every Test Lab route to the device before
// asking Video Cloud to invalidate its credentials. A failed remote step can be
// retried without ever restoring the local access that was revoked here.
func (s *Store) BeginLabDeviceRetirement(ctx context.Context, actor, cloud, product, device, operation string) (LabDeviceRetirement, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return LabDeviceRetirement{}, err
	}
	defer tx.Rollback(ctx)
	if err = authorizeDeviceUserMutationTx(ctx, tx, actor, cloud, device, "registry_device.manage"); err != nil {
		return LabDeviceRetirement{}, err
	}
	var result LabDeviceRetirement
	result.DeviceID = device
	err = tx.QueryRow(ctx, `SELECT operation_id::text,status FROM test_lab_device_retirements WHERE device_id::text=$1 AND brand_cloud_id::text=$2 AND product_id::text=$3 FOR UPDATE`, device, cloud, product).Scan(&result.OperationID, &result.Status)
	if err == nil {
		if result.OperationID != operation {
			return LabDeviceRetirement{}, ErrConflict
		}
		if result.Status == "failed" {
			if _, err = tx.Exec(ctx, `UPDATE test_lab_device_retirements SET status='pending',failure_code=NULL,updated_at=now() WHERE device_id::text=$1`, device); err != nil {
				return LabDeviceRetirement{}, err
			}
			result.Status = "pending"
		}
		return result, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return LabDeviceRetirement{}, err
	}
	d, err := labDeviceTx(ctx, tx, actor, cloud, product, device)
	if err != nil {
		return LabDeviceRetirement{}, err
	}
	if d.Metadata["video_cloud_activation_status"] == "pending" {
		return LabDeviceRetirement{}, ErrConflict
	}
	if _, err = tx.Exec(ctx, `INSERT INTO test_lab_device_retirements(device_id,brand_cloud_id,product_id,operation_id,requested_by,status) VALUES($1,$2,$3,$4,$5,'pending')`, device, cloud, product, operation, actor); err != nil {
		return LabDeviceRetirement{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE devices SET status='disabled',disabled_at=COALESCE(disabled_at,now()),updated_at=now() WHERE id::text=$1 AND organization_id::text=$2`, device, cloud); err != nil {
		return LabDeviceRetirement{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE device_user_bindings SET disabled_at=COALESCE(disabled_at,now()),updated_at=now() WHERE device_id::text=$1 AND disabled_at IS NULL`, device); err != nil {
		return LabDeviceRetirement{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE test_lab_sessions SET revoked_at=now() WHERE device_id::text=$1 AND revoked_at IS NULL`, device); err != nil {
		return LabDeviceRetirement{}, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM test_lab_bind_grants WHERE device_id::text=$1`, device); err != nil {
		return LabDeviceRetirement{}, err
	}
	if err = createAuditEventTx(ctx, tx, AuditEventInput{ActorUserID: &actor, OrganizationID: &cloud, EventType: "test_lab.device_retirement_requested", SubjectType: "device", SubjectID: device, Payload: map[string]any{"operation_id": operation, "product_id": product}}); err != nil {
		return LabDeviceRetirement{}, err
	}
	result.OperationID, result.Status = operation, "pending"
	return result, tx.Commit(ctx)
}

func (s *Store) FinishLabDeviceRetirement(ctx context.Context, actor, cloud, product, device, operation string, completed bool) (LabDeviceRetirement, error) {
	status := "failed"
	if completed {
		status = "completed"
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return LabDeviceRetirement{}, err
	}
	defer tx.Rollback(ctx)
	var result LabDeviceRetirement
	err = tx.QueryRow(ctx, `UPDATE test_lab_device_retirements SET status=CASE WHEN status='completed' THEN status ELSE $5 END,failure_code=CASE WHEN status='completed' OR $5='completed' THEN NULL ELSE 'runtime_unavailable' END,updated_at=now() WHERE device_id::text=$1 AND brand_cloud_id::text=$2 AND product_id::text=$3 AND operation_id::text=$4 RETURNING device_id::text,operation_id::text,status`, device, cloud, product, operation, status).Scan(&result.DeviceID, &result.OperationID, &result.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return LabDeviceRetirement{}, ErrNotFound
	}
	if err != nil {
		return LabDeviceRetirement{}, err
	}
	if err = createAuditEventTx(ctx, tx, AuditEventInput{ActorUserID: &actor, OrganizationID: &cloud, EventType: "test_lab.device_retirement_" + result.Status, SubjectType: "device", SubjectID: device, Payload: map[string]any{"operation_id": operation}}); err != nil {
		return LabDeviceRetirement{}, err
	}
	return result, tx.Commit(ctx)
}
