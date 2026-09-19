package store

import "context"

type DevicePKIRequeueTarget struct {
	OperationID string `json:"operation_id"`
	ProductID   string `json:"product_id,omitempty"`
	IssuerID    string `json:"previous_issuer_id,omitempty"`
}

// RequeueDevicePKI is an explicit non-production maintenance action, not a
// startup migration. The operator must first pin and verify the replacement
// Device Root in the controller and fence enrollment/writers. Business IDs and
// records are preserved; cancelled/deleting/disabled scopes are never revived.
func (s *Store) RequeueDevicePKI(ctx context.Context, environment, cloudID, replacementRootID string, apply bool) ([]DevicePKIRequeueTarget, error) {
	if (environment != "dev" && environment != "staging" && environment != "test" && environment != "local") || !billingCreationUUID(cloudID) || !billingCreationUUID(replacementRootID) {
		return nil, ErrConflict
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var active bool
	err = tx.QueryRow(ctx, `SELECT organization_kind='brand_cloud' AND status='active' AND deleted_at IS NULL AND NOT EXISTS(SELECT 1 FROM cloud_deletion_operations d WHERE d.brand_cloud_id=organizations.id AND d.phase<>'canceled') FROM organizations WHERE id=$1 FOR UPDATE`, cloudID).Scan(&active)
	if err != nil {
		return nil, err
	}
	if !active {
		return nil, ErrConflict
	}
	// Acquire business rows before outbox rows, in the worker's lock order.
	if _, err = tx.Exec(ctx, `SELECT id FROM device_item_profiles WHERE brand_cloud_id=$1 ORDER BY id FOR UPDATE`, cloudID); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT operation_id::text,product_id,COALESCE(issuer_id::text,'') FROM device_pki_outbox j WHERE cloud_id=$1 AND status IN ('ready','failed') AND (product_id='' OR EXISTS(SELECT 1 FROM device_item_profiles p WHERE p.id::text=j.product_id AND p.brand_cloud_id=j.cloud_id AND p.status='active' AND p.disabled_at IS NULL)) ORDER BY product_id FOR UPDATE`, cloudID)
	if err != nil {
		return nil, err
	}
	targets := []DevicePKIRequeueTarget{}
	for rows.Next() {
		var target DevicePKIRequeueTarget
		if err = rows.Scan(&target.OperationID, &target.ProductID, &target.IssuerID); err != nil {
			rows.Close()
			return nil, err
		}
		targets = append(targets, target)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || !apply {
		return targets, err
	}
	for _, target := range targets {
		var operationID string
		err = tx.QueryRow(ctx, `UPDATE device_pki_outbox SET operation_id=gen_random_uuid(),status='pending',issuer_id=NULL,controller_operation_id=NULL,attempts=0,available_at=clock_timestamp(),lease_id=NULL,lease_until=NULL,error_code='' WHERE operation_id=$1 RETURNING operation_id::text`, target.OperationID).Scan(&operationID)
		if err != nil {
			return nil, err
		}
		if target.ProductID == "" {
			_, err = tx.Exec(ctx, `UPDATE organizations SET pki_status='pending',pki_operation_id=$2,pki_issuer_id=NULL WHERE id=$1`, cloudID, operationID)
		} else {
			_, err = tx.Exec(ctx, `UPDATE device_item_profiles SET pki_status='pending',pki_operation_id=$2,pki_issuer_id=NULL WHERE id::text=$1`, target.ProductID, operationID)
		}
		if err != nil {
			return nil, err
		}
	}
	if len(targets) > 0 {
		if err = createAuditEventTx(ctx, tx, AuditEventInput{EventType: "nonproduction_device_pki_requeued", OrganizationID: &cloudID, SubjectType: "brand_cloud", SubjectID: cloudID, Payload: map[string]any{"environment": environment, "replacement_root_id": replacementRootID, "previous_jobs": targets}}); err != nil {
			return nil, err
		}
	}
	return targets, tx.Commit(ctx)
}
