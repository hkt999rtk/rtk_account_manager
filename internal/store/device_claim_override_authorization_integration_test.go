package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"rtk_account_manager/internal/model"
)

func invokeClaimOverride(s *Store, ctx context.Context, action, actor, target string, claim model.DeviceClaim) (DeviceClaimOverrideResult, error) {
	if action == "reclaim" {
		return s.ReclaimDeviceClaimToken(ctx, DeviceClaimReclaimInput{TokenID: claim.TokenID, TargetOrganizationID: target, ActorUserID: actor, Reason: "support verified", Evidence: map[string]any{"ticket": "isolated-override"}})
	}
	return s.TransferDeviceClaim(ctx, DeviceClaimTransferInput{ClaimID: claim.ID, TargetOrganizationID: target, ActorUserID: actor, Reason: "support verified", Evidence: map[string]any{"ticket": "isolated-override"}})
}

func TestClaimOverridesRequirePlatformAuthorityOperationalCloudsAndProduct(t *testing.T) {
	for _, action := range []string{"transfer", "reclaim"} {
		t.Run(action, func(t *testing.T) {
			env := newStoreIntegrationEnv(t)
			ctx := context.Background()
			source := handoffDeveloper(t, env, "override-source")
			target := handoffDeveloper(t, env, "override-target")
			operator := handoffDeveloper(t, env, "override-operator")
			product, err := env.store.CreateDeviceItemProfile(ctx, DeviceItemProfileCreateInput{ActorUserID: &source.User.ID, BrandCloudID: source.BrandCloud.ID, ProfileKey: "override-product", DisplayName: "Override Product", Category: model.DeviceCategoryIPCamera, CAProfile: "ca", IssuerProfile: "issuer", ServiceOptions: []string{"mqtt"}})
			if err != nil {
				t.Fatal(err)
			}
			claimAuthorizationToken(t, env, source.BrandCloud.ID, "override-product-claim", &product.ID)
			resolved, err := env.store.ResolveDeviceClaimToken(ctx, DeviceClaimResolveInput{RequestedBy: source.User.ID, OrganizationID: source.BrandCloud.ID, TokenHash: "override-product-claim"})
			if err != nil {
				t.Fatal(err)
			}
			deny := func(expected error) {
				t.Helper()
				if _, err := invokeClaimOverride(env.store, ctx, action, operator.User.ID, target.BrandCloud.ID, resolved.Claim); !errors.Is(err, expected) {
					t.Fatalf("override bypass: %v, want %v", err, expected)
				}
			}
			deny(ErrNotFound)
			if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin=true WHERE id=$1`, operator.User.ID); err != nil {
				t.Fatal(err)
			}
			deny(ErrConflict) // Cannot keep the source Product under the destination cloud.
			for _, owner := range []string{source.User.ID, target.User.ID} {
				if _, err := env.db.Exec(ctx, `UPDATE users SET signup_pending_verification=true WHERE id=$1`, owner); err != nil {
					t.Fatal(err)
				}
				deny(ErrNotFound)
				if _, err := env.db.Exec(ctx, `UPDATE users SET signup_pending_verification=false WHERE id=$1`, owner); err != nil {
					t.Fatal(err)
				}
			}
			configureTestHandoff(t, env)
			for _, which := range []string{"source", "target"} {
				from, to := source, target
				if which == "target" {
					from, to = target, source
				}
				key := "override-fence-" + which
				handoffRequest(t, env, from, to, key)
				transfer, err := env.store.AcceptBrandCloudOwnerTransfer(ctx, to.User.ID, key, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				deny(ErrNotFound)
				if _, err := env.store.CancelBrandCloudOwnerTransfer(ctx, BrandCloudOwnerTransferQuery{BrandCloudID: from.BrandCloud.ID, TransferID: transfer.ID, RequesterID: from.User.ID}, time.Now()); err != nil {
					t.Fatal(err)
				}
				deny(ErrNotFound)
				for _, participant := range []string{"billing", "test_resources"} {
					if _, err := env.store.RecordCloudHandoffAbortAck(ctx, HandoffAbortAck{CloudID: from.BrandCloud.ID, OperationID: transfer.ID, OwnershipVersion: 1, Participant: participant, ReceiptSHA256: strings.Repeat("a", 64)}); err != nil {
						t.Fatal(err)
					}
				}
			}
			// An audited same-cloud recovery keeps the Product and unique owner.
			got, err := invokeClaimOverride(env.store, ctx, action, operator.User.ID, source.BrandCloud.ID, resolved.Claim)
			if err != nil || got.Device.DeviceItemProfileID == nil || *got.Device.DeviceItemProfileID != product.ID || got.Device.OrganizationID != source.BrandCloud.ID {
				t.Fatalf("same-cloud recovery failed: %+v %v", got, err)
			}
			var owner string
			if err := env.db.QueryRow(ctx, `SELECT user_id::text FROM organization_members WHERE organization_id=$1 AND role='owner'`, source.BrandCloud.ID).Scan(&owner); err != nil || owner != source.User.ID {
				t.Fatalf("claim changed cloud owner: %s %v", owner, err)
			}
		})
	}
}

func TestClaimOverrideWriteFailuresRollBackThenRetry(t *testing.T) {
	for _, stage := range []string{"device", "token", "claim", "audit", "commit", "duplicate_serial"} {
		t.Run(stage, func(t *testing.T) {
			env := newStoreIntegrationEnv(t)
			ctx := context.Background()
			source := handoffDeveloper(t, env, "override-rollback")
			target, err := env.store.CreateOrganization(ctx, source.User.ID, "Legacy recovery")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin=true WHERE id=$1`, source.User.ID); err != nil {
				t.Fatal(err)
			}
			claimAuthorizationToken(t, env, source.BrandCloud.ID, "rollback-claim", nil)
			resolved, err := env.store.ResolveDeviceClaimToken(ctx, DeviceClaimResolveInput{RequestedBy: source.User.ID, OrganizationID: source.BrandCloud.ID, TokenHash: "rollback-claim"})
			if err != nil {
				t.Fatal(err)
			}
			var setup, cleanup string
			switch stage {
			case "device":
				setup = `ALTER TABLE devices ADD CONSTRAINT override_failure_test CHECK(organization_id<>'` + target.ID + `') NOT VALID`
				cleanup = `ALTER TABLE devices DROP CONSTRAINT override_failure_test`
			case "token":
				setup = `ALTER TABLE device_claim_tokens ADD CONSTRAINT override_failure_test CHECK(organization_id<>'` + target.ID + `') NOT VALID`
				cleanup = `ALTER TABLE device_claim_tokens DROP CONSTRAINT override_failure_test`
			case "claim":
				setup = `ALTER TABLE device_claims ADD CONSTRAINT override_failure_test CHECK(status='resolved') NOT VALID`
				cleanup = `ALTER TABLE device_claims DROP CONSTRAINT override_failure_test`
			case "audit":
				setup = `ALTER TABLE audit_events ADD CONSTRAINT override_failure_test CHECK(event_type<>'device_claim_transferred') NOT VALID`
				cleanup = `ALTER TABLE audit_events DROP CONSTRAINT override_failure_test`
			case "commit":
				setup = `CREATE FUNCTION reject_override_commit_test() RETURNS TRIGGER LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'isolated commit rejection' USING ERRCODE='23514'; END $$;
				CREATE CONSTRAINT TRIGGER override_failure_test AFTER UPDATE ON device_claims DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_override_commit_test()`
				cleanup = `DROP TRIGGER override_failure_test ON device_claims; DROP FUNCTION reject_override_commit_test()`
			case "duplicate_serial":
				setup = fmt.Sprintf(`UPDATE devices SET serial_number='override-collision' WHERE id='%s'; INSERT INTO devices(organization_id,name,category,serial_number) VALUES('%s','Collision','ip_camera','override-collision')`, resolved.Device.ID, target.ID)
				cleanup = `UPDATE devices SET serial_number=NULL WHERE organization_id='` + target.ID + `'`
			}
			if _, err := env.db.Exec(ctx, setup); err != nil {
				t.Fatal(err)
			}
			defer env.db.Exec(context.Background(), cleanup)
			_, err = invokeClaimOverride(env.store, ctx, "transfer", source.User.ID, target.ID, resolved.Claim)
			var pgErr *pgconn.PgError
			if stage == "duplicate_serial" {
				if !errors.Is(err, ErrConflict) {
					t.Fatalf("duplicate error: %v", err)
				}
			} else if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
				t.Fatalf("did not reach intended rejection: %v", err)
			}
			var deviceCloud, tokenCloud, status string
			var audits int
			if err := env.db.QueryRow(ctx, `SELECT d.organization_id::text,t.organization_id::text,c.status,(SELECT count(*) FROM audit_events WHERE event_type='device_claim_transferred') FROM devices d JOIN device_claims c ON c.device_id=d.id JOIN device_claim_tokens t ON t.id=c.claim_token_id WHERE c.id=$1`, resolved.Claim.ID).Scan(&deviceCloud, &tokenCloud, &status, &audits); err != nil || deviceCloud != source.BrandCloud.ID || tokenCloud != source.BrandCloud.ID || status != "resolved" || audits != 0 {
				t.Fatalf("partial override: %s/%s/%s/%d %v", deviceCloud, tokenCloud, status, audits, err)
			}
			if _, err := env.db.Exec(ctx, cleanup); err != nil {
				t.Fatal(err)
			}
			if _, err := invokeClaimOverride(env.store, ctx, "transfer", source.User.ID, target.ID, resolved.Claim); err != nil {
				t.Fatal("retry failed", err)
			}
		})
	}
}

func TestClaimOverridesLockOppositeCloudMovesInTheSameOrder(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	first, err := env.store.Register(ctx, RegisterInput{Email: "override-race-a@example.test", PasswordHash: "fixture", OrganizationName: "Legacy A"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := env.store.Register(ctx, RegisterInput{Email: "override-race-b@example.test", PasswordHash: "fixture", OrganizationName: "Legacy B"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin=true WHERE id=ANY($1::uuid[])`, []string{first.User.ID, second.User.ID}); err != nil {
		t.Fatal(err)
	}
	claim := func(actor, cloud, hash string) model.DeviceClaim {
		t.Helper()
		claimAuthorizationToken(t, env, cloud, hash, nil)
		got, err := env.store.ResolveDeviceClaimToken(ctx, DeviceClaimResolveInput{RequestedBy: actor, OrganizationID: cloud, TokenHash: hash})
		if err != nil {
			t.Fatal(err)
		}
		return got.Claim
	}
	a := claim(first.User.ID, first.Organization.ID, "override-race-a")
	b := claim(second.User.ID, second.Organization.ID, "override-race-b")
	clouds := []string{first.Organization.ID, second.Organization.ID}
	slices.Sort(clouds)
	tx, err := env.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `SELECT 1 FROM organizations WHERE id=$1 FOR UPDATE`, clouds[0]); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	go func() {
		_, err := invokeClaimOverride(env.store, ctx, "transfer", first.User.ID, second.Organization.ID, a)
		results <- err
	}()
	go func() {
		_, err := invokeClaimOverride(env.store, ctx, "reclaim", second.User.ID, first.Organization.ID, b)
		results <- err
	}()
	for {
		var waiting int
		if err := env.db.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'SELECT organization_kind FROM organizations%'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= 2 {
			break
		}
		select {
		case err := <-results:
			t.Fatalf("override escaped ordered cloud locks: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	var targetA, targetB string
	if err := env.db.QueryRow(ctx, `SELECT (SELECT organization_id::text FROM devices WHERE id=$1),(SELECT organization_id::text FROM devices WHERE id=$2)`, a.DeviceID, b.DeviceID).Scan(&targetA, &targetB); err != nil || targetA != second.Organization.ID || targetB != first.Organization.ID {
		t.Fatalf("opposite moves: %s/%s %v", targetA, targetB, err)
	}
}
