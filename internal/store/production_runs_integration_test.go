package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"rtk_account_manager/internal/model"
)

type failingProductionRunRow struct {
	err error
}

func (r failingProductionRunRow) Scan(...any) error {
	return r.err
}

func TestScanProductionRunMapsNoRowsToNotFound(t *testing.T) {
	_, err := scanProductionRun(failingProductionRunRow{err: pgx.ErrNoRows})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestScanProductionRunReturnsScanError(t *testing.T) {
	scanErr := errors.New("scan failed")
	_, err := scanProductionRun(failingProductionRunRow{err: scanErr})
	if !errors.Is(err, scanErr) {
		t.Fatalf("expected scan error, got %v", err)
	}
}

func TestValidateProductionRunCreateRejectsInvalidInput(t *testing.T) {
	validFrom := time.Now().UTC()
	valid := ProductionRunCreateInput{
		BrandCloudID:        "brand-cloud-id",
		DeviceItemProfileID: "profile-id",
		AllowedQuantity:     1,
		ValidFrom:           validFrom,
		ValidUntil:          validFrom.Add(time.Hour),
	}

	tests := []struct {
		name string
		in   ProductionRunCreateInput
		want error
	}{
		{
			name: "missing brand cloud",
			in: ProductionRunCreateInput{
				DeviceItemProfileID: valid.DeviceItemProfileID,
				AllowedQuantity:     valid.AllowedQuantity,
				ValidFrom:           valid.ValidFrom,
				ValidUntil:          valid.ValidUntil,
			},
			want: ErrNotFound,
		},
		{
			name: "missing profile",
			in: ProductionRunCreateInput{
				BrandCloudID:    valid.BrandCloudID,
				AllowedQuantity: valid.AllowedQuantity,
				ValidFrom:       valid.ValidFrom,
				ValidUntil:      valid.ValidUntil,
			},
			want: ErrNotFound,
		},
		{
			name: "non-positive allowed quantity",
			in: ProductionRunCreateInput{
				BrandCloudID:        valid.BrandCloudID,
				DeviceItemProfileID: valid.DeviceItemProfileID,
				AllowedQuantity:     0,
				ValidFrom:           valid.ValidFrom,
				ValidUntil:          valid.ValidUntil,
			},
			want: ErrConflict,
		},
		{
			name: "zero valid from",
			in: ProductionRunCreateInput{
				BrandCloudID:        valid.BrandCloudID,
				DeviceItemProfileID: valid.DeviceItemProfileID,
				AllowedQuantity:     valid.AllowedQuantity,
				ValidUntil:          valid.ValidUntil,
			},
			want: ErrConflict,
		},
		{
			name: "valid until before valid from",
			in: ProductionRunCreateInput{
				BrandCloudID:        valid.BrandCloudID,
				DeviceItemProfileID: valid.DeviceItemProfileID,
				AllowedQuantity:     valid.AllowedQuantity,
				ValidFrom:           valid.ValidUntil,
				ValidUntil:          valid.ValidFrom,
			},
			want: ErrConflict,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateProductionRunCreate(tt.in); !errors.Is(err, tt.want) {
				t.Fatalf("expected %v, got %v", tt.want, err)
			}
		})
	}
}

func TestCreateProductionRunBindsBrandCloudAndProfile(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()

	admin, err := env.store.Register(ctx, RegisterInput{
		Email:            "production-run-admin@example.com",
		PasswordHash:     "hash",
		OrganizationName: "Production Run Admin Org",
	})
	if err != nil {
		t.Fatal(err)
	}
	brand, err := createVerifiedOwnedCloudForTest(t, env, admin.User.ID, BrandCloudInput{Name: "Production Brand"})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := env.store.CreateDeviceItemProfile(ctx, DeviceItemProfileCreateInput{
		ActorUserID:    &admin.User.ID,
		BrandCloudID:   brand.ID,
		ProfileKey:     "cam-prod-v1",
		DisplayName:    "Camera Production V1",
		Category:       model.DeviceCategoryIPCamera,
		CAProfile:      "product-ca-prod-v1",
		IssuerProfile:  "factory-line-a",
		ServiceOptions: []string{"video_streaming"},
	})
	if err != nil {
		t.Fatal(err)
	}

	validFrom := time.Now().UTC().Truncate(time.Microsecond)
	run, err := env.store.CreateProductionRun(ctx, ProductionRunCreateInput{
		ActorUserID:         &admin.User.ID,
		BrandCloudID:        brand.ID,
		DeviceItemProfileID: profile.ID,
		FactoryID:           "factory-a",
		BatchID:             "batch-20260617",
		AllowedQuantity:     500,
		ValidFrom:           validFrom,
		ValidUntil:          validFrom.Add(48 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.BrandCloudID != brand.ID || run.DeviceItemProfileID != profile.ID || run.AllowedQuantity != 500 || run.Status != model.ProductionRunStatusActive {
		t.Fatalf("unexpected production run: %+v", run)
	}

	events, err := env.store.ListAuditEvents(ctx, AuditEventListFilter{SubjectType: "factory_production_run", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if events.Page.Total != 1 || events.Events[0].SubjectID != run.ID {
		t.Fatalf("expected production run audit event, got %+v", events)
	}
}

func TestCreateProductionRunRejectsDisabledOrCrossBrandProfile(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()

	admin, err := env.store.Register(ctx, RegisterInput{
		Email:            "production-run-reject-admin@example.com",
		PasswordHash:     "hash",
		OrganizationName: "Production Run Reject Admin Org",
	})
	if err != nil {
		t.Fatal(err)
	}
	brand, err := createVerifiedOwnedCloudForTest(t, env, admin.User.ID, BrandCloudInput{Name: "Production Reject Brand"})
	if err != nil {
		t.Fatal(err)
	}
	otherBrand, err := createVerifiedOwnedCloudForTest(t, env, admin.User.ID, BrandCloudInput{Name: "Production Reject Other Brand"})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := env.store.CreateDeviceItemProfile(ctx, DeviceItemProfileCreateInput{
		ActorUserID:    &admin.User.ID,
		BrandCloudID:   brand.ID,
		ProfileKey:     "cam-disabled",
		DisplayName:    "Disabled Camera",
		Category:       model.DeviceCategoryIPCamera,
		CAProfile:      "product-ca-disabled",
		IssuerProfile:  "factory-line-a",
		ServiceOptions: []string{"video_streaming"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.DisableDeviceItemProfile(ctx, brand.ID, profile.ID, &admin.User.ID); err != nil {
		t.Fatal(err)
	}

	validFrom := time.Now().UTC()
	_, err = env.store.CreateProductionRun(ctx, ProductionRunCreateInput{
		ActorUserID:         &admin.User.ID,
		BrandCloudID:        brand.ID,
		DeviceItemProfileID: profile.ID,
		AllowedQuantity:     1,
		ValidFrom:           validFrom,
		ValidUntil:          validFrom.Add(time.Hour),
	})
	if !errors.Is(err, ErrDeviceItemProfileDisabled) {
		t.Fatalf("expected disabled profile error, got %v", err)
	}

	_, err = env.store.CreateProductionRun(ctx, ProductionRunCreateInput{
		ActorUserID:         &admin.User.ID,
		BrandCloudID:        otherBrand.ID,
		DeviceItemProfileID: profile.ID,
		AllowedQuantity:     1,
		ValidFrom:           validFrom,
		ValidUntil:          validFrom.Add(time.Hour),
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected cross-brand not found, got %v", err)
	}
}
