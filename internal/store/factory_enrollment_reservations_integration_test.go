package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"rtk_account_manager/internal/model"
)

func factoryAdmissionFixture(t *testing.T, env storeIntegrationEnv, quantity int) (DeveloperSignupResult, FactoryEnrollmentAdmission) {
	t.Helper()
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "factory-reservation")
	p, err := env.store.CreateDeviceItemProfileAsUser(ctx, authorizedProductInput(owner.User.ID, owner.BrandCloud.ID, "factory-reservation"))
	if err != nil {
		t.Fatal(err)
	}
	in := authorizedProductionInput(owner.User.ID, owner.BrandCloud.ID, p.ID)
	in.AllowedQuantity = quantity
	run, _, err := env.store.IssueProductionRunAsUser(ctx, in, func(model.ProductionRun, model.DeviceItemProfile) (string, error) {
		return "fixture-token-not-for-use", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return owner, FactoryEnrollmentAdmission{RunID: run.ID, CloudID: owner.BrandCloud.ID, ProductID: p.ID, RequestID: "request-1", DeviceID: "isolated-device", RequestSHA256: strings.Repeat("a", 64)}
}

func factoryResult(in FactoryEnrollmentAdmission, r FactoryEnrollmentReservation, status string) FactoryEnrollmentResult {
	return FactoryEnrollmentResult{CloudID: in.CloudID, RunID: in.RunID, ReservationID: r.ID, RequestSHA256: in.RequestSHA256, Status: status, EvidenceSHA256: strings.Repeat("b", 64)}
}

func TestFactoryEnrollmentReservationsSerializeQuotaAndReplayResults(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	_, in := factoryAdmissionFixture(t, env, 2)
	first, err := env.store.ReserveFactoryEnrollment(ctx, in)
	if err != nil || first.Status != "reserved" || first.CompletedAt != nil {
		t.Fatalf("reserve: %+v %v", first, err)
	}
	if replay, err := New(env.db).ReserveFactoryEnrollment(ctx, in); err != nil || replay.ID != first.ID {
		t.Fatalf("restart replay: %+v %v", replay, err)
	}
	for _, change := range []func(*FactoryEnrollmentAdmission){
		func(v *FactoryEnrollmentAdmission) { v.DeviceID = "different" },
		func(v *FactoryEnrollmentAdmission) { v.RequestSHA256 = strings.Repeat("c", 64) },
	} {
		bad := in
		change(&bad)
		if _, err := env.store.ReserveFactoryEnrollment(ctx, bad); !errors.Is(err, ErrConflict) {
			t.Fatalf("payload conflict: %v", err)
		}
	}
	var wg sync.WaitGroup
	results := make(chan FactoryEnrollmentReservation, 12)
	failures := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			next := in
			next.RequestID = fmt.Sprintf("parallel-%d", i)
			r, err := env.store.ReserveFactoryEnrollment(ctx, next)
			if err != nil {
				failures <- err
			} else {
				results <- r
			}
		}(i)
	}
	wg.Wait()
	close(results)
	close(failures)
	if len(results) != 1 || len(failures) != 11 {
		t.Fatalf("quota oversubscribed: admitted=%d denied=%d", len(results), len(failures))
	}
	for err := range failures {
		if !errors.Is(err, ErrProductionRunCapacity) {
			t.Fatal(err)
		}
	}
	second := <-results
	result := factoryResult(in, first, "issued")
	completed, err := env.store.CompleteFactoryEnrollment(ctx, result)
	if err != nil || completed.CompletedAt == nil || completed.Status != "issued" {
		t.Fatalf("complete: %+v %v", completed, err)
	}
	if replay, err := New(env.db).CompleteFactoryEnrollment(ctx, result); err != nil || !replay.CompletedAt.Equal(*completed.CompletedAt) {
		t.Fatalf("result replay: %+v %v", replay, err)
	}
	if replay, err := env.store.ReserveFactoryEnrollment(ctx, in); err != nil || replay.Status != "issued" {
		t.Fatalf("terminal admission replay: %+v %v", replay, err)
	}
	for _, change := range []func(*FactoryEnrollmentResult){
		func(v *FactoryEnrollmentResult) { v.Status = "not_issued" },
		func(v *FactoryEnrollmentResult) { v.EvidenceSHA256 = strings.Repeat("c", 64) },
		func(v *FactoryEnrollmentResult) { v.RequestSHA256 = strings.Repeat("c", 64) },
	} {
		bad := result
		change(&bad)
		if _, err := env.store.CompleteFactoryEnrollment(ctx, bad); !errors.Is(err, ErrConflict) {
			t.Fatalf("changed result accepted: %v", err)
		}
	}
	if _, err := env.store.CompleteFactoryEnrollment(ctx, factoryResult(in, second, "not_issued")); err != nil {
		t.Fatal(err)
	}
	next := in
	next.RequestID = "replacement"
	if _, err := env.store.ReserveFactoryEnrollment(ctx, next); err != nil {
		t.Fatal("definite non-issue did not free capacity", err)
	}
	var issued, audits int
	if err := env.db.QueryRow(ctx, `SELECT issued_quantity,(SELECT count(*) FROM audit_events WHERE subject_id=$2) FROM factory_production_runs WHERE id=$1`, in.RunID, first.ID).Scan(&issued, &audits); err != nil || issued != 1 || audits != 2 {
		t.Fatalf("double count/audit: %d %d %v", issued, audits, err)
	}
	for _, sql := range []string{
		`UPDATE factory_enrollment_reservations SET status='reserved',completed_at=NULL,evidence_sha256=NULL WHERE id=$1`,
		`DELETE FROM factory_enrollment_reservations WHERE id=$1`,
		`UPDATE factory_enrollment_reservations SET device_id='changed' WHERE id=$1`,
	} {
		if _, err := env.db.Exec(ctx, sql, first.ID); err == nil {
			t.Fatal("terminal evidence mutable")
		}
	}
}

func TestFactoryEnrollmentAdmissionRechecksAuthorityAndProvenance(t *testing.T) {
	for _, stage := range []string{"legacy_run", "stale_ownership", "revoked_actor", "pending_owner", "disabled_product", "disabled_run", "future_run", "expired_run", "missing_run", "wrong_cloud", "wrong_product", "no_actor", "invalid_request", "invalid_device", "invalid_digest", "canceled"} {
		t.Run(stage, func(t *testing.T) {
			env := newStoreIntegrationEnv(t)
			ctx := context.Background()
			owner, in := factoryAdmissionFixture(t, env, 1)
			var sql string
			expected := ErrNotFound
			switch stage {
			case "legacy_run":
				sql = `UPDATE factory_production_runs SET authorization_ownership_version=NULL`
			case "stale_ownership":
				sql = `UPDATE factory_production_runs SET authorization_ownership_version=authorization_ownership_version+1`
			case "revoked_actor":
				sql = `UPDATE users SET disabled_at=now()`
			case "pending_owner":
				sql = `UPDATE users SET signup_pending_verification=true`
			case "disabled_product":
				sql = `UPDATE device_item_profiles SET status='disabled',disabled_at=now()`
				expected = ErrDeviceItemProfileDisabled
			case "disabled_run":
				sql = `UPDATE factory_production_runs SET status='disabled'`
			case "future_run":
				sql = `UPDATE factory_production_runs SET valid_from=now()+interval '2 hours',valid_until=now()+interval '3 hours'`
			case "expired_run":
				sql = `UPDATE factory_production_runs SET valid_from=now()-interval '2 hours',valid_until=now()-interval '1 hour'`
			case "missing_run":
				in.RunID = owner.User.ID
			case "wrong_cloud":
				in.CloudID = owner.User.ID
			case "wrong_product":
				in.ProductID = owner.User.ID
			case "no_actor":
				sql = `UPDATE factory_production_runs SET created_by=NULL`
			case "invalid_request":
				in.RequestID = " "
				expected = ErrConflict
			case "invalid_device":
				in.DeviceID = strings.Repeat("a", 257)
				expected = ErrConflict
			case "invalid_digest":
				in.RequestSHA256 = "not-a-digest"
				expected = ErrConflict
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				expected = context.Canceled
			}
			if sql != "" {
				if _, err := env.db.Exec(ctx, sql); err != nil {
					t.Fatal(err)
				}
			}
			if r, err := env.store.ReserveFactoryEnrollment(ctx, in); !errors.Is(err, expected) || r.ID != "" {
				t.Fatalf("admission bypass: %+v %v want %v", r, err, expected)
			}
			var count int
			if err := env.db.QueryRow(context.Background(), `SELECT count(*) FROM factory_enrollment_reservations`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("denied admission wrote: %d %v", count, err)
			}
		})
	}
}

func TestFactoryEnrollmentPendingResultsBlockHandoffEvenWithAllReceipts(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	source, in := factoryAdmissionFixture(t, env, 2)
	r, err := env.store.ReserveFactoryEnrollment(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	configureTestHandoff(t, env)
	target := handoffDeveloper(t, env, "factory-target")
	op := handoffRequest(t, env, source, target, "factory-handoff")
	if _, err := env.store.AcceptBrandCloudOwnerTransfer(ctx, target.User.ID, "factory-handoff", time.Now()); err != nil {
		t.Fatal(err)
	}
	q := BrandCloudOwnerTransferQuery{BrandCloudID: in.CloudID, TransferID: op.ID, RequesterID: target.User.ID}
	state, err := env.store.GetCloudHandoffPreparation(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	ack := HandoffPrepareAck{OperationID: op.ID, CloudID: in.CloudID, SourceUserID: source.User.ID, TargetUserID: target.User.ID, OwnershipVersion: state.OwnershipVersion, Cutoff: state.Cutoff, HoldReceiptSHA256: strings.Repeat("c", 64), DrainCheckpointSHA256: strings.Repeat("d", 64)}
	// Synthetic remote receipts deliberately lie about draining. This test proves
	// the independent local guard, not a real factory participant integration.
	for _, participant := range append([]string{"billing"}, RequiredHandoffProducers()...) {
		ack.Participant = participant
		state, err = env.store.RecordCloudHandoffPrepareAck(ctx, ack)
		if err != nil {
			t.Fatal(err)
		}
	}
	if state.AllParticipantsPrepared || state.PendingFactoryEnrollments != 1 || len(state.MissingParticipants) != 0 {
		t.Fatalf("pending reservation escaped receipts: %+v", state)
	}
	remote := &commitTestBilling{balanceTestBilling: &balanceTestBilling{version: 2, confirmed: map[string]bool{}}}
	if err := env.store.ConfigureHandoffBilling(remote); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.PreviewOwnerHandoff(ctx, q); !errors.Is(err, ErrHandoffSnapshotNotReady) {
		t.Fatalf("pending issue allowed preview: %v", err)
	}
	if _, err := env.store.CommitOwnerHandoff(ctx, in.CloudID, op.ID); !errors.Is(err, ErrHandoffSnapshotNotReady) {
		t.Fatalf("pending issue allowed commit: %v", err)
	}
	if remote.reads != 0 {
		t.Fatal("pending issue reached financial confirmation")
	}
	if _, err := env.store.BeginFactoryEnrollmentCancellation(ctx, in); err != nil {
		t.Fatal("fence prevented trusted cancellation intent", err)
	}
	state, err = env.store.GetCloudHandoffPreparation(ctx, q)
	if err != nil || state.AllParticipantsPrepared || state.PendingFactoryEnrollments != 1 {
		t.Fatal("cancellation intent prematurely released handoff", err)
	}
	if _, err := env.store.PreviewOwnerHandoff(ctx, q); !errors.Is(err, ErrHandoffSnapshotNotReady) {
		t.Fatal("intent without issuer proof enabled billing confirmation")
	}
	if _, err := env.store.ReserveFactoryEnrollment(ctx, in); !errors.Is(err, ErrNotFound) {
		t.Fatalf("fenced replay allowed issuance: %v", err)
	}
	// Token/run expiry is not evidence of no issuance and does not clear a hold.
	if _, err := env.db.Exec(ctx, `UPDATE factory_production_runs SET valid_from=now()-interval '2 hours',valid_until=now()-interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	state, err = New(env.db).GetCloudHandoffPreparation(ctx, q)
	if err != nil || state.AllParticipantsPrepared || state.PendingFactoryEnrollments != 1 {
		t.Fatalf("expiry/restart released hold: %+v %v", state, err)
	}
	if _, err := New(env.db).CompleteFactoryEnrollment(ctx, factoryResult(in, r, "issued")); err != nil {
		t.Fatal("fence prevented reconciliation", err)
	}
	state, err = env.store.GetCloudHandoffPreparation(ctx, q)
	if err != nil || !state.AllParticipantsPrepared || state.PendingFactoryEnrollments != 0 {
		t.Fatalf("terminal outcome did not drain: %+v %v", state, err)
	}
	if _, err := env.store.PreviewOwnerHandoff(ctx, q); err != nil {
		t.Fatal(err)
	}
}

func TestFactoryEnrollmentWritesRollbackOnPersistenceFailure(t *testing.T) {
	for _, stage := range []string{"reserve_write", "reserve_audit", "reserve_commit", "result_write", "result_audit", "result_commit", "counter_write"} {
		t.Run(stage, func(t *testing.T) {
			env := newStoreIntegrationEnv(t)
			ctx := context.Background()
			_, in := factoryAdmissionFixture(t, env, 1)
			var r FactoryEnrollmentReservation
			resultStage := !strings.HasPrefix(stage, "reserve_")
			if resultStage {
				var err error
				r, err = env.store.ReserveFactoryEnrollment(ctx, in)
				if err != nil {
					t.Fatal(err)
				}
			}
			var setup, cleanup string
			switch stage {
			case "reserve_write":
				setup = `ALTER TABLE factory_enrollment_reservations ADD CONSTRAINT isolated_failure CHECK(false) NOT VALID`
				cleanup = `ALTER TABLE factory_enrollment_reservations DROP CONSTRAINT isolated_failure`
			case "result_write":
				setup = `ALTER TABLE factory_enrollment_reservations ADD CONSTRAINT isolated_failure CHECK(status='reserved') NOT VALID`
				cleanup = `ALTER TABLE factory_enrollment_reservations DROP CONSTRAINT isolated_failure`
			case "counter_write":
				setup = `ALTER TABLE factory_production_runs ADD CONSTRAINT isolated_failure CHECK(issued_quantity=0) NOT VALID`
				cleanup = `ALTER TABLE factory_production_runs DROP CONSTRAINT isolated_failure`
			case "reserve_audit", "result_audit":
				setup = `ALTER TABLE audit_events ADD CONSTRAINT isolated_failure CHECK(subject_type<>'factory_enrollment_reservation') NOT VALID`
				cleanup = `ALTER TABLE audit_events DROP CONSTRAINT isolated_failure`
			case "reserve_commit", "result_commit":
				setup = `CREATE FUNCTION reject_factory_result_test() RETURNS TRIGGER LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'isolated factory rejection' USING ERRCODE='23514'; END $$; CREATE CONSTRAINT TRIGGER isolated_factory_failure AFTER INSERT OR UPDATE ON factory_enrollment_reservations DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_factory_result_test()`
				cleanup = `DROP TRIGGER isolated_factory_failure ON factory_enrollment_reservations;DROP FUNCTION reject_factory_result_test()`
			}
			if _, err := env.db.Exec(ctx, setup); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if _, err := env.db.Exec(ctx, cleanup); err != nil {
					t.Error(err)
				}
			})
			var out FactoryEnrollmentReservation
			var err error
			if resultStage {
				out, err = env.store.CompleteFactoryEnrollment(ctx, factoryResult(in, r, "issued"))
			} else {
				out, err = env.store.ReserveFactoryEnrollment(ctx, in)
			}
			if err == nil || out.ID != "" {
				t.Fatalf("failure exposed result: %+v %v", out, err)
			}
			var issued, terminal, pending int
			if err := env.db.QueryRow(ctx, `SELECT issued_quantity,(SELECT count(*) FROM factory_enrollment_reservations WHERE status<>'reserved'),(SELECT count(*) FROM factory_enrollment_reservations WHERE status='reserved') FROM factory_production_runs WHERE id=$1`, in.RunID).Scan(&issued, &terminal, &pending); err != nil || issued != 0 || terminal != 0 || (resultStage && pending != 1) || (!resultStage && pending != 0) {
				t.Fatalf("partial commit: %d %d %d %v", issued, terminal, pending, err)
			}
		})
	}
}

func TestFactoryEnrollmentResultValidationAndConcurrentCompletion(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner, in := factoryAdmissionFixture(t, env, 1)
	r, err := env.store.ReserveFactoryEnrollment(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	result := factoryResult(in, r, "issued")
	for _, stage := range []string{"unknown_outcome", "missing_evidence", "missing_digest", "missing_cloud", "missing_run", "missing_reservation", "wrong_digest", "inconsistent_counter", "canceled"} {
		t.Run(stage, func(t *testing.T) {
			bad := result
			ctx := context.Background()
			expected := ErrConflict
			switch stage {
			case "unknown_outcome":
				bad.Status = "timeout"
			case "missing_evidence":
				bad.EvidenceSHA256 = ""
			case "missing_digest":
				bad.RequestSHA256 = ""
			case "missing_cloud":
				bad.CloudID = owner.User.ID
				expected = ErrNotFound
			case "missing_run":
				bad.RunID = owner.User.ID
				expected = ErrNotFound
			case "missing_reservation":
				bad.ReservationID = owner.User.ID
				expected = ErrNotFound
			case "wrong_digest":
				bad.RequestSHA256 = strings.Repeat("c", 64)
			case "inconsistent_counter":
				if _, err := env.db.Exec(ctx, `UPDATE factory_production_runs SET issued_quantity=allowed_quantity`); err != nil {
					t.Fatal(err)
				}
				defer func() {
					if _, err := env.db.Exec(ctx, `UPDATE factory_production_runs SET issued_quantity=0`); err != nil {
						t.Error(err)
					}
				}()
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				expected = context.Canceled
			}
			if out, err := env.store.CompleteFactoryEnrollment(ctx, bad); !errors.Is(err, expected) || out.ID != "" {
				t.Fatalf("invalid result: %+v %v want %v", out, err, expected)
			}
		})
	}
	var wg sync.WaitGroup
	outcomes := make(chan error, 10)
	for range 10 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := env.store.CompleteFactoryEnrollment(ctx, result); outcomes <- err }()
	}
	wg.Wait()
	close(outcomes)
	for err := range outcomes {
		if err != nil {
			t.Fatal(err)
		}
	}
	var issued, audit int
	if err := env.db.QueryRow(ctx, `SELECT issued_quantity,(SELECT count(*) FROM audit_events WHERE subject_id=$2 AND event_type='factory_enrollment_issued') FROM factory_production_runs WHERE id=$1`, in.RunID, r.ID).Scan(&issued, &audit); err != nil || issued != 1 || audit != 1 {
		t.Fatalf("concurrent completion counted twice: %d %d %v", issued, audit, err)
	}
}

func TestFactoryEnrollmentPlatformAuthorityIsPersistedAndRechecked(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	_, in := factoryAdmissionFixture(t, env, 2)
	operator := handoffDeveloper(t, env, "factory-platform")
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin=true WHERE id=$1`, operator.User.ID); err != nil {
		t.Fatal(err)
	}
	create := authorizedProductionInput(operator.User.ID, in.CloudID, in.ProductID)
	create.PlatformOverride = true
	run, _, err := env.store.IssueProductionRunAsUser(ctx, create, func(model.ProductionRun, model.DeviceItemProfile) (string, error) {
		return "isolated-platform-run", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	in.RunID = run.ID
	if _, err := env.store.ReserveFactoryEnrollment(ctx, in); err != nil {
		t.Fatal("explicit platform run rejected", err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin=false WHERE id=$1`, operator.User.ID); err != nil {
		t.Fatal(err)
	}
	in.RequestID = "revoked-operator"
	if _, err := env.store.ReserveFactoryEnrollment(ctx, in); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked operator token accepted: %v", err)
	}
}

func TestFactoryEnrollmentRechecksRunAfterWaitingForCloudLock(t *testing.T) {
	for _, change := range []string{"disabled", "different_creator", "different_product", "deleted"} {
		t.Run(change, func(t *testing.T) {
			env := newStoreIntegrationEnv(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			owner, in := factoryAdmissionFixture(t, env, 1)
			other := handoffDeveloper(t, env, "factory-other")
			tx, err := env.db.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			if _, err := tx.Exec(ctx, `SELECT 1 FROM organizations WHERE id=$1 FOR UPDATE`, in.CloudID); err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			go func() { _, err := env.store.ReserveFactoryEnrollment(ctx, in); result <- err }()
			for {
				var waiting bool
				if err := env.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'SELECT id::text FROM organizations%')`).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				select {
				case err := <-result:
					t.Fatalf("escaped admission lock: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(10 * time.Millisecond):
				}
			}
			var sql string
			switch change {
			case "disabled":
				sql = `UPDATE factory_production_runs SET status='disabled' WHERE id=$1`
			case "different_creator":
				sql = `UPDATE factory_production_runs SET created_by=$2 WHERE id=$1`
			case "different_product":
				sql = `UPDATE factory_production_runs SET device_item_profile_id=(SELECT id FROM device_item_profiles WHERE brand_cloud_id=$2 LIMIT 1) WHERE id=$1`
			case "deleted":
				sql = `DELETE FROM factory_production_runs WHERE id=$1`
			}
			if change == "different_creator" {
				_, err = tx.Exec(ctx, sql, in.RunID, other.User.ID)
			} else if change == "different_product" {
				// A second same-cloud Product is trusted fixture setup; the run scope
				// changing while a request waits must still fail, not be retargeted.
				var product string
				err = tx.QueryRow(ctx, `INSERT INTO device_item_profiles(brand_cloud_id,profile_key,display_name,category,ca_profile,issuer_profile) VALUES($1,'other','Other','ip_camera','ca','issuer') RETURNING id::text`, owner.BrandCloud.ID).Scan(&product)
				if err == nil {
					_, err = tx.Exec(ctx, `UPDATE factory_production_runs SET device_item_profile_id=$2 WHERE id=$1`, in.RunID, product)
				}
			} else {
				_, err = tx.Exec(ctx, sql, in.RunID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-result; !errors.Is(err, ErrNotFound) {
				t.Fatalf("stale run admitted: %v", err)
			}
		})
	}
}
