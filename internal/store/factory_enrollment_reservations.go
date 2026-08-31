package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrProductionRunCapacity = errors.New("production run capacity exhausted")

// Trusted factory adapter input, not human authentication or caller-selected
// authority. The adapter validates the production JWT and canonical body first.
type FactoryEnrollmentAdmission struct {
	RunID, CloudID, ProductID, RequestID, DeviceID, RequestSHA256 string
}

type FactoryEnrollmentReservation struct {
	ID, RunID, RequestID, DeviceID, RequestSHA256, Status string
	EvidenceSHA256                                        *string
	CreatedAt                                             time.Time
	CompletedAt                                           *time.Time
}

const factoryReservationColumns = `id::text,production_run_id::text,request_id,device_id,request_sha256,status,evidence_sha256,created_at,completed_at`

// Lookup is a trusted reconciliation read, not a new issuance authorization.
// It deliberately works after the original JWT expires or a cloud is fenced.
func (s *Store) LookupFactoryEnrollment(ctx context.Context, in FactoryEnrollmentAdmission) (FactoryEnrollmentReservation, error) {
	return scanFactoryReservation(s.db.QueryRow(ctx, `SELECT `+factoryReservationColumns+` FROM factory_enrollment_reservations
		WHERE production_run_id::text=$1 AND request_id=$2 AND device_id=$3 AND request_sha256=$4
		AND EXISTS(SELECT 1 FROM factory_production_runs r WHERE r.id=production_run_id AND r.brand_cloud_id::text=$5 AND r.device_item_profile_id::text=$6)`,
		in.RunID, in.RequestID, in.DeviceID, in.RequestSHA256, in.CloudID, in.ProductID))
}

func scanFactoryReservation(row rowScanner) (FactoryEnrollmentReservation, error) {
	var out FactoryEnrollmentReservation
	err := row.Scan(&out.ID, &out.RunID, &out.RequestID, &out.DeviceID, &out.RequestSHA256, &out.Status, &out.EvidenceSHA256, &out.CreatedAt, &out.CompletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return out, err
}

// ReserveFactoryEnrollment serializes quota with authority and lifecycle
// admission. A replay returns a journal reference, never authority to issue a
// second certificate. Unknown issuer outcomes remain reserved indefinitely.
func (s *Store) ReserveFactoryEnrollment(ctx context.Context, in FactoryEnrollmentAdmission) (FactoryEnrollmentReservation, error) {
	if strings.TrimSpace(in.RequestID) == "" || len(in.RequestID) > 128 || strings.TrimSpace(in.DeviceID) == "" || len(in.DeviceID) > 256 || !handoffEvidenceDigest.MatchString(in.RequestSHA256) {
		return FactoryEnrollmentReservation{}, ErrConflict
	}
	// Discover the immutable signer before taking actor -> cloud -> Product ->
	// run locks. Recheck the signer and authority provenance after all locks.
	var actor string
	var platform bool
	err := s.db.QueryRow(ctx, `SELECT COALESCE(created_by::text,''),authorization_platform_override FROM factory_production_runs
		WHERE id::text=$1 AND brand_cloud_id::text=$2 AND device_item_profile_id::text=$3`, in.RunID, in.CloudID, in.ProductID).Scan(&actor, &platform)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && actor == "") {
		return FactoryEnrollmentReservation{}, ErrNotFound
	}
	if err != nil {
		return FactoryEnrollmentReservation{}, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return FactoryEnrollmentReservation{}, err
	}
	defer tx.Rollback(ctx)
	if err := authorizeProductUserMutationTx(ctx, tx, actor, in.CloudID, in.ProductID, platform); err != nil {
		return FactoryEnrollmentReservation{}, err
	}
	profile, err := getDeviceItemProfile(ctx, tx, in.CloudID, in.ProductID, true)
	if err != nil {
		return FactoryEnrollmentReservation{}, err
	}
	if profile.Status == "disabled" {
		return FactoryEnrollmentReservation{}, ErrDeviceItemProfileDisabled
	}
	var valid bool
	var allowed, issued int
	err = tx.QueryRow(ctx, `SELECT r.status='active' AND r.valid_from<=clock_timestamp() AND r.valid_until>clock_timestamp()
		AND COALESCE(r.authorization_ownership_version=o.ownership_version,false)
		AND COALESCE(r.created_by::text=$4,false) AND r.authorization_platform_override=$5,
		r.allowed_quantity,r.issued_quantity
		FROM factory_production_runs r JOIN organizations o ON o.id=r.brand_cloud_id
		WHERE r.id::text=$1 AND r.brand_cloud_id::text=$2 AND r.device_item_profile_id::text=$3 FOR UPDATE OF r`,
		in.RunID, in.CloudID, in.ProductID, actor, platform).Scan(&valid, &allowed, &issued)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !valid) {
		return FactoryEnrollmentReservation{}, ErrNotFound
	}
	if err != nil {
		return FactoryEnrollmentReservation{}, err
	}
	out, err := scanFactoryReservation(tx.QueryRow(ctx, `SELECT `+factoryReservationColumns+` FROM factory_enrollment_reservations
		WHERE production_run_id=$1 AND request_id=$2`, in.RunID, in.RequestID))
	if err == nil {
		if out.DeviceID != in.DeviceID || out.RequestSHA256 != in.RequestSHA256 {
			return FactoryEnrollmentReservation{}, ErrConflict
		}
		return out, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return FactoryEnrollmentReservation{}, err
	}
	var pending int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM factory_enrollment_reservations WHERE production_run_id=$1 AND status='reserved'`, in.RunID).Scan(&pending); err != nil {
		return FactoryEnrollmentReservation{}, err
	}
	if issued >= allowed || pending >= allowed-issued {
		return FactoryEnrollmentReservation{}, ErrProductionRunCapacity
	}
	out, err = scanFactoryReservation(tx.QueryRow(ctx, `INSERT INTO factory_enrollment_reservations(production_run_id,request_id,device_id,request_sha256)
		VALUES($1,$2,$3,$4) RETURNING `+factoryReservationColumns, in.RunID, in.RequestID, in.DeviceID, in.RequestSHA256))
	if err != nil {
		return FactoryEnrollmentReservation{}, err
	}
	if err := auditFactoryReservation(ctx, tx, in.CloudID, out); err != nil {
		return FactoryEnrollmentReservation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return FactoryEnrollmentReservation{}, err
	}
	return out, nil
}

type FactoryEnrollmentResult struct {
	CloudID, RunID, ReservationID, RequestSHA256, Status, EvidenceSHA256 string
}

// CompleteFactoryEnrollment is for a trusted, durable issuer reconciliation
// adapter. In particular a timeout/HTTP error is NOT a not_issued result. This
// closes previously admitted work even during a fence or after run expiry; it
// cannot grant new issuance authority or reopen a terminal reservation.
func (s *Store) CompleteFactoryEnrollment(ctx context.Context, in FactoryEnrollmentResult) (FactoryEnrollmentReservation, error) {
	if (in.Status != "issued" && in.Status != "not_issued") || !handoffEvidenceDigest.MatchString(in.RequestSHA256) || !handoffEvidenceDigest.MatchString(in.EvidenceSHA256) {
		return FactoryEnrollmentReservation{}, ErrConflict
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return FactoryEnrollmentReservation{}, err
	}
	defer tx.Rollback(ctx)
	// Cloud serialization also prevents a handoff readiness/commit race. This
	// reconciliation path deliberately does not call operational admission.
	var cloud string
	err = tx.QueryRow(ctx, `SELECT id::text FROM organizations WHERE id::text=$1 AND organization_kind='brand_cloud' FOR UPDATE`, in.CloudID).Scan(&cloud)
	if errors.Is(err, pgx.ErrNoRows) {
		return FactoryEnrollmentReservation{}, ErrNotFound
	}
	if err != nil {
		return FactoryEnrollmentReservation{}, err
	}
	var run string
	err = tx.QueryRow(ctx, `SELECT id::text FROM factory_production_runs WHERE id::text=$1 AND brand_cloud_id::text=$2 FOR UPDATE`, in.RunID, cloud).Scan(&run)
	if errors.Is(err, pgx.ErrNoRows) {
		return FactoryEnrollmentReservation{}, ErrNotFound
	}
	if err != nil {
		return FactoryEnrollmentReservation{}, err
	}
	out, err := scanFactoryReservation(tx.QueryRow(ctx, `SELECT `+factoryReservationColumns+` FROM factory_enrollment_reservations
		WHERE id::text=$1 AND production_run_id=$2 FOR UPDATE`, in.ReservationID, run))
	if err != nil {
		return FactoryEnrollmentReservation{}, err
	}
	if out.RequestSHA256 != in.RequestSHA256 {
		return FactoryEnrollmentReservation{}, ErrConflict
	}
	if out.Status != "reserved" {
		if out.Status != in.Status || stringValue(out.EvidenceSHA256) != in.EvidenceSHA256 {
			return FactoryEnrollmentReservation{}, ErrConflict
		}
		return out, nil
	}
	if in.Status == "issued" {
		result, err := tx.Exec(ctx, `UPDATE factory_production_runs SET issued_quantity=issued_quantity+1
			WHERE id=$1 AND issued_quantity<allowed_quantity`, run)
		if err != nil {
			return FactoryEnrollmentReservation{}, err
		}
		if result.RowsAffected() != 1 {
			return FactoryEnrollmentReservation{}, ErrConflict
		}
	}
	out, err = scanFactoryReservation(tx.QueryRow(ctx, `UPDATE factory_enrollment_reservations SET status=$2,evidence_sha256=$3,completed_at=clock_timestamp()
		WHERE id=$1 RETURNING `+factoryReservationColumns, out.ID, in.Status, in.EvidenceSHA256))
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

func auditFactoryReservation(ctx context.Context, tx pgx.Tx, cloud string, out FactoryEnrollmentReservation) error {
	return createAuditEventTx(ctx, tx, AuditEventInput{EventType: "factory_enrollment_" + out.Status,
		OrganizationID: &cloud, SubjectType: "factory_enrollment_reservation", SubjectID: out.ID,
		Payload: map[string]any{"production_run_id": out.RunID, "request_sha256": out.RequestSHA256, "status": out.Status, "evidence_sha256": out.EvidenceSHA256}})
}
