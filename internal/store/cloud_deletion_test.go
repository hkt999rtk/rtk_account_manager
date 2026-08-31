package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"rtk_account_manager/internal/billinghandoff"
)

// Synthetic durable producer/Billing fixture. Real transport and provider
// reconciliation acceptance must be tested independently before deployment.
type deletionCoordinatorFixture struct {
	mu        sync.Mutex
	bindings  map[string]billinghandoff.ClosureBinding
	commands  map[string]string
	ack       map[string]billinghandoff.ClosureAcknowledgment
	failReply bool
	notReady  bool
	wrongHold bool
}

func (f *deletionCoordinatorFixture) PrepareCloudClosure(_ context.Context, in billinghandoff.ClosureBinding, sha string) (billinghandoff.ClosureOperation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bindings[in.OperationID] = in
	return billinghandoff.ClosureOperation{ID: in.OperationID, OwnerUserID: in.OwnerUserID, OwnershipVersion: in.OwnershipVersion, Cutoff: in.Cutoff, Phase: "preparing"}, nil
}
func (f *deletionCoordinatorFixture) CloudClosureStatus(_ context.Context, in billinghandoff.ClosureBinding) (billinghandoff.ClosureStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return billinghandoff.ClosureStatus{Ready: !f.notReady, ReceiptID: in.OperationID, Blockers: []string{}}, nil
}
func (f *deletionCoordinatorFixture) CloseCloud(_ context.Context, in billinghandoff.ClosureBinding, receipt, sha string) (billinghandoff.ClosureAcknowledgment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if prior := f.commands[in.OperationID]; prior != "" && prior != receipt+sha {
		return billinghandoff.ClosureAcknowledgment{}, errors.New("changed durable close command")
	}
	f.commands[in.OperationID] = receipt + sha
	ack, ok := f.ack[in.OperationID]
	if !ok {
		ack = billinghandoff.ClosureAcknowledgment{OperationID: in.OperationID, Phase: "closed", ClosedAt: time.Now().UTC(), ReceiptSHA256: strings.Repeat("c", 64)}
		f.ack[in.OperationID] = ack
	}
	if f.failReply {
		f.failReply = false
		return billinghandoff.ClosureAcknowledgment{}, errors.New("lost reply after Billing close")
	}
	return ack, nil
}
func (f *deletionCoordinatorFixture) PrepareCloudDeletion(_ context.Context, in billinghandoff.ClosureBinding, version int64) (CloudDeletionHold, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.wrongHold {
		version++
	}
	// Equivalent RFC3339 offsets must not be mistaken for another cutoff.
	in.Cutoff = in.Cutoff.In(time.FixedZone("fixture-offset", 8*60*60))
	return CloudDeletionHold{Binding: in, AuthorizationVersion: version, Participant: "test_resources", Held: true, Empty: true, ReceiptSHA256: strings.Repeat("a", 64)}, nil
}
func configureDeletionFixture(t *testing.T, s *Store, f *deletionCoordinatorFixture) {
	t.Helper()
	if err := s.ConfigureCloudDeletionPreflight(CloudDeletionPreflightOptions{Billing: deletionBillingFunc(syntheticDeletionBilling), Resources: map[string]CloudDeletionResourceObserver{"test_resources": deletionResourceFunc(syntheticDeletionResources)}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfigureCloudDeletion(CloudDeletionOptions{Billing: f, Producers: map[string]CloudDeletionProducer{"test_resources": f}}); err != nil {
		t.Fatal(err)
	}
}
func newDeletionCoordinatorFixture() *deletionCoordinatorFixture {
	return &deletionCoordinatorFixture{bindings: map[string]billinghandoff.ClosureBinding{}, commands: map[string]string{}, ack: map[string]billinghandoff.ClosureAcknowledgment{}}
}

func deleteEmptyCloudFixture(t *testing.T, env storeIntegrationEnv, user, cloud string) {
	t.Helper()
	s := New(env.db)
	configureDeletionFixture(t, s, newDeletionCoordinatorFixture())
	op, err := s.RequestDeveloperCloudDeletion(context.Background(), user, cloud, "fixture-delete")
	if err != nil {
		t.Fatal(err)
	}
	op, err = s.AdvanceCloudDeletion(context.Background(), cloud, op.ID)
	if err != nil || op.State != "succeeded" {
		t.Fatalf("fixture deletion: %+v %v", op, err)
	}
}

func TestCloudDeletionDurableReplayAndLostBillingReply(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "delete-durable")
	cloud := owner.BrandCloud.ID
	if _, err := env.db.Exec(ctx, `INSERT INTO device_operations(operation_id,correlation_id,organization_id,device_id,operation_type,status,completed_at) VALUES('deletion-history','deletion-history',$1,gen_random_uuid(),'unprovision','succeeded',now())`, cloud); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `INSERT INTO role_assignments(role_id,actor_type,actor_id,scope_type,scope_id,organization_id) SELECT id,'user',$1,'organization',$2::text,$2::text::uuid FROM roles WHERE name='admin'`, owner.User.ID, cloud); err != nil {
		t.Fatal(err)
	}
	f := newDeletionCoordinatorFixture()
	f.failReply = true
	configureDeletionFixture(t, env.store, f)
	op, err := env.store.RequestDeveloperCloudDeletion(ctx, owner.User.ID, cloud, "delete-1")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := env.store.RequestDeveloperCloudDeletion(ctx, owner.User.ID, cloud, "delete-1")
	if err != nil || replay.ID != op.ID {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	if _, err = env.store.RequestDeveloperCloudDeletion(ctx, owner.User.ID, cloud, "different"); !errors.Is(err, ErrConflict) {
		t.Fatalf("different key: %v", err)
	}
	var allowed bool
	if err = env.db.QueryRow(ctx, `SELECT user_can_access_brand_cloud($1,$2)`, owner.User.ID, cloud).Scan(&allowed); err != nil || allowed {
		t.Fatalf("fence not installed: %v %v", allowed, err)
	}
	_, err = env.db.Exec(ctx, `INSERT INTO device_item_profiles(brand_cloud_id,profile_key,display_name,status,category,ca_profile,issuer_profile) VALUES($1,'after','after','active','generic','ca','issuer')`, cloud)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.ConstraintName != "cloud_deletion_fence" {
		t.Fatalf("new Product bypassed fence: %v", err)
	}
	_, err = env.db.Exec(ctx, `UPDATE device_operations SET status='retrying',completed_at=NULL WHERE operation_id='deletion-history'`)
	if !errors.As(err, &pgErr) || pgErr.ConstraintName != "cloud_deletion_fence" {
		t.Fatalf("historical job reactivated: %v", err)
	}
	op, err = env.store.AdvanceCloudDeletion(ctx, cloud, op.ID)
	if err == nil || op.Phase != "closing" || op.State != "blocked" {
		t.Fatalf("lost reply: %+v %v", op, err)
	}
	var deleted bool
	if err = env.db.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM organizations WHERE id=$1`, cloud).Scan(&deleted); err != nil || deleted {
		t.Fatalf("premature tombstone: %v %v", deleted, err)
	}
	restarted := New(env.db)
	configureDeletionFixture(t, restarted, f)
	op, err = restarted.AdvanceCloudDeletion(ctx, cloud, op.ID)
	if err != nil || op.State != "succeeded" {
		t.Fatalf("forward retry: %+v %v", op, err)
	}
	for i := 0; i < 2; i++ {
		replay, err = restarted.RequestDeveloperCloudDeletion(ctx, owner.User.ID, cloud, "delete-1")
		if err != nil || replay.ID != op.ID || replay.State != "succeeded" {
			t.Fatalf("deleted replay: %+v %v", replay, err)
		}
	}
	if _, err = restarted.GetManagedBrandCloud(ctx, owner.User.ID, cloud); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted cloud returned: %v", err)
	}
	// Even a stale delegated ACL restored outside the normal API cannot make a
	// tombstoned cloud accessible. Global sessions on other clouds are untouched.
	if _, err = env.db.Exec(ctx, `UPDATE role_assignments SET disabled_at=NULL WHERE organization_id=$1`, cloud); err != nil {
		t.Fatal(err)
	}
	if err = env.db.QueryRow(ctx, `SELECT user_can_access_brand_cloud($1,$2)`, owner.User.ID, cloud).Scan(&allowed); err != nil || allowed {
		t.Fatalf("stale ACL resurrected cloud: %v %v", allowed, err)
	}
	if _, err = restarted.GetOrganization(ctx, cloud, owner.User.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale ACL exposes cloud: %v", err)
	}
	view, err := restarted.GetDeveloperCloudDeletion(ctx, owner.User.ID, cloud, op.ID)
	if err != nil || view.State != "succeeded" {
		t.Fatalf("operation history unavailable: %+v %v", view, err)
	}
	_, err = env.db.Exec(ctx, `UPDATE organizations SET deleted_at=NULL WHERE id=$1`, cloud)
	if !errors.As(err, &pgErr) || pgErr.ConstraintName != "cloud_deletion_fence" {
		t.Fatalf("cloud restored: %v", err)
	}
}

func TestCloudDeletionRequiresEvidenceAndPreservesAuditAtomicity(t *testing.T) {
	for _, failure := range []string{"producer", "billing", "audit"} {
		t.Run(failure, func(t *testing.T) {
			env := newStoreIntegrationEnv(t)
			ctx := context.Background()
			owner := handoffDeveloper(t, env, "delete-evidence")
			f := newDeletionCoordinatorFixture()
			configureDeletionFixture(t, env.store, f)
			op, err := env.store.RequestDeveloperCloudDeletion(ctx, owner.User.ID, owner.BrandCloud.ID, "delete-evidence")
			if err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "producer":
				f.wrongHold = true
			case "billing":
				f.notReady = true
			case "audit":
				if _, err = env.db.Exec(ctx, `ALTER TABLE audit_events ADD CONSTRAINT test_deletion_audit CHECK(event_type<>'brand_cloud_deletion_succeeded') NOT VALID`); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					_, _ = env.db.Exec(context.Background(), `ALTER TABLE audit_events DROP CONSTRAINT IF EXISTS test_deletion_audit`)
				})
			}
			if _, err = env.store.AdvanceCloudDeletion(ctx, op.CloudID, op.ID); err == nil {
				t.Fatal("missing evidence/audit succeeded")
			}
			var deleted bool
			if err = env.db.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM organizations WHERE id=$1`, op.CloudID).Scan(&deleted); err != nil || deleted {
				t.Fatalf("partial tombstone: %v %v", deleted, err)
			}
			f.wrongHold = false
			f.notReady = false
			if failure == "audit" {
				if _, err = env.db.Exec(ctx, `ALTER TABLE audit_events DROP CONSTRAINT test_deletion_audit`); err != nil {
					t.Fatal(err)
				}
			}
			op, err = env.store.AdvanceCloudDeletion(ctx, op.CloudID, op.ID)
			if err != nil || op.State != "succeeded" {
				t.Fatalf("recovery: %+v %v", op, err)
			}
		})
	}
}

func TestCloudDeletionOwnerOnlyAndConcurrentAdmission(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "delete-owner-only")
	other := handoffDeveloper(t, env, "delete-other")
	f := newDeletionCoordinatorFixture()
	configureDeletionFixture(t, env.store, f)
	if _, err := env.store.RequestDeveloperCloudDeletion(ctx, other.User.ID, owner.BrandCloud.ID, "other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other owner admitted: %v", err)
	}
	start := make(chan struct{})
	out := make(chan CloudDeletionOperation, 8)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			<-start
			op, err := env.store.RequestDeveloperCloudDeletion(ctx, owner.User.ID, owner.BrandCloud.ID, "concurrent")
			out <- op
			errs <- err
		}()
	}
	close(start)
	var id string
	for i := 0; i < 8; i++ {
		op := <-out
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		if id != "" && id != op.ID {
			t.Fatal("duplicate operation")
		}
		id = op.ID
	}
	if _, err := env.store.GetDeveloperCloudDeletion(ctx, other.User.ID, owner.BrandCloud.ID, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other owner read: %v", err)
	}
}

func TestCloudDeletionLeasedJobRecovery(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "delete-job")
	f := newDeletionCoordinatorFixture()
	f.failReply = true
	configureDeletionFixture(t, env.store, f)
	op, err := env.store.RequestDeveloperCloudDeletion(ctx, owner.User.ID, owner.BrandCloud.ID, "job")
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := env.store.ClaimCloudDeletionJobs(ctx, 1, time.Minute)
	if err != nil || len(jobs) != 1 || jobs[0].OperationID != op.ID {
		t.Fatalf("claim: %+v %v", jobs, err)
	}
	if others, err := env.store.ClaimCloudDeletionJobs(ctx, 1, time.Minute); err != nil || len(others) != 0 {
		t.Fatalf("double claim: %+v %v", others, err)
	}
	if _, err = env.store.ProcessCloudDeletionJob(ctx, jobs[0]); err == nil {
		t.Fatal("expected lost reply")
	}
	if _, err = env.db.Exec(ctx, `UPDATE cloud_deletion_jobs SET lease_until=clock_timestamp()-interval '1 second' WHERE operation_id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	newJobs, err := env.store.ClaimCloudDeletionJobs(ctx, 1, time.Minute)
	if err != nil || len(newJobs) != 1 || newJobs[0].LeaseID == jobs[0].LeaseID {
		t.Fatalf("reclaim: %+v %v", newJobs, err)
	}
	if _, err = env.store.ProcessCloudDeletionJob(ctx, jobs[0]); !errors.Is(err, ErrHandoffLeaseLost) {
		t.Fatalf("stale worker: %v", err)
	}
	if ok, err := env.store.FinishCloudDeletionJob(ctx, jobs[0], 0); err != nil || ok {
		t.Fatalf("stale lease released: %v %v", ok, err)
	}
	// A disablement after Billing has committed cannot force rollback/reopen.
	if _, err = env.db.Exec(ctx, `UPDATE users SET disabled_at=now() WHERE id=$1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	op, err = env.store.ProcessCloudDeletionJob(ctx, newJobs[0])
	if err != nil || op.State != "succeeded" {
		t.Fatalf("job recovery: %+v %v", op, err)
	}
	if ok, err := env.store.FinishCloudDeletionJob(ctx, newJobs[0], time.Second); err != nil || !ok {
		t.Fatalf("finish: %v %v", ok, err)
	}
	if next, err := env.store.ClaimCloudDeletionJobs(ctx, 1, time.Minute); err != nil || len(next) != 0 {
		t.Fatalf("terminal reclaimed: %+v %v", next, err)
	}
}
