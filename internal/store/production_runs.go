package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"rtk_account_manager/internal/model"
)

type ProductionRunCreateInput struct {
	ActorUserID         *string
	PlatformOverride    bool
	BrandCloudID        string
	DeviceItemProfileID string
	FactoryID           string
	BatchID             string
	AllowedQuantity     int
	ValidFrom           time.Time
	ValidUntil          time.Time
	Now                 time.Time
}

func (s *Store) ListProductionRuns(ctx context.Context, brandCloudID, profileID string, limit, offset int) (ProductionRunPage, error) {
	var total int
	if err := s.db.QueryRow(ctx, `SELECT count(*)::int FROM factory_production_runs WHERE brand_cloud_id = $1 AND ($2 = '' OR device_item_profile_id::text = $2)`, brandCloudID, strings.TrimSpace(profileID)).Scan(&total); err != nil {
		return ProductionRunPage{}, err
	}
	rows, err := s.db.Query(ctx, `
		SELECT id::text, brand_cloud_id::text, device_item_profile_id::text, factory_id, batch_id,
			status, allowed_quantity, issued_quantity, valid_from, valid_until, created_by::text, created_at, updated_at
		FROM factory_production_runs
		WHERE brand_cloud_id = $1 AND ($2 = '' OR device_item_profile_id::text = $2)
		ORDER BY created_at DESC, id DESC LIMIT $3 OFFSET $4
	`, brandCloudID, strings.TrimSpace(profileID), limit, offset)
	if err != nil {
		return ProductionRunPage{}, err
	}
	defer rows.Close()
	runs := []model.ProductionRun{}
	for rows.Next() {
		run, err := scanProductionRun(rows)
		if err != nil {
			return ProductionRunPage{}, err
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return ProductionRunPage{}, err
	}
	return ProductionRunPage{Runs: runs, Page: Page{Limit: limit, Offset: offset, Total: total}}, nil
}

// Bootstrap/fixture persistence. HTTP uses IssueProductionRunAsUser.
func (s *Store) CreateProductionRun(ctx context.Context, in ProductionRunCreateInput) (model.ProductionRun, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.ProductionRun{}, err
	}
	defer tx.Rollback(ctx)
	run, _, err := createProductionRunTx(ctx, tx, in)
	if err != nil {
		return model.ProductionRun{}, err
	}
	return run, tx.Commit(ctx)
}

var ErrProductionRunSigning = errors.New("production JWT signing failed")

// The issuer must perform only bounded, in-process signing: no network or DB
// calls. A signed token is returned only after its run and audit commit.
type ProductionRunIssuer func(model.ProductionRun, model.DeviceItemProfile) (string, error)

func (s *Store) IssueProductionRunAsUser(ctx context.Context, in ProductionRunCreateInput, issue ProductionRunIssuer) (model.ProductionRun, string, error) {
	if issue == nil {
		return model.ProductionRun{}, "", ErrProductionRunSigning
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.ProductionRun{}, "", err
	}
	defer tx.Rollback(ctx)
	if err := authorizeProductUserMutationTx(ctx, tx, stringValue(in.ActorUserID), in.BrandCloudID, in.DeviceItemProfileID, in.PlatformOverride); err != nil {
		return model.ProductionRun{}, "", err
	}
	run, profile, err := createProductionRunTx(ctx, tx, in)
	if err != nil {
		return model.ProductionRun{}, "", err
	}
	token, err := issue(run, profile)
	if err != nil || strings.TrimSpace(token) == "" {
		return model.ProductionRun{}, "", ErrProductionRunSigning
	}
	if err := tx.Commit(ctx); err != nil {
		return model.ProductionRun{}, "", err
	}
	return run, token, nil
}

func createProductionRunTx(ctx context.Context, tx pgx.Tx, in ProductionRunCreateInput) (model.ProductionRun, model.DeviceItemProfile, error) {
	if err := validateProductionRunCreate(in); err != nil {
		return model.ProductionRun{}, model.DeviceItemProfile{}, err
	}
	if err := ensureBrandCloud(ctx, tx, in.BrandCloudID); err != nil {
		return model.ProductionRun{}, model.DeviceItemProfile{}, err
	}
	profile, err := getDeviceItemProfile(ctx, tx, in.BrandCloudID, in.DeviceItemProfileID, true)
	if err != nil {
		return model.ProductionRun{}, model.DeviceItemProfile{}, err
	}
	if profile.Status == model.DeviceItemProfileStatusDisabled {
		return model.ProductionRun{}, model.DeviceItemProfile{}, ErrDeviceItemProfileDisabled
	}

	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	run, err := scanProductionRun(tx.QueryRow(ctx, `
		INSERT INTO factory_production_runs (
			brand_cloud_id, device_item_profile_id, factory_id, batch_id,
			status, allowed_quantity, valid_from, valid_until, created_by, created_at, updated_at,
			authorization_ownership_version, authorization_platform_override
		)
		VALUES ($1, $2, $3, $4, 'active', $5, $6, $7, $8, $9, $9,
			(SELECT ownership_version FROM organizations WHERE id=$1), $10)
		RETURNING id::text, brand_cloud_id::text, device_item_profile_id::text, factory_id, batch_id,
			status, allowed_quantity, issued_quantity, valid_from, valid_until, created_by::text, created_at, updated_at
	`, in.BrandCloudID, in.DeviceItemProfileID, strings.TrimSpace(in.FactoryID), strings.TrimSpace(in.BatchID),
		in.AllowedQuantity, in.ValidFrom.UTC(), in.ValidUntil.UTC(), in.ActorUserID, now, in.PlatformOverride))
	if err != nil {
		return model.ProductionRun{}, model.DeviceItemProfile{}, err
	}

	if err := createAuditEventTx(ctx, tx, AuditEventInput{
		EventType:      "factory_production_run_created",
		ActorUserID:    in.ActorUserID,
		OrganizationID: &in.BrandCloudID,
		SubjectType:    "factory_production_run",
		SubjectID:      run.ID,
		Payload: map[string]any{
			"brand_cloud_id":         run.BrandCloudID,
			"device_item_profile_id": run.DeviceItemProfileID,
			"factory_id":             run.FactoryID,
			"batch_id":               run.BatchID,
			"allowed_quantity":       run.AllowedQuantity,
			"valid_from":             run.ValidFrom,
			"valid_until":            run.ValidUntil,
		},
	}); err != nil {
		return model.ProductionRun{}, model.DeviceItemProfile{}, err
	}
	return run, profile, nil
}

func validateProductionRunCreate(in ProductionRunCreateInput) error {
	if strings.TrimSpace(in.BrandCloudID) == "" || strings.TrimSpace(in.DeviceItemProfileID) == "" {
		return ErrNotFound
	}
	if in.AllowedQuantity <= 0 {
		return ErrConflict
	}
	if in.ValidFrom.IsZero() || in.ValidUntil.IsZero() || !in.ValidUntil.After(in.ValidFrom) {
		return ErrConflict
	}
	return nil
}

func scanProductionRun(row rowScanner) (model.ProductionRun, error) {
	var run model.ProductionRun
	if err := row.Scan(
		&run.ID,
		&run.BrandCloudID,
		&run.DeviceItemProfileID,
		&run.FactoryID,
		&run.BatchID,
		&run.Status,
		&run.AllowedQuantity,
		&run.IssuedQuantity,
		&run.ValidFrom,
		&run.ValidUntil,
		&run.CreatedBy,
		&run.CreatedAt,
		&run.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.ProductionRun{}, ErrNotFound
		}
		return model.ProductionRun{}, err
	}
	return run, nil
}
