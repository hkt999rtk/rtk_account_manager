package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const identityCorrectionMigration = "051_identity_activation_correction.sql"

type IdentityCorrectionReport struct {
	Migration               string          `json:"migration"`
	Ready                   bool            `json:"ready"`
	Reason                  string          `json:"reason,omitempty"`
	BlockingIDs             json.RawMessage `json:"blocking_ids,omitempty"`
	CandidateUsers          int             `json:"candidate_users"`
	ActivationHoldsBefore   int             `json:"activation_holds_before"`
	ActivationHoldsAfter    int             `json:"activation_holds_after"`
	RefreshTokensToRevoke   int             `json:"refresh_tokens_to_revoke"`
	AppCertificatesToRevoke int             `json:"app_certificates_to_revoke"`
	IneligibleOwnerClouds   int             `json:"ineligible_owner_clouds"`
	RolledBack              bool            `json:"rolled_back"`
}

// PreflightIdentityCorrection executes the exact forward migration on a
// restored, write-frozen database and ALWAYS rolls its transaction back. It
// deliberately does not call Migrate, which commits migration files.
func PreflightIdentityCorrection(ctx context.Context, pool *pgxpool.Pool) (report IdentityCorrectionReport, err error) {
	report.Migration = identityCorrectionMigration
	dir, err := findMigrationDir()
	if err != nil {
		return report, err
	}
	sql, err := os.ReadFile(filepath.Join(dir, identityCorrectionMigration))
	if err != nil {
		return report, err
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return report, err
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if rollbackErr := tx.Rollback(rollbackCtx); rollbackErr != nil {
			report.Ready = false
			if err == nil {
				err = fmt.Errorf("roll back identity preflight: %w", rollbackErr)
			}
			return
		}
		report.RolledBack = true
	}()
	var prepared, holdsExist bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version='050_backfill_immediate_brand_account_acl.sql'),
		to_regclass('organization_member_activation_holds') IS NOT NULL`).Scan(&prepared, &holdsExist); err != nil {
		return report, err
	}
	if !prepared {
		return report, errors.New("identity preflight requires a restored database migrated through 050")
	}
	if holdsExist {
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM organization_member_activation_holds`).Scan(&report.ActivationHoldsBefore); err != nil {
			return report, err
		}
	}
	var refreshBefore, certificatesBefore int
	if err = tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM refresh_tokens WHERE revoked_at IS NULL),
		(SELECT count(*) FROM app_certificates WHERE subject_type='user' AND revoked_at IS NULL)`).Scan(&refreshBefore, &certificatesBefore); err != nil {
		return report, err
	}
	if _, err = tx.Exec(ctx, string(sql)); err != nil {
		var databaseError *pgconn.PgError
		if errors.As(err, &databaseError) && databaseError.Code == "P0001" && strings.HasPrefix(databaseError.Message, "identity correction refused:") {
			report.Reason = databaseError.Message
			if json.Valid([]byte(databaseError.Detail)) {
				report.BlockingIDs = json.RawMessage(databaseError.Detail)
			}
			return report, nil
		}
		return report, err
	}
	var refreshAfter, certificatesAfter int
	// Rollback alone would never fire deferred owner/quota constraints when
	// this preflight is run on a restore already containing multi-cloud DDL.
	if _, err = tx.Exec(ctx, "SET CONSTRAINTS ALL IMMEDIATE"); err != nil {
		return report, err
	}
	if err = tx.QueryRow(ctx, `
		SELECT count(*) FROM organizations o
		WHERE o.organization_kind='brand_cloud' AND to_jsonb(o)->>'deleted_at' IS NULL
		  AND NOT EXISTS (SELECT 1 FROM organization_members m JOIN users u ON u.id=m.user_id
		                  WHERE m.organization_id=o.id AND m.role='owner' AND m.disabled_at IS NULL
		                    AND u.disabled_at IS NULL AND u.email_verified AND NOT u.signup_pending_verification)
	`).Scan(&report.IneligibleOwnerClouds); err != nil {
		return report, err
	}
	if err = tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM identity_correction_candidates),
		(SELECT count(*) FROM organization_member_activation_holds),
		(SELECT count(*) FROM refresh_tokens WHERE revoked_at IS NULL),
		(SELECT count(*) FROM app_certificates WHERE subject_type='user' AND revoked_at IS NULL)`).Scan(&report.CandidateUsers, &report.ActivationHoldsAfter, &refreshAfter, &certificatesAfter); err != nil {
		return report, err
	}
	report.RefreshTokensToRevoke = refreshBefore - refreshAfter
	report.AppCertificatesToRevoke = certificatesBefore - certificatesAfter
	report.Ready = true
	return report, nil
}
