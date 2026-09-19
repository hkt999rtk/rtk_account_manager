package store

import (
	"context"
	"github.com/jackc/pgx/v5"
)

type DevicePKIJob struct {
	OperationID string
	CloudID     string
	ProductID   string
	LeaseID     string
}

type DevicePKIReceipt struct {
	Status      string `json:"pki_status"`
	OperationID string `json:"operation_id"`
	IssuerID    string `json:"issuer_id,omitempty"`
}

func (r DevicePKIReceipt) Validate() error {
	if r.Status != "pending" && r.Status != "ready" && r.Status != "failed" && r.Status != "cancelled" {
		return ErrConflict
	}
	if r.Status == "ready" && (!billingCreationUUID(r.IssuerID) || !billingCreationUUID(r.OperationID)) {
		return ErrConflict
	}
	if r.Status != "ready" && r.IssuerID != "" {
		return ErrConflict
	}
	if r.OperationID != "" && !billingCreationUUID(r.OperationID) {
		return ErrConflict
	}
	return nil
}

func (s *Store) ClaimDevicePKI(ctx context.Context) (DevicePKIJob, error) {
	var j DevicePKIJob
	err := s.db.QueryRow(ctx, `WITH candidate AS (
 SELECT operation_id FROM device_pki_outbox WHERE status='pending' AND available_at<=clock_timestamp()
 AND (lease_until IS NULL OR lease_until<clock_timestamp()) ORDER BY available_at,product_id LIMIT 1 FOR UPDATE SKIP LOCKED
 ) UPDATE device_pki_outbox j SET lease_id=gen_random_uuid(),lease_until=clock_timestamp()+interval '2 minutes',attempts=LEAST(attempts+1,1000000)
 FROM candidate c WHERE j.operation_id=c.operation_id RETURNING j.operation_id::text,j.cloud_id::text,j.product_id,j.lease_id::text`).Scan(&j.OperationID, &j.CloudID, &j.ProductID, &j.LeaseID)
	if err == pgx.ErrNoRows {
		return j, ErrNotFound
	}
	return j, err
}

func (s *Store) DevicePKIContextActive(ctx context.Context, j DevicePKIJob) (bool, error) {
	var active bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organizations o WHERE o.id=$1 AND o.organization_kind='brand_cloud' AND o.status='active' AND o.deleted_at IS NULL
 AND NOT EXISTS(SELECT 1 FROM cloud_deletion_operations d WHERE d.brand_cloud_id=o.id AND d.phase<>'canceled')
 AND ($2='' OR EXISTS(SELECT 1 FROM device_item_profiles p WHERE p.id::text=$2 AND p.brand_cloud_id=o.id AND p.status='active' AND p.disabled_at IS NULL)))`, j.CloudID, j.ProductID).Scan(&active)
	return active, err
}

// Serialize remote provisioning with Cloud deletion, ownership mutations and
// Product disablement. A signed active-context assertion cannot outlive the
// locked business snapshot while its HTTP request is in flight.
func (s *Store) WithDevicePKIContext(ctx context.Context, j DevicePKIJob, run func() error) (bool, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var active bool
	err = tx.QueryRow(ctx, `SELECT status='active' AND deleted_at IS NULL AND NOT EXISTS(SELECT 1 FROM cloud_deletion_operations d WHERE d.brand_cloud_id=organizations.id AND d.phase<>'canceled') FROM organizations WHERE id=$1 FOR UPDATE`, j.CloudID).Scan(&active)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !active {
		return false, nil
	}
	if j.ProductID != "" {
		err = tx.QueryRow(ctx, `SELECT status='active' AND disabled_at IS NULL FROM device_item_profiles WHERE id::text=$1 AND brand_cloud_id=$2 FOR UPDATE`, j.ProductID, j.CloudID).Scan(&active)
		if err == pgx.ErrNoRows {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !active {
			return false, nil
		}
	}
	if err = run(); err != nil {
		return true, err
	}
	return true, tx.Commit(ctx)
}

// Receipt is accepted only from the authenticated controller; fence late lease
// holders and recheck current business state before publishing ready to users.
func (s *Store) FinishDevicePKI(ctx context.Context, j DevicePKIJob, r DevicePKIReceipt, code string) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if code != "" && code != "provider_unavailable" && code != "context_inactive" && code != "reconciliation_required" {
		return ErrConflict
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var active bool
	err = tx.QueryRow(ctx, `SELECT status='active' AND deleted_at IS NULL AND NOT EXISTS(SELECT 1 FROM cloud_deletion_operations d WHERE d.brand_cloud_id=organizations.id AND d.phase<>'canceled') FROM organizations WHERE id=$1 FOR UPDATE`, j.CloudID).Scan(&active)
	if err != nil && err != pgx.ErrNoRows {
		return err
	}
	if j.ProductID != "" && active {
		err = tx.QueryRow(ctx, `SELECT status='active' AND disabled_at IS NULL FROM device_item_profiles WHERE id::text=$1 AND brand_cloud_id=$2 FOR UPDATE`, j.ProductID, j.CloudID).Scan(&active)
		if err != nil && err != pgx.ErrNoRows {
			return err
		}
	}
	if err == pgx.ErrNoRows || !active {
		r.Status, r.IssuerID, code = "cancelled", "", "context_inactive"
	}
	tag, err := tx.Exec(ctx, `UPDATE device_pki_outbox SET status=$3,issuer_id=NULLIF($4,'')::uuid,controller_operation_id=COALESCE(NULLIF($5,'')::uuid,controller_operation_id),error_code=$6,
 available_at=clock_timestamp()+interval '15 seconds',lease_id=NULL,lease_until=NULL WHERE operation_id=$1 AND lease_id::text=$2 AND lease_until>clock_timestamp() AND status='pending'`, j.OperationID, j.LeaseID, r.Status, r.IssuerID, r.OperationID, code)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	if j.ProductID == "" {
		_, err = tx.Exec(ctx, `UPDATE organizations SET pki_status=$2,pki_issuer_id=NULLIF($3,'')::uuid WHERE id=$1 AND pki_operation_id=$4 AND deleted_at IS NULL`, j.CloudID, r.Status, r.IssuerID, j.OperationID)
	} else {
		_, err = tx.Exec(ctx, `UPDATE device_item_profiles SET pki_status=$2,pki_issuer_id=NULLIF($3,'')::uuid WHERE id::text=$1 AND pki_operation_id=$4`, j.ProductID, r.Status, r.IssuerID, j.OperationID)
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
