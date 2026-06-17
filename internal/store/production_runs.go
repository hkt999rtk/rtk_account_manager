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
	BrandCloudID        string
	DeviceItemProfileID string
	FactoryID           string
	BatchID             string
	AllowedQuantity     int
	ValidFrom           time.Time
	ValidUntil          time.Time
	Now                 time.Time
}

func (s *Store) CreateProductionRun(ctx context.Context, in ProductionRunCreateInput) (model.ProductionRun, error) {
	if err := validateProductionRunCreate(in); err != nil {
		return model.ProductionRun{}, err
	}
	if err := ensureBrandCloud(ctx, s.db, in.BrandCloudID); err != nil {
		return model.ProductionRun{}, err
	}
	profile, err := s.GetDeviceItemProfile(ctx, in.BrandCloudID, in.DeviceItemProfileID)
	if err != nil {
		return model.ProductionRun{}, err
	}
	if profile.Status == model.DeviceItemProfileStatusDisabled {
		return model.ProductionRun{}, ErrDeviceItemProfileDisabled
	}

	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.ProductionRun{}, err
	}
	defer tx.Rollback(ctx)

	run, err := scanProductionRun(tx.QueryRow(ctx, `
		INSERT INTO factory_production_runs (
			brand_cloud_id, device_item_profile_id, factory_id, batch_id,
			status, allowed_quantity, valid_from, valid_until, created_by, created_at, updated_at
		)
		VALUES ($1, $2, $3, $4, 'active', $5, $6, $7, $8, $9, $9)
		RETURNING id::text, brand_cloud_id::text, device_item_profile_id::text, factory_id, batch_id,
			status, allowed_quantity, issued_quantity, valid_from, valid_until, created_by::text, created_at, updated_at
	`, in.BrandCloudID, in.DeviceItemProfileID, strings.TrimSpace(in.FactoryID), strings.TrimSpace(in.BatchID),
		in.AllowedQuantity, in.ValidFrom.UTC(), in.ValidUntil.UTC(), in.ActorUserID, now))
	if err != nil {
		return model.ProductionRun{}, err
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
		return model.ProductionRun{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.ProductionRun{}, err
	}
	return run, nil
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
