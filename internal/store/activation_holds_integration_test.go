package store

import (
	"context"
	"testing"
	"time"

	"rtk_account_manager/internal/emaildelivery"
	"rtk_account_manager/internal/model"
)

func TestEmailActivationRestoresOnlyUnchangedVerificationHolds(t *testing.T) {
	for _, kind := range []string{"new", "existing-unverified", "repeat-email", "admin-disable", "admin-disable-then-email", "role-change", "provision-role-change", "stale-hold", "removed-member", "global-disable-enable", "same-transaction-disable", "existing-admin-disable", "outbox-rollback"} {
		t.Run(kind, func(t *testing.T) {
			env := newStoreIntegrationEnv(t)
			ctx := context.Background()
			env.store.ConfigureEmailOutboxCipher(integrationEmailCipher(t))
			anchor, err := env.store.EnsurePlatformAdmin(ctx, "anchor@example.com", "fixture-anchor-hash", nil)
			if err != nil {
				t.Fatal(err)
			}
			brand, err := env.store.CreateBrandCloud(ctx, anchor.ID, BrandCloudInput{Name: "Hold Fixture", TenantSlug: "hold-fixture", OwnerUserID: anchor.ID})
			if err != nil {
				t.Fatal(err)
			}
			const email = "hold-subject@example.com"
			if kind == "existing-unverified" || kind == "existing-admin-disable" {
				registered, err := env.store.Register(ctx, RegisterInput{Email: email, PasswordHash: "fixture-existing-hash", OrganizationName: "Existing"})
				if err != nil {
					t.Fatal(err)
				}
				if kind == "existing-admin-disable" {
					if _, err := env.db.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role,disabled_at) VALUES($1,$2,'admin',now()-interval '1 day')`, brand.ID, registered.User.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			expires := time.Now().Add(time.Hour)
			input := BrandCloudAccountInput{Email: email, PasswordHash: "!pending!", Role: model.RoleAdmin, ActivationMode: "email", ActivationTokenHash: "fixture-hold-token", ActivationExpiresAt: expires,
				ActivationEmail: &EmailOutboxInput{IdempotencyKey: "fixture-hold-outbox", TemplateVersion: 1, ExpiresAt: &expires, Payload: emaildelivery.Payload{RecipientEmail: email, Token: "fixture-hold-token", ExpiresAt: expires.Format(time.RFC3339)}}}
			if kind == "outbox-rollback" {
				env.store.ConfigureEmailOutboxCipher(nil)
			}
			account, err := env.store.ProvisionBrandCloudAccount(ctx, anchor.ID, brand.ID, input)
			if kind == "outbox-rollback" {
				if err == nil {
					t.Fatal("missing cipher must reject the transaction")
				}
				var leaked bool
				if err := env.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE email=$1)
					OR EXISTS(SELECT 1 FROM organization_member_activation_holds)
					OR EXISTS(SELECT 1 FROM email_outbox)`, email).Scan(&leaked); err != nil {
					t.Fatal(err)
				}
				if leaked {
					t.Fatal("failed outbox left an identity, hold or email")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			user := account.User.ID
			if account.Member.DisabledAt == nil {
				t.Fatal("unverified email provisioning must suspend new membership")
			}
			switch kind {
			case "admin-disable", "admin-disable-then-email":
				if _, err := env.store.DisableDeveloperBrandCloudMember(ctx, brand.ID, user); err != nil {
					t.Fatal(err)
				}
			case "role-change":
				if _, err := env.store.UpdateMemberRole(ctx, brand.ID, user, model.RoleMember); err != nil {
					t.Fatal(err)
				}
			case "stale-hold":
				if _, err := env.db.Exec(ctx, `UPDATE organization_member_activation_holds SET updated_at=updated_at-interval '1 second' WHERE user_id=$1`, user); err != nil {
					t.Fatal(err)
				}
			case "removed-member":
				if err := env.store.RemoveMember(ctx, brand.ID, user); err != nil {
					t.Fatal(err)
				}
			case "global-disable-enable":
				if _, err := env.store.DisableMemberUser(ctx, brand.ID, user); err != nil {
					t.Fatal(err)
				}
				if _, err := env.store.EnableMemberUser(ctx, brand.ID, user); err != nil {
					t.Fatal(err)
				}
			case "same-transaction-disable":
				tx, err := env.db.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(ctx)
				if _, err := tx.Exec(ctx, `UPDATE organization_members SET disabled_at=now(),updated_at=now() WHERE organization_id=$1 AND user_id=$2`, brand.ID, user); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(ctx, `INSERT INTO organization_member_activation_holds(organization_id,user_id,disabled_at,updated_at,source)
					SELECT organization_id,user_id,disabled_at,updated_at,'provisioning' FROM organization_members WHERE organization_id=$1 AND user_id=$2`, brand.ID, user); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(ctx, `UPDATE organization_members SET disabled_at=disabled_at,updated_at=updated_at WHERE organization_id=$1 AND user_id=$2`, brand.ID, user); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "repeat-email" || kind == "admin-disable-then-email" || kind == "provision-role-change" {
				input.ActivationTokenHash = "fixture-second-token"
				input.ActivationEmail.IdempotencyKey = "fixture-second-outbox"
				if kind == "provision-role-change" {
					input.Role = model.RoleMember
				}
				if _, err := env.store.ProvisionBrandCloudAccount(ctx, anchor.ID, brand.ID, input); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := env.store.VerifyEmailToken(ctx, input.ActivationTokenHash, "fixture-activated-hash"); err != nil {
				t.Fatal(err)
			}
			var enabled bool
			var holds int
			if err := env.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organization_members WHERE organization_id=$1 AND user_id=$2 AND disabled_at IS NULL),
				(SELECT count(*) FROM organization_member_activation_holds WHERE user_id=$2)`, brand.ID, user).Scan(&enabled, &holds); err != nil {
				t.Fatal(err)
			}
			want := kind == "new" || kind == "existing-unverified" || kind == "repeat-email"
			if enabled != want || holds != 0 {
				t.Fatalf("enabled=%t want=%t remaining holds=%d", enabled, want, holds)
			}
			if _, err := env.store.VerifyEmailToken(ctx, input.ActivationTokenHash, "fixture-replayed-hash"); err == nil {
				t.Fatal("consumed activation token replayed")
			}
		})
	}
}
