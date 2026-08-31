package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"rtk_account_manager/internal/emaildelivery"
)

func TestGlobalEmailLoginDoesNotDependOnMembershipNamespace(t *testing.T) {
	for _, tc := range []struct {
		name, role                           string
		platform, pending, disabled, missing bool
		brands                               int
	}{
		{name: "owner", role: "owner", brands: 1},
		{name: "admin", role: "admin", brands: 1},
		{name: "member", role: "member", brands: 1},
		{name: "multiple-brands", role: "admin", brands: 2},
		{name: "dual-role", role: "owner", brands: 1, platform: true},
		{name: "platform-only", platform: true},
		{name: "no-membership"},
		{name: "pending-owner", role: "owner", brands: 1, pending: true},
		{name: "disabled-owner", role: "owner", brands: 1, disabled: true},
		{name: "unknown", missing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newStoreIntegrationEnv(t)
			ctx := context.Background()
			env.store.ConfigureEmailOutboxCipher(integrationEmailCipher(t))
			const email = "global-email-user@example.com"
			var userID string
			if !tc.missing {
				if err := env.db.QueryRow(ctx, `INSERT INTO users(email,password_hash,email_verified,signup_pending_verification,platform_admin,disabled_at)
					VALUES($1,'fixture-password-hash',NOT $2,$2,$3,CASE WHEN $4 THEN now() ELSE NULL END) RETURNING id::text`, email, tc.pending, tc.platform, tc.disabled).Scan(&userID); err != nil {
					t.Fatal(err)
				}
				var anchorID string
				if tc.brands > 0 {
					if err := env.db.QueryRow(ctx, `INSERT INTO users(email,password_hash,email_verified)
						VALUES('fixture-anchor@example.com','fixture-password-hash',true) RETURNING id::text`).Scan(&anchorID); err != nil {
						t.Fatal(err)
					}
				}
				for n := 0; n < tc.brands; n++ {
					var brandID string
					if err := env.db.QueryRow(ctx, `INSERT INTO organizations(name,organization_kind,status,tenant_slug)
						VALUES('Global email fixture','brand_cloud','active',gen_random_uuid()::text) RETURNING id::text`).Scan(&brandID); err != nil {
						t.Fatal(err)
					}
					if _, err := env.db.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role) VALUES($1,$2,$3),($1,$4,'owner')`, brandID, userID, tc.role, anchorID); err != nil {
						t.Fatal(err)
					}
				}
			}
			eligible := !tc.pending && !tc.disabled && !tc.missing
			for _, withEmail := range []bool{false, true} {
				tokenHash := "fixture-global-login-token"
				expiresAt := time.Now().Add(time.Hour)
				var created bool
				var err error
				if withEmail {
					tokenHash += "-outbox"
					created, err = env.store.CreateLoginActivationTokenForEmailAndEmail(ctx, " Global-Email-User@Example.COM ", tokenHash, expiresAt, EmailOutboxInput{
						IdempotencyKey: tokenHash, MessageType: "login_activation", TemplateVersion: 1, ExpiresAt: &expiresAt,
						Payload: emaildelivery.Payload{RecipientEmail: email, Token: "fixture-email-token", ExpiresAt: expiresAt.Format(time.RFC3339)},
					})
				} else {
					created, err = env.store.CreateLoginActivationTokenForEmail(ctx, " Global-Email-User@Example.COM ", tokenHash, expiresAt)
				}
				if err != nil || created != eligible {
					t.Fatalf("withEmail=%t created=%t want=%t err=%v", withEmail, created, eligible, err)
				}
				if !eligible {
					continue
				}
				var subjectType, subjectID, scope string
				if err := env.db.QueryRow(ctx, `SELECT subject_type,subject_id,scope FROM auth_tokens WHERE token_hash=$1`, tokenHash).Scan(&subjectType, &subjectID, &scope); err != nil {
					t.Fatal(err)
				}
				if subjectType != "user" || subjectID != userID || scope != "" {
					t.Fatal("email login token is not bound to the global user with empty tenant scope")
				}
				activated, err := env.store.ActivateLoginToken(ctx, tokenHash)
				if err != nil || activated.ID != userID {
					t.Fatalf("activate global identity: user=%s err=%v", activated.ID, err)
				}
				if _, err := env.store.ActivateLoginToken(ctx, tokenHash); !errors.Is(err, ErrNotFound) {
					t.Fatalf("consumed token replay: %v", err)
				}
			}
			if eligible {
				expiresAt := time.Now().Add(time.Hour)
				_, err := env.store.CreateLoginActivationTokenForEmailAndEmail(ctx, email, "fixture-rollback-token", expiresAt, EmailOutboxInput{
					IdempotencyKey: "fixture-rollback", MessageType: "unsupported", ExpiresAt: &expiresAt,
					Payload: emaildelivery.Payload{RecipientEmail: email, Token: "fixture-email-token"},
				})
				if err == nil {
					t.Fatal("invalid outbox write must roll back token creation")
				}
			}
			var tokens, emails int
			if err := env.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM auth_tokens),(SELECT count(*) FROM email_outbox)`).Scan(&tokens, &emails); err != nil {
				t.Fatal(err)
			}
			if eligible && (tokens != 2 || emails != 1) || !eligible && (tokens != 0 || emails != 0) {
				t.Fatalf("token/outbox counts=%d/%d eligible=%t", tokens, emails, eligible)
			}
		})
	}
}
