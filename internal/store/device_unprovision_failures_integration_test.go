package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestUnprovisionFailureKeepsDeviceAndDoesNotQueueWork(t *testing.T) {
	for _, stage := range []string{"actor", "evidence", "missing_platform_device", "disabled", "not_provisioned", "missing_claim", "operation", "outbox", "delete", "commit", "canceled"} {
		t.Run(stage, func(t *testing.T) {
			env := newStoreIntegrationEnv(t)
			ctx := context.Background()
			owner := handoffDeveloper(t, env, "unprovision-failure")
			claimAuthorizationToken(t, env, owner.BrandCloud.ID, "unprovision-failure", nil)
			claim, err := env.store.ResolveDeviceClaimToken(ctx, DeviceClaimResolveInput{RequestedBy: owner.User.ID, OrganizationID: owner.BrandCloud.ID, TokenHash: "unprovision-failure"})
			if err != nil {
				t.Fatal(err)
			}
			in := DeviceUnprovisionInput{ActorUserID: owner.User.ID, OrganizationID: owner.BrandCloud.ID, DeviceID: claim.Device.ID, Reason: "user_request"}
			var setup, cleanup string
			expected := ErrNotFound
			switch stage {
			case "actor":
				in.ActorUserID = ""
			case "evidence":
				in.PlatformOverride = true
				expected = ErrClaimEvidenceRequired
			case "missing_platform_device":
				in.PlatformOverride = true
				in.Evidence = map[string]any{"ticket": "test"}
				in.DeviceID = owner.User.ID
				setup = `UPDATE users SET platform_admin=true WHERE id='` + owner.User.ID + `'`
			case "disabled":
				setup = `UPDATE devices SET disabled_at=now() WHERE id='` + claim.Device.ID + `'`
				expected = ErrDisabled
			case "not_provisioned":
				setup = `UPDATE devices SET metadata='{}' WHERE id='` + claim.Device.ID + `'`
				expected = ErrNotProvisioned
			case "missing_claim":
				setup = `DELETE FROM device_claims WHERE id='` + claim.Claim.ID + `'`
				expected = ErrNotProvisioned
			case "operation":
				setup = `ALTER TABLE device_operations ADD CONSTRAINT unprovision_failure_test CHECK(operation_type<>'unprovision') NOT VALID`
				cleanup = `ALTER TABLE device_operations DROP CONSTRAINT unprovision_failure_test`
			case "outbox":
				setup = `ALTER TABLE device_message_outbox ADD CONSTRAINT unprovision_failure_test CHECK(false) NOT VALID`
				cleanup = `ALTER TABLE device_message_outbox DROP CONSTRAINT unprovision_failure_test`
			case "delete", "commit":
				setup = `CREATE FUNCTION reject_unprovision_test() RETURNS TRIGGER LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'isolated unprovision rejection' USING ERRCODE='23514'; END $$;`
				if stage == "delete" {
					setup += `CREATE TRIGGER unprovision_failure_test BEFORE DELETE ON devices FOR EACH ROW EXECUTE FUNCTION reject_unprovision_test()`
				} else {
					setup += `CREATE CONSTRAINT TRIGGER unprovision_failure_test AFTER DELETE ON devices DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_unprovision_test()`
				}
				cleanup = `DROP TRIGGER unprovision_failure_test ON devices; DROP FUNCTION reject_unprovision_test()`
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
			if cleanup != "" {
				defer env.db.Exec(context.Background(), cleanup)
			}
			_, err = env.store.UnprovisionDevice(ctx, in)
			if cleanup != "" {
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
					t.Fatalf("did not reach failure: %v", err)
				}
			} else if !errors.Is(err, expected) {
				t.Fatalf("wrong rejection: %v, want %v", err, expected)
			}
			var devices, operations, messages, audits int
			if err := env.db.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM devices WHERE id=$1),(SELECT count(*) FROM device_operations),(SELECT count(*) FROM device_message_outbox),(SELECT count(*) FROM audit_events WHERE event_type='device_unprovisioned')`, claim.Device.ID).Scan(&devices, &operations, &messages, &audits); err != nil || devices != 1 || operations != 0 || messages != 0 || audits != 0 {
				t.Fatalf("partial unprovision: %d/%d/%d/%d %v", devices, operations, messages, audits, err)
			}
			if cleanup != "" {
				if _, err := env.db.Exec(ctx, cleanup); err != nil {
					t.Fatal(err)
				}
				if _, err := env.store.UnprovisionDevice(ctx, in); err != nil {
					t.Fatal("retry failed", err)
				}
			}
		})
	}
}
