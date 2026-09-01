package database

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"rtk_account_manager/internal/store"
)

// Replay immutable published 049/050, then apply the forward correction.
// No applied marker is deleted and no legacy migration source is patched.
func applyPublishedIdentityCutover(t *testing.T, db *pgxpool.Pool, full string) {
	t.Helper()
	legacyDir := t.TempDir()
	for target, source := range map[string]string{
		"049_unify_human_identity.sql":                 filepath.Join(full, "049_unify_human_identity.sql"),
		"050_backfill_immediate_brand_account_acl.sql": filepath.Join(full, "050_backfill_immediate_brand_account_acl.sql"),
	} {
		data, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(legacyDir, target), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("MIGRATIONS_DIR", legacyDir)
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIGRATIONS_DIR", full)
}

func replayIdentityCorrection(t *testing.T, db *pgxpool.Pool, full string) {
	t.Helper()
	sql, err := os.ReadFile(filepath.Join(full, "051_identity_activation_correction.sql"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(context.Background(), string(sql)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestIdentityForwardCredentialCorrectionIntegration(t *testing.T) {
	for _, tc := range []struct {
		name, hash, wantError                             string
		existing, changed, oidcLink, oidcAudit, soleOwner bool
	}{
		{name: "empty inherited credential"},
		{name: "malformed inherited credential", hash: "not-bcrypt"},
		{name: "existing global untouched", hash: "not-bcrypt", existing: true},
		{name: "changed global credential untouched", hash: "not-bcrypt", changed: true},
		{name: "OIDC linked adoption blocks", hash: "not-bcrypt", oidcLink: true, wantError: "adopted SSO"},
		{name: "OIDC audit adoption blocks", hash: "not-bcrypt", oidcAudit: true, wantError: "adopted SSO"},
		{name: "sole owner correction retains ownership and fences cloud", hash: "not-bcrypt", soleOwner: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, full := newIdentityCaseDatabase(t)
			ctx := context.Background()
			validHash := identityCaseHash(t, "fixture-global-password")
			anchor := ""
			if !tc.soleOwner {
				anchor = identityCaseUser(t, db, "anchor@example.com", validHash, true)
			}
			brand := identityCaseBrand(t, db, "forward-brand", anchor)
			if tc.existing {
				identityCaseUser(t, db, "subject@example.com", validHash, true)
			}
			role := "admin"
			if tc.soleOwner {
				role = "owner"
			}
			legacy := identityCaseLegacy(t, db, brand, "subject@example.com", tc.hash, role, true)
			applyPublishedIdentityCutover(t, db, full)
			user := identityCaseID(t, db, `SELECT id::text FROM users WHERE email='subject@example.com'`)
			if tc.changed {
				identityCaseExec(t, db, `UPDATE users SET password_hash=$2 WHERE id=$1`, user, validHash)
			}
			if tc.oidcLink {
				identityCaseExec(t, db, `WITH provider AS (
					INSERT INTO identity_providers(provider_id,name,type,issuer_url,client_id)
					VALUES('fixture-oidc','Fixture','oidc','https://fixture.invalid','fixture-client') RETURNING id)
					INSERT INTO user_identities(user_id,provider_id,issuer_url,subject,email,email_verified)
					SELECT $1,id,'https://fixture.invalid','fixture-subject','subject@example.com',true FROM provider`, user)
			}
			if tc.oidcAudit {
				identityCaseExec(t, db, `INSERT INTO audit_events(event_type,actor_user_id,subject_type,subject_id)
				VALUES('oidc_login_succeeded',$1::uuid,'user',$1::uuid::text)`, user)
			}
			identityCaseExec(t, db, `INSERT INTO refresh_tokens(user_id,token_hash,expires_at) VALUES($1,'fixture-forward-refresh',now()+interval '1 hour')`, user)
			identityCaseExec(t, db, `INSERT INTO app_certificates(user_id,subject_type,subject_id,subject,csr_sha256,certificate_pem,certificate_chain_pem,fingerprint_sha256,serial_number,issuer_request_id,not_before,not_after)
				VALUES($1::uuid,'user',$1::uuid::text,'app-user:'||$1::uuid::text,'csr','cert','chain','fingerprint','serial','issuer',now()-interval '1 minute',now()+interval '1 hour')`, user)
			report, preflightErr := PreflightIdentityCorrection(ctx, db)
			if preflightErr != nil || !report.RolledBack || report.Ready != (tc.wantError == "") {
				t.Fatalf("preflight=%+v err=%v", report, preflightErr)
			}
			if tc.wantError != "" && (!strings.Contains(report.Reason, tc.wantError) || len(report.BlockingIDs) == 0) {
				t.Fatalf("preflight omitted blocking evidence: %+v", report)
			}
			if report.Ready {
				wantIneligible := 0
				if tc.soleOwner {
					wantIneligible = 1
				}
				if report.IneligibleOwnerClouds != wantIneligible {
					t.Fatalf("fenced cloud count=%d want=%d", report.IneligibleOwnerClouds, wantIneligible)
				}
				want := 1
				if tc.existing || tc.changed {
					want = 0
				}
				if report.CandidateUsers != want || report.RefreshTokensToRevoke != want || report.AppCertificatesToRevoke != want || report.ActivationHoldsAfter != want {
					t.Fatalf("preflight counts=%+v expected=%d", report, want)
				}
			}
			encoded, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "subject@example.com") || strings.Contains(string(encoded), "fixture-forward-refresh") || strings.Contains(string(encoded), validHash) {
				t.Fatal("preflight report exposed credentials/account details")
			}
			var rollbackConfirmed bool
			if err := db.QueryRow(ctx, `SELECT to_regclass('organization_member_activation_holds') IS NULL
				AND NOT EXISTS(SELECT 1 FROM refresh_tokens WHERE revoked_at IS NOT NULL)
				AND NOT EXISTS(SELECT 1 FROM audit_events WHERE event_type='identity_credential_corrected')`).Scan(&rollbackConfirmed); err != nil {
				t.Fatal(err)
			}
			if !rollbackConfirmed {
				t.Fatal("preflight committed changes")
			}
			err = Migrate(ctx, db)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("want %s refusal, got %v", tc.wantError, err)
				}
				var unchanged bool
				if err := db.QueryRow(ctx, `SELECT u.password_hash=$2 AND u.email_verified AND u.signup_pending_verification=$3
					AND EXISTS(SELECT 1 FROM refresh_tokens WHERE user_id=u.id AND revoked_at IS NULL)
					AND EXISTS(SELECT 1 FROM app_certificates WHERE user_id=u.id AND revoked_at IS NULL)
					AND NOT EXISTS(SELECT 1 FROM schema_migrations WHERE version='051_identity_activation_correction.sql')
					AND NOT EXISTS(SELECT 1 FROM audit_events WHERE event_type='identity_credential_corrected')
					AND to_regclass('organization_member_activation_holds') IS NULL
					FROM users u WHERE u.id=$1`, user, tc.hash, false).Scan(&unchanged); err != nil {
					t.Fatal(err)
				}
				if !unchanged {
					t.Fatal("refused forward migration left partial state")
				}
				if !tc.oidcLink && !tc.oidcAudit {
					return
				}
				// Approved local-credential remediation leaves SSO verification and
				// memberships intact; the next cutover must preserve that adoption.
				identityCaseExec(t, db, `UPDATE users SET password_hash=$2 WHERE id=$1`, user, validHash)
				if err := Migrate(ctx, db); err != nil {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			corrected := !tc.existing && !tc.changed && !tc.oidcLink && !tc.oidcAudit
			var hash, result, conflict string
			var verified, pending, disabled bool
			if err := db.QueryRow(ctx, `SELECT u.password_hash,u.email_verified,u.signup_pending_verification,m.disabled_at IS NOT NULL,map.result,map.conflict_status
				FROM users u JOIN organization_members m ON m.user_id=u.id AND m.organization_id=$2
				JOIN brand_cloud_user_migrations map ON map.user_id=u.id AND map.brand_cloud_user_id=$3 WHERE u.id=$1`, user, brand, legacy).Scan(&hash, &verified, &pending, &disabled, &result, &conflict); err != nil {
				t.Fatal(err)
			}
			if corrected {
				if verified || !pending || !disabled || !strings.HasPrefix(hash, "!password-reset-required!") || result != "created_user" || conflict != "password_reset_required" {
					t.Fatal("correction did not suspend the exact inherited account")
				}
			} else if hash != validHash || !verified || pending || disabled || conflict != "none" {
				t.Fatal("unrelated/remediated global account was changed")
			}
			replayIdentityCorrection(t, db, full)
			var events, holds, refresh, certificates int
			if err := db.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM audit_events WHERE event_type='identity_credential_corrected'),
				(SELECT count(*) FROM organization_member_activation_holds WHERE user_id=$1),
				(SELECT count(*) FROM refresh_tokens WHERE user_id=$1 AND revoked_at IS NOT NULL),
				(SELECT count(*) FROM app_certificates WHERE user_id=$1 AND revoked_at IS NOT NULL)`, user).Scan(&events, &holds, &refresh, &certificates); err != nil {
				t.Fatal(err)
			}
			want := 0
			if corrected {
				want = 1
			}
			if events != want || holds != want || refresh != want || certificates != want {
				t.Fatalf("idempotent correction events/holds/revocations=%d/%d/%d/%d want %d", events, holds, refresh, certificates, want)
			}
			if corrected {
				s := store.New(db)
				if err := s.CreatePasswordResetToken(ctx, user, "fixture-reset", time.Now().Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
				if _, err := s.ResetPasswordWithToken(ctx, "fixture-reset", validHash); err != nil {
					t.Fatal(err)
				}
				if _, err := s.GetRole(ctx, brand, user); err == nil {
					t.Fatal("password reset granted a pending membership")
				}
				if err := s.CreateEmailVerificationToken(ctx, user, "fixture-verify", time.Now().Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
				if _, err := s.VerifyEmailToken(ctx, "fixture-verify", validHash); err != nil {
					t.Fatal(err)
				}
				if _, err := s.GetRole(ctx, brand, user); err != nil {
					t.Fatal(err)
				}
				if _, err := s.RefreshTokenActive(ctx, "fixture-forward-refresh"); err == nil {
					t.Fatal("activation resurrected revoked refresh")
				}
				if err := s.AuthorizeActiveAppCertificateForSubject(ctx, "user", user, "fingerprint", time.Now()); err == nil {
					t.Fatal("activation resurrected revoked certificate")
				}
			}
		})
	}
}

func TestIdentityForwardResolutionGuardsIntegration(t *testing.T) {
	for _, mode := range []string{"conflicting", "stale", "missing-evidence", "missing-actor", "latest-keep"} {
		t.Run(mode, func(t *testing.T) {
			db, full := newIdentityCaseDatabase(t)
			ctx := context.Background()
			hash := identityCaseHash(t, "fixture-resolution-password")
			anchor := identityCaseUser(t, db, "anchor@example.com", hash, true)
			brand := identityCaseBrand(t, db, "resolution-brand", anchor)
			user := identityCaseUser(t, db, "subject@example.com", hash, false)
			identityCaseLegacy(t, db, brand, "subject@example.com", hash, "admin", false)
			applyPublishedIdentityCutover(t, db, full)
			// Existing-global provenance is deliberately ambiguous, so an invalid
			// resolution cannot be masked by automatic created-user backfill.
			identityCaseExec(t, db, `INSERT INTO audit_events(event_type,actor_user_id,organization_id,subject_type,subject_id,payload)
				SELECT 'identity_activation_hold_resolved',CASE WHEN $4='missing-actor' THEN NULL ELSE $3::uuid END,
				organization_id,'user',user_id::text,jsonb_build_object('decision','restore_after_verification',
				'source','identity_migration','evidence',CASE WHEN $4='missing-evidence' THEN ' ' ELSE 'reviewed-fixture-backup' END,
				'disabled_at',disabled_at::text,'updated_at',updated_at::text)
				FROM organization_members WHERE organization_id=$1 AND user_id=$2`, brand, user, anchor, mode)
			if mode == "conflicting" || mode == "latest-keep" {
				identityCaseExec(t, db, `INSERT INTO audit_events(event_type,actor_user_id,organization_id,subject_type,subject_id,payload,created_at)
					SELECT event_type,actor_user_id,organization_id,subject_type,subject_id,
					payload || '{"decision":"keep_disabled"}'::jsonb,
					CASE WHEN $3='conflicting' THEN created_at ELSE created_at+interval '1 second' END
					FROM audit_events WHERE event_type='identity_activation_hold_resolved' AND organization_id=$1 AND subject_id=$2`, brand, user, mode)
			}
			if mode == "stale" {
				identityCaseExec(t, db, `UPDATE organization_members SET updated_at=updated_at+interval '1 second' WHERE organization_id=$1 AND user_id=$2`, brand, user)
			}
			report, err := PreflightIdentityCorrection(ctx, db)
			wantReady := mode == "latest-keep"
			if err != nil || !report.RolledBack || report.Ready != wantReady {
				t.Fatalf("preflight=%+v err=%v", report, err)
			}
			if !wantReady && len(report.BlockingIDs) == 0 {
				t.Fatal("refused resolution has no diagnostic IDs")
			}
			if mode == "conflicting" && !strings.Contains(report.Reason, "conflicting activation") {
				t.Fatalf("wrong refusal: %s", report.Reason)
			}
			err = Migrate(ctx, db)
			if !wantReady {
				if err == nil {
					t.Fatal("invalid resolution allowed migration")
				}
				var partial bool
				if err := db.QueryRow(ctx, `SELECT to_regclass('organization_member_activation_holds') IS NOT NULL
					OR EXISTS(SELECT 1 FROM schema_migrations WHERE version='051_identity_activation_correction.sql')`).Scan(&partial); err != nil || partial {
					t.Fatalf("failed correction committed: partial=%t err=%v", partial, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var holds int
			if err := db.QueryRow(ctx, `SELECT count(*) FROM organization_member_activation_holds WHERE organization_id=$1 AND user_id=$2`, brand, user).Scan(&holds); err != nil || holds != 0 {
				t.Fatalf("latest keep decision ignored: holds=%d err=%v", holds, err)
			}
		})
	}
}

func TestIdentityPreflightRequiresPreparedDatabase(t *testing.T) {
	db, _ := newIdentityCaseDatabase(t)
	report, err := PreflightIdentityCorrection(context.Background(), db)
	if err == nil || !strings.Contains(err.Error(), "through 050") || report.Ready || !report.RolledBack {
		t.Fatalf("unprepared preflight=%+v err=%v", report, err)
	}
}

func TestIdentityForwardActivationProvenanceIntegration(t *testing.T) {
	for _, tc := range []struct {
		name                                                                     string
		legacy, existing, pending, expired, edited, adminDisabled, noToken, keep bool
		wantBlock                                                                bool
	}{
		{name: "created legacy suspension", legacy: true},
		{name: "existing global legacy ambiguity", legacy: true, existing: true, wantBlock: true},
		{name: "legacy administrative disable", legacy: true, adminDisabled: true, keep: true},
		{name: "edited legacy suspension", legacy: true, edited: true, wantBlock: true, keep: true},
		{name: "explicit keep overrides eligible legacy backfill", legacy: true, keep: true},
		{name: "pending email provisioning", pending: true},
		{name: "non-pending email provisioning"},
		{name: "expired email provisioning", expired: true},
		{name: "edited email provisioning", edited: true, wantBlock: true, keep: true},
		{name: "administratively disabled provisioning", adminDisabled: true, wantBlock: true, keep: true},
		{name: "missing provisioning token", noToken: true, wantBlock: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, full := newIdentityCaseDatabase(t)
			ctx := context.Background()
			hash := identityCaseHash(t, "fixture-provenance-password")
			anchor := identityCaseUser(t, db, "anchor@example.com", hash, true)
			brand := identityCaseBrand(t, db, "provenance-brand", anchor)
			var user string
			if tc.legacy {
				if tc.existing {
					user = identityCaseUser(t, db, "subject@example.com", hash, false)
				}
				legacy := identityCaseLegacy(t, db, brand, "subject@example.com", hash, "admin", false)
				if tc.adminDisabled {
					identityCaseExec(t, db, `UPDATE brand_cloud_users SET disabled_at=now()-interval '1 day' WHERE id=$1`, legacy)
				}
			}
			applyPublishedIdentityCutover(t, db, full)
			if tc.legacy {
				user = identityCaseID(t, db, `SELECT id::text FROM users WHERE email='subject@example.com'`)
			} else {
				user = identityCaseUser(t, db, "subject@example.com", hash, false)
				identityCaseExec(t, db, `UPDATE users SET signup_pending_verification=$2 WHERE id=$1`, user, tc.pending)
				tx, err := db.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(ctx)
				if _, err := tx.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role,disabled_at,created_at,updated_at)
					VALUES($1,$2,'admin',CASE WHEN $3 THEN now()-interval '1 day' ELSE now() END,
					CASE WHEN $3 THEN now()-interval '2 days' ELSE now() END,now())`, brand, user, tc.adminDisabled); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(ctx, `INSERT INTO audit_events(event_type,organization_id,subject_type,subject_id,payload)
					VALUES('brand_cloud_account_assigned',$1,'user',$2,'{"activation_mode":"email"}')`, brand, user); err != nil {
					t.Fatal(err)
				}
				if !tc.noToken {
					if _, err := tx.Exec(ctx, `INSERT INTO auth_tokens(user_id,subject_type,subject_id,purpose,scope,token_hash,expires_at)
						VALUES($1,'user',$1,'email_verification','','fixture-old-provision',CASE WHEN $2 THEN now()-interval '1 hour' ELSE now()+interval '1 hour' END)`, user, tc.expired); err != nil {
						t.Fatal(err)
					}
				}
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if tc.edited {
				identityCaseExec(t, db, `UPDATE organization_members SET role='member',updated_at=now() WHERE organization_id=$1 AND user_id=$2`, brand, user)
			}
			source := "provisioning"
			if tc.legacy {
				source = "identity_migration"
			}
			resolve := func(decision string) {
				identityCaseExec(t, db, `INSERT INTO audit_events(event_type,actor_user_id,organization_id,subject_type,subject_id,payload)
					SELECT 'identity_activation_hold_resolved',$3,organization_id,'user',user_id::text,
					jsonb_build_object('decision',$4::text,'source',$5::text,'evidence','fixture-reviewed-backup','disabled_at',disabled_at::text,'updated_at',updated_at::text)
					FROM organization_members WHERE organization_id=$1 AND user_id=$2`, brand, user, anchor, decision, source)
			}
			if tc.keep && !tc.wantBlock && !tc.adminDisabled {
				resolve("keep_disabled")
			}
			err := Migrate(ctx, db)
			if tc.wantBlock {
				if err == nil || !strings.Contains(err.Error(), "unresolved activation") {
					t.Fatalf("expected provenance refusal, got %v", err)
				}
				var partial bool
				if err := db.QueryRow(ctx, `SELECT to_regclass('organization_member_activation_holds') IS NOT NULL
					OR EXISTS(SELECT 1 FROM schema_migrations WHERE version='051_identity_activation_correction.sql')`).Scan(&partial); err != nil {
					t.Fatal(err)
				}
				if partial {
					t.Fatal("provenance refusal committed partial correction")
				}
				decision := "restore_after_verification"
				if tc.keep {
					decision = "keep_disabled"
				}
				resolve(decision)
				if err := Migrate(ctx, db); err != nil {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			replayIdentityCorrection(t, db, full)
			var holds int
			if err := db.QueryRow(ctx, `SELECT count(*) FROM organization_member_activation_holds WHERE organization_id=$1 AND user_id=$2`, brand, user).Scan(&holds); err != nil {
				t.Fatal(err)
			}
			want := 1
			if tc.keep {
				want = 0
			}
			if holds != want {
				t.Fatalf("holds=%d want=%d", holds, want)
			}
			s := store.New(db)
			if _, err := s.CreateEmailVerificationTokenForEmail(ctx, "subject@example.com", "fixture-resend", time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if status, err := s.EmailVerificationTokenStatus(ctx, "fixture-resend"); err != nil || status != "valid" {
				t.Fatalf("resent status=%s err=%v", status, err)
			}
			if _, err := s.VerifyEmailToken(ctx, "fixture-resend", hash); err != nil {
				t.Fatal(err)
			}
			var enabled bool
			if err := db.QueryRow(ctx, `SELECT disabled_at IS NULL FROM organization_members WHERE organization_id=$1 AND user_id=$2`, brand, user).Scan(&enabled); err != nil {
				t.Fatal(err)
			}
			if enabled == tc.keep {
				t.Fatalf("activation enabled=%t keep-disabled=%t", enabled, tc.keep)
			}
			if _, err := s.VerifyEmailToken(ctx, "fixture-resend", hash); err == nil {
				t.Fatal("activation token replay succeeded")
			}
		})
	}
}
