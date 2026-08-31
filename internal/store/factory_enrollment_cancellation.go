package store

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

// BeginFactoryEnrollmentCancellation fences delayed admissions even when an
// earlier reserve response was lost. Only the dedicated trusted reconciliation
// service may request this. It is NOT an issuer receipt and releases no quota.
func (s *Store) BeginFactoryEnrollmentCancellation(ctx context.Context, in FactoryEnrollmentAdmission) (FactoryEnrollmentReservation, error) {
	if strings.TrimSpace(in.RequestID) == "" || len(in.RequestID) > 128 || strings.TrimSpace(in.DeviceID) == "" || len(in.DeviceID) > 256 || !handoffEvidenceDigest.MatchString(in.RequestSHA256) {
		return FactoryEnrollmentReservation{}, ErrConflict
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return FactoryEnrollmentReservation{}, err
	}
	defer tx.Rollback(ctx)
	var cloud, run string
	err = tx.QueryRow(ctx, `SELECT id::text FROM organizations WHERE id::text=$1 AND organization_kind='brand_cloud' FOR UPDATE`, in.CloudID).Scan(&cloud)
	if errors.Is(err, pgx.ErrNoRows) {
		return FactoryEnrollmentReservation{}, ErrNotFound
	}
	if err != nil {
		return FactoryEnrollmentReservation{}, err
	}
	// Shares cloud -> run ordering with admission and completion. Do not require
	// current actor authority: cancellation must work after expiry/revocation.
	err = tx.QueryRow(ctx, `SELECT id::text FROM factory_production_runs WHERE id::text=$1 AND brand_cloud_id=$2 AND device_item_profile_id::text=$3 FOR UPDATE`, in.RunID, cloud, in.ProductID).Scan(&run)
	if errors.Is(err, pgx.ErrNoRows) {
		return FactoryEnrollmentReservation{}, ErrNotFound
	}
	if err != nil {
		return FactoryEnrollmentReservation{}, err
	}
	out, err := scanFactoryReservation(tx.QueryRow(ctx, `SELECT `+factoryReservationColumns+` FROM factory_enrollment_reservations WHERE production_run_id=$1 AND request_id=$2 FOR UPDATE`, run, in.RequestID))
	if err == nil {
		if out.DeviceID != in.DeviceID || out.RequestSHA256 != in.RequestSHA256 {
			return FactoryEnrollmentReservation{}, ErrConflict
		}
		if out.Status != "reserved" {
			return out, nil
		}
		out, err = scanFactoryReservation(tx.QueryRow(ctx, `UPDATE factory_enrollment_reservations SET status='cancel_requested' WHERE id=$1 RETURNING `+factoryReservationColumns, out.ID))
	} else if errors.Is(err, ErrNotFound) {
		// No row is not proof of non-admission. Persist a key that any delayed
		// Reserve must encounter; the issuer must subsequently fence this UUID.
		out, err = scanFactoryReservation(tx.QueryRow(ctx, `INSERT INTO factory_enrollment_reservations(production_run_id,request_id,device_id,request_sha256,status,admitted) VALUES($1,$2,$3,$4,'cancel_requested',false) RETURNING `+factoryReservationColumns, run, in.RequestID, in.DeviceID, in.RequestSHA256))
	}
	if err != nil {
		return FactoryEnrollmentReservation{}, err
	}
	if err := auditFactoryReservation(ctx, tx, cloud, out); err != nil {
		return FactoryEnrollmentReservation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return FactoryEnrollmentReservation{}, err
	}
	return out, nil
}
