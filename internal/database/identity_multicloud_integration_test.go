package database

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rtk_account_manager/internal/store"
)

func TestIdentityForwardRejectsNonUniqueDesignatedOwner(t *testing.T) {
	for _, state := range []string{"zero-disabled-cloud", "multiple-active-cloud"} {
		t.Run(state, func(t *testing.T) {
			db, full := newIdentityCaseDatabase(t)
			ctx := context.Background()
			owner := identityCaseUser(t, db, "owner@example.com", "fixture-existing-hash", true)
			cloud := ""
			if state == "zero-disabled-cloud" {
				cloud = identityCaseID(t, db, `INSERT INTO organizations(name,organization_kind,status,tenant_slug)
                    VALUES('Empty','brand_cloud','disabled','empty') RETURNING id::text`)
			} else {
				cloud = identityCaseBrand(t, db, "multiple", owner)
				second := identityCaseUser(t, db, "second@example.com", "fixture-existing-hash", true)
				identityCaseExec(t, db, `INSERT INTO organization_members(organization_id,user_id,role) VALUES($1,$2,'owner')`, cloud, second)
			}
			applyPublishedIdentityCutover(t, db, full)
			report, err := PreflightIdentityCorrection(ctx, db)
			if err != nil || report.Ready || !report.RolledBack || !strings.Contains(report.Reason, "exactly one designated owner") || !strings.Contains(string(report.BlockingIDs), cloud) {
				t.Fatalf("cardinality preflight=%+v err=%v", report, err)
			}
			if err := Migrate(ctx, db); err == nil || !strings.Contains(err.Error(), "exactly one designated owner") {
				t.Fatalf("invalid ownership must stop 051: %v", err)
			}
			var unchanged bool
			if err := db.QueryRow(ctx, `SELECT to_regclass('organization_member_activation_holds') IS NULL
                AND NOT EXISTS(SELECT 1 FROM schema_migrations WHERE version='051_identity_activation_correction.sql')`).Scan(&unchanged); err != nil || !unchanged {
				t.Fatalf("failed forward migration changed state: unchanged=%t err=%v", unchanged, err)
			}
		})
	}
}

func TestIdentityCorrectionFencesAllCloudActorsUntilOwnerActivates(t *testing.T) {
	db, full := newIdentityCaseDatabase(t)
	ctx := context.Background()
	cloud := identityCaseBrand(t, db, "owner-correction", "")
	identityCaseLegacy(t, db, cloud, "owner@example.com", "unsupported-inherited-hash", "owner", true)
	collaborator := identityCaseUser(t, db, "collaborator@example.com", "fixture-global-hash", true)
	identityCaseExec(t, db, `INSERT INTO organization_members(organization_id,user_id,role) VALUES($1,$2,'admin')`, cloud, collaborator)
	applyPublishedIdentityCutover(t, db, full)
	owner := identityCaseID(t, db, `SELECT id::text FROM users WHERE email='owner@example.com'`)
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	s := store.New(db)
	for _, user := range []string{owner, collaborator} {
		if _, err := s.GetRole(ctx, cloud, user); err == nil {
			t.Fatal("ineligible owner cloud exposed a role")
		}
		var allowed bool
		if err := db.QueryRow(ctx, `SELECT user_can_access_brand_cloud($1,$2)`, user, cloud).Scan(&allowed); err != nil || allowed {
			t.Fatalf("cloud fence allowed=%t err=%v", allowed, err)
		}
	}
	var owners int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM organization_members WHERE organization_id=$1 AND role='owner'`, cloud).Scan(&owners); err != nil || owners != 1 {
		t.Fatalf("correction must retain one designated owner: %d %v", owners, err)
	}
	// The same rollback-only executable must work after 052+ are installed,
	// including their deferred owner/quota constraints and eligibility fences.
	report, err := PreflightIdentityCorrection(ctx, db)
	if err != nil || !report.Ready || !report.RolledBack || report.IneligibleOwnerClouds != 1 || report.CandidateUsers != 0 {
		t.Fatalf("multi-cloud replay preflight=%+v err=%v", report, err)
	}
	if err := s.CreateEmailVerificationToken(ctx, owner, "owner-activation", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyEmailToken(ctx, "owner-activation", identityCaseHash(t, "fixture-activated-owner")); err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{owner, collaborator} {
		if _, err := s.GetRole(ctx, cloud, user); err != nil {
			t.Fatalf("owner activation did not release cloud fence: %v", err)
		}
	}
}

func TestIdentityForwardAcceptsPendingOrDisabledDesignatedOwner(t *testing.T) {
	for _, state := range []string{"pending", "disabled"} {
		t.Run(state, func(t *testing.T) {
			db, full := newIdentityCaseDatabase(t)
			owner := identityCaseUser(t, db, "owner@example.com", "fixture-existing-hash", true)
			cloud := identityCaseBrand(t, db, "retained-owner", owner)
			applyPublishedIdentityCutover(t, db, full)
			identityCaseExec(t, db, `UPDATE users SET signup_pending_verification=$2,disabled_at=CASE WHEN $3 THEN now() ELSE NULL END WHERE id=$1`, owner, state == "pending", state == "disabled")
			report, err := PreflightIdentityCorrection(context.Background(), db)
			if err != nil || !report.Ready || !report.RolledBack || report.IneligibleOwnerClouds != 1 {
				t.Fatalf("designated owner preflight=%+v err=%v", report, err)
			}
			if err := Migrate(context.Background(), db); err != nil {
				t.Fatal(err)
			}
			if _, err := store.New(db).GetRole(context.Background(), cloud, owner); err == nil {
				t.Fatal("ineligible owner gained access")
			}
		})
	}
}

func TestIdentityPre049DuplicateTargetRemainsReleaseBlocker(t *testing.T) {
	db, full := newIdentityCaseDatabase(t)
	owner := identityCaseUser(t, db, "anchor@example.com", "fixture-global-hash", true)
	a := identityCaseBrand(t, db, "duplicate-a", owner)
	b := identityCaseBrand(t, db, "duplicate-b", owner)
	hash := identityCaseHash(t, "fixture-legacy")
	identityCaseLegacy(t, db, a, "shared@example.com", hash, "member", true)
	other := identityCaseLegacy(t, db, b, "shared@example.com", hash, "admin", true)
	identityCaseExec(t, db, `INSERT INTO brand_cloud_memberships(brand_cloud_id,brand_cloud_user_id,role) VALUES($1,$2,'admin')`, a, other)
	// Do not silently patch published 049 or drop evidence to force success.
	// This path needs its own reviewed pre-cutover remediation before release.
	t.Setenv("MIGRATIONS_DIR", filepath.Clean(full))
	if err := Migrate(context.Background(), db); err == nil || !strings.Contains(err.Error(), "cannot affect row a second time") {
		t.Fatalf("expected published 049 duplicate-target blocker, got %v", err)
	}
	var untouched bool
	if err := db.QueryRow(context.Background(), `SELECT NOT EXISTS(SELECT 1 FROM schema_migrations WHERE version='049_unify_human_identity.sql')
        AND to_regclass('brand_cloud_user_migrations') IS NULL
        AND (SELECT count(*) FROM brand_cloud_users)=2`).Scan(&untouched); err != nil || !untouched {
		t.Fatalf("failed fresh cutover lost evidence: intact=%t err=%v", untouched, err)
	}
}

func TestIdentityForwardAppliesAfterPreviouslyInstalledMultiCloudMigrations(t *testing.T) {
	db, full := newIdentityCaseDatabase(t)
	ctx := context.Background()
	cloud := identityCaseBrand(t, db, "late-identity-correction", "")
	identityCaseLegacy(t, db, cloud, "late-owner@example.com", "unsafe-inherited-hash", "owner", true)
	applyPublishedIdentityCutover(t, db, full)
	owner := identityCaseID(t, db, `SELECT id::text FROM users WHERE email='late-owner@example.com'`)
	// Model the earlier implementation candidate, which contained 052+ but
	// had not integrated 051. Never remove an applied marker to build a fixture.
	priorCandidate := t.TempDir()
	entries, err := os.ReadDir(full)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") || entry.Name() < multiCloudMigration {
			continue
		}
		contents, err := os.ReadFile(filepath.Join(full, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(priorCandidate, entry.Name()), contents, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("MIGRATIONS_DIR", priorCandidate)
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var beforeVersion int64
	if err := db.QueryRow(ctx, `SELECT authorization_version FROM organizations WHERE id=$1`, cloud).Scan(&beforeVersion); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIGRATIONS_DIR", full)
	report, err := PreflightIdentityCorrection(ctx, db)
	if err != nil || !report.Ready || !report.RolledBack || report.CandidateUsers != 1 || report.IneligibleOwnerClouds != 1 {
		t.Fatalf("late correction preflight=%+v err=%v", report, err)
	}
	var rollbackVerified bool
	if err := db.QueryRow(ctx, `SELECT authorization_version=$2 AND to_regclass('organization_member_activation_holds') IS NULL FROM organizations WHERE id=$1`, cloud, beforeVersion).Scan(&rollbackVerified); err != nil || !rollbackVerified {
		t.Fatalf("preflight changed multi-cloud authority version: intact=%t err=%v", rollbackVerified, err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var corrected bool
	if err := db.QueryRow(ctx, `SELECT authorization_version>$2 AND NOT user_can_access_brand_cloud($3,id::text)
		AND EXISTS(SELECT 1 FROM organization_member_activation_holds h WHERE h.organization_id=id)
		FROM organizations WHERE id=$1`, cloud, beforeVersion, owner).Scan(&corrected); err != nil || !corrected {
		t.Fatalf("late correction did not preserve fence/hold: corrected=%t err=%v", corrected, err)
	}
}
