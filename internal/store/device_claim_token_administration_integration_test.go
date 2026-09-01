package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"rtk_account_manager/internal/model"
)

func platformTokenInput(actor, cloud, hash string, product *string) DeviceClaimTokenCreateInput {
	return DeviceClaimTokenCreateInput{CreatedBy: &actor, OrganizationID: &cloud, DeviceItemProfileID: product, TokenHash: hash, Category: model.DeviceCategoryIPCamera, VideoCloudDevid: hash, ActivityID: "activity", ClipPublicKey: "sensitive-provisioning-material", ServiceOptions: []string{"mqtt"}, ExpiresAt: time.Now().Add(time.Hour)}
}

func TestPlatformClaimTokenAdministrationRechecksAuthority(t *testing.T) {
	for _, action := range []string{"create", "revoke"} {
		for _, state := range []string{"missing_actor", "not_admin", "disabled", "pending", "canceled"} {
			t.Run(action+"/"+state, func(t *testing.T) {
				env := newStoreIntegrationEnv(t)
				ctx := context.Background()
				owner := handoffDeveloper(t, env, "token-authority")
				token := claimAuthorizationToken(t, env, owner.BrandCloud.ID, "authority-token", nil)
				actor := owner.User.ID
				if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin=true WHERE id=$1`, actor); err != nil {
					t.Fatal(err)
				}
				var setup string
				expected := ErrNotFound
				switch state {
				case "missing_actor":
					actor = ""
				case "not_admin":
					setup = `UPDATE users SET platform_admin=false`
				case "disabled":
					setup = `UPDATE users SET disabled_at=now()`
				case "pending":
					setup = `UPDATE users SET signup_pending_verification=true`
				case "canceled":
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
					expected = context.Canceled
				}
				if setup != "" {
					if _, err := env.db.Exec(ctx, setup); err != nil {
						t.Fatal(err)
					}
				}
				var err error
				if action == "create" {
					_, err = env.store.CreateDeviceClaimTokenAsPlatform(ctx, platformTokenInput(actor, owner.BrandCloud.ID, "unauthorized-token", nil))
				} else {
					_, err = env.store.RevokeDeviceClaimTokenAsPlatform(ctx, actor, token.ID, time.Time{})
				}
				if !errors.Is(err, expected) {
					t.Fatalf("authority bypass: %v, want %v", err, expected)
				}
				var tokens, revoked, audits int
				if err := env.db.QueryRow(context.Background(), `SELECT count(*),count(revoked_at),(SELECT count(*) FROM audit_events WHERE event_type IN ('device_claim_token_created','device_claim_token_revoked')) FROM device_claim_tokens`).Scan(&tokens, &revoked, &audits); err != nil || tokens != 1 || revoked != 0 || audits != 0 {
					t.Fatalf("unauthorized mutation: %d/%d/%d %v", tokens, revoked, audits, err)
				}
			})
		}
	}
}

func TestPlatformClaimTokenAdministrationLocksManufacturerAndTargetClouds(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "token-manufacturer")
	other := handoffDeveloper(t, env, "token-other")
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin=true WHERE id=$1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	product, err := env.store.CreateDeviceItemProfile(ctx, DeviceItemProfileCreateInput{ActorUserID: &owner.User.ID, BrandCloudID: owner.BrandCloud.ID, ProfileKey: "token-product", DisplayName: "Product", Category: model.DeviceCategoryIPCamera, CAProfile: "ca", IssuerProfile: "issuer", ServiceOptions: []string{"mqtt"}})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := env.store.CreateOrganization(ctx, owner.User.ID, "Legacy customer")
	if err != nil {
		t.Fatal(err)
	}
	for _, cloud := range []string{owner.BrandCloud.ID, legacy.ID} {
		token, err := env.store.CreateDeviceClaimTokenAsPlatform(ctx, platformTokenInput(owner.User.ID, cloud, "product-token-"+cloud, &product.ID))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.store.RevokeDeviceClaimTokenAsPlatform(ctx, owner.User.ID, token.ID, time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.store.CreateDeviceClaimTokenAsPlatform(ctx, platformTokenInput(owner.User.ID, other.BrandCloud.ID, "wrong-product", &product.ID)); !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-cloud Product accepted: %v", err)
	}
	// Unbound manufacturing tokens do not invent a cloud membership.
	unbound := platformTokenInput(owner.User.ID, "", "unbound", nil)
	unbound.OrganizationID = nil
	token, err := env.store.CreateDeviceClaimTokenAsPlatform(ctx, unbound)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.RevokeDeviceClaimTokenAsPlatform(ctx, owner.User.ID, token.ID, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.RevokeDeviceClaimTokenAsPlatform(ctx, owner.User.ID, other.User.ID, time.Time{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing token: %v", err)
	}
	if _, err := env.store.CreateDeviceClaimTokenAsPlatform(ctx, platformTokenInput(owner.User.ID, owner.BrandCloud.ID, "missing-product", &other.User.ID)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Product: %v", err)
	}
	// Even a legacy customer token must respect its manufacturer's handoff fence.
	bound := claimAuthorizationToken(t, env, legacy.ID, "fenced-token", &product.ID)
	configureTestHandoff(t, env)
	handoffRequest(t, env, owner, other, "token-fence")
	op, err := env.store.AcceptBrandCloudOwnerTransfer(ctx, other.User.ID, "token-fence", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"preparing", "canceling"} {
		if phase == "canceling" {
			if _, err := env.store.CancelBrandCloudOwnerTransfer(ctx, BrandCloudOwnerTransferQuery{BrandCloudID: owner.BrandCloud.ID, TransferID: op.ID, RequesterID: owner.User.ID}, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		for _, cloud := range []string{owner.BrandCloud.ID, legacy.ID} {
			if _, err := env.store.CreateDeviceClaimTokenAsPlatform(ctx, platformTokenInput(owner.User.ID, cloud, "fenced-new-token", &product.ID)); !errors.Is(err, ErrNotFound) {
				t.Fatalf("%s creation escaped cloud fence: %v", phase, err)
			}
		}
		if _, err := env.store.RevokeDeviceClaimTokenAsPlatform(ctx, owner.User.ID, bound.ID, time.Time{}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s revocation escaped cloud fence: %v", phase, err)
		}
	}
	for _, participant := range append([]string{"billing"}, RequiredHandoffProducers()...) {
		if _, err := env.store.RecordCloudHandoffAbortAck(ctx, HandoffAbortAck{CloudID: owner.BrandCloud.ID, OperationID: op.ID, OwnershipVersion: 1, Participant: participant, ReceiptSHA256: strings.Repeat("a", 64)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.store.RevokeDeviceClaimTokenAsPlatform(ctx, owner.User.ID, bound.ID, time.Time{}); err != nil {
		t.Fatal(err)
	}
}

func TestPlatformClaimTokenAdministrationAuditAndCommitRollback(t *testing.T) {
	for _, action := range []string{"create", "revoke"} {
		for _, stage := range []string{"audit", "commit", "write"} {
			t.Run(action+"/"+stage, func(t *testing.T) {
				env := newStoreIntegrationEnv(t)
				ctx := context.Background()
				owner := handoffDeveloper(t, env, "token-rollback")
				if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin=true WHERE id=$1`, owner.User.ID); err != nil {
					t.Fatal(err)
				}
				in := platformTokenInput(owner.User.ID, owner.BrandCloud.ID, "sensitive-token-hash", nil)
				var token model.DeviceClaimToken
				if action == "revoke" {
					var err error
					token, err = env.store.CreateDeviceClaimToken(ctx, in)
					if err != nil {
						t.Fatal(err)
					}
				}
				write := func() (model.DeviceClaimToken, error) {
					if action == "create" {
						return env.store.CreateDeviceClaimTokenAsPlatform(ctx, in)
					}
					return env.store.RevokeDeviceClaimTokenAsPlatform(ctx, owner.User.ID, token.ID, time.Time{})
				}
				setup := `ALTER TABLE audit_events ADD CONSTRAINT token_audit_failure_test CHECK(event_type NOT IN ('device_claim_token_created','device_claim_token_revoked')) NOT VALID`
				cleanup := `ALTER TABLE audit_events DROP CONSTRAINT token_audit_failure_test`
				if stage == "write" {
					setup = `ALTER TABLE device_claim_tokens ADD CONSTRAINT token_write_failure_test CHECK(false) NOT VALID`
					cleanup = `ALTER TABLE device_claim_tokens DROP CONSTRAINT token_write_failure_test`
				}
				if stage == "commit" {
					setup = `CREATE FUNCTION reject_token_commit_test() RETURNS TRIGGER LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'isolated token rejection' USING ERRCODE='23514'; END $$;CREATE CONSTRAINT TRIGGER token_commit_failure_test AFTER INSERT OR UPDATE ON device_claim_tokens DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_token_commit_test()`
					cleanup = `DROP TRIGGER token_commit_failure_test ON device_claim_tokens;DROP FUNCTION reject_token_commit_test()`
				}
				if _, err := env.db.Exec(ctx, setup); err != nil {
					t.Fatal(err)
				}
				defer env.db.Exec(context.Background(), cleanup)
				_, err := write()
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
					t.Fatalf("did not reach failure: %v", err)
				}
				var count, revoked, audits int
				want := 0
				if action == "revoke" {
					want = 1
				}
				if err := env.db.QueryRow(ctx, `SELECT count(*),count(revoked_at),(SELECT count(*) FROM audit_events WHERE event_type LIKE 'device_claim_token_%') FROM device_claim_tokens`).Scan(&count, &revoked, &audits); err != nil || count != want || revoked != 0 || audits != 0 {
					t.Fatalf("partial token mutation: %d/%d/%d %v", count, revoked, audits, err)
				}
				if _, err := env.db.Exec(ctx, cleanup); err != nil {
					t.Fatal(err)
				}
				got, err := write()
				if err != nil {
					t.Fatal("retry failed", err)
				}
				if action == "revoke" {
					again, err := write()
					if err != nil || !again.RevokedAt.Equal(*got.RevokedAt) {
						t.Fatalf("revocation replay: %+v %v", again, err)
					}
				}
				var payload string
				if err := env.db.QueryRow(ctx, `SELECT count(*),string_agg(payload::text,'') FROM audit_events WHERE event_type LIKE 'device_claim_token_%'`).Scan(&audits, &payload); err != nil || audits != 1 || strings.Contains(payload, in.TokenHash) || strings.Contains(payload, in.ClipPublicKey) {
					t.Fatalf("audit duplicated or leaked secrets: count=%d err=%v", audits, err)
				}
			})
		}
	}
}

func TestPlatformTokenRevocationRejectsScopeChangeWhileWaiting(t *testing.T) {
	for _, change := range []string{"move", "delete"} {
		t.Run(change, func(t *testing.T) {
			env := newStoreIntegrationEnv(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			owner := handoffDeveloper(t, env, "token-scope-race")
			target, err := env.store.CreateOrganization(ctx, owner.User.ID, "Legacy recovery")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin=true WHERE id=$1`, owner.User.ID); err != nil {
				t.Fatal(err)
			}
			token := claimAuthorizationToken(t, env, owner.BrandCloud.ID, "token-scope-race", nil)
			tx, err := env.db.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			if _, err := tx.Exec(ctx, `SELECT 1 FROM organizations WHERE id=$1 FOR UPDATE`, owner.BrandCloud.ID); err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			go func() {
				_, err := env.store.RevokeDeviceClaimTokenAsPlatform(ctx, owner.User.ID, token.ID, time.Time{})
				result <- err
			}()
			for {
				var waiting bool
				if err := env.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'SELECT organization_kind FROM organizations%')`).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				select {
				case err := <-result:
					t.Fatalf("revocation escaped cloud lock: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(10 * time.Millisecond):
				}
			}
			expected := ErrConflict
			if change == "move" {
				_, err = tx.Exec(ctx, `UPDATE device_claim_tokens SET organization_id=$2 WHERE id=$1`, token.ID, target.ID)
			} else {
				_, err = tx.Exec(ctx, `DELETE FROM device_claim_tokens WHERE id=$1`, token.ID)
				expected = ErrNotFound
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-result:
				if !errors.Is(err, expected) {
					t.Fatalf("stale scope accepted: %v want %v", err, expected)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			var audits, revoked int
			if err := env.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM audit_events WHERE event_type='device_claim_token_revoked'),(SELECT count(*) FROM device_claim_tokens WHERE revoked_at IS NOT NULL)`).Scan(&audits, &revoked); err != nil || audits != 0 || revoked != 0 {
				t.Fatalf("stale scope mutated token: %d/%d %v", audits, revoked, err)
			}
		})
	}
}
