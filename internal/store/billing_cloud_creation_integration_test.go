package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestBillingCloudCreationOutboxCommitsWithUniqueInitialOwner(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	result, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{Email: "billing-event@example.com", PasswordHash: "hash", OrganizationName: "Billing Event Cloud", SignupPendingVerification: true})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := env.store.ClaimBillingCloudCreations(ctx)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("claim %+v %v", jobs, err)
	}
	j := jobs[0]
	if j.CloudID != result.BrandCloud.ID || j.OwnerUserID != result.User.ID || !j.Valid() {
		t.Fatal("incorrect initial owner event")
	}
	// SQL CHECK treats NULL as success unless the receipt is explicitly required.
	if _, err := env.db.Exec(ctx, `UPDATE brand_cloud_billing_creation_outbox SET delivered_at=clock_timestamp(),billing_account_id=cloud_id WHERE event_id=$1`, j.EventID); err == nil {
		t.Fatal("delivery without a receipt accepted")
	}
	if jobs, err := env.store.ClaimBillingCloudCreations(ctx); err != nil || len(jobs) != 0 {
		t.Fatal("leased event claimed twice", err)
	}
	bad := BillingCloudCreationReceipt{BillingCloudCreation: j.BillingCloudCreation, AccountID: j.CloudID}
	bad.OwnerUserID = j.CloudID
	if _, err := env.store.FinishBillingCloudCreation(ctx, j, &bad); !errors.Is(err, ErrConflict) {
		t.Fatal("mismatched receipt", err)
	}
	if ok, err := env.store.FinishBillingCloudCreation(ctx, j, nil); err != nil || !ok {
		t.Fatal("retry not retained", err)
	}
	if jobs, err := env.store.ClaimBillingCloudCreations(ctx); err != nil || len(jobs) != 0 {
		t.Fatal("retry ignored backoff", err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE brand_cloud_billing_creation_outbox SET available_at=clock_timestamp()-interval '1 second' WHERE event_id=$1`, j.EventID); err != nil {
		t.Fatal(err)
	}
	jobs, err = env.store.ClaimBillingCloudCreations(ctx)
	if err != nil || len(jobs) != 1 || jobs[0].LeaseID == j.LeaseID || jobs[0].EvidenceSHA256 != j.EvidenceSHA256 {
		t.Fatal("immutable retry lost", err)
	}
	r := BillingCloudCreationReceipt{BillingCloudCreation: j.BillingCloudCreation, AccountID: j.CloudID}
	if ok, err := env.store.FinishBillingCloudCreation(ctx, j, &r); err != nil || ok {
		t.Fatal("old lease completed new attempt", err)
	}
	if ok, err := env.store.FinishBillingCloudCreation(ctx, jobs[0], &r); err != nil || !ok {
		t.Fatal("complete", err)
	}
	if jobs, err := env.store.ClaimBillingCloudCreations(ctx); err != nil || len(jobs) != 0 {
		t.Fatal("delivered replayed", err)
	}
	for _, q := range []string{`UPDATE brand_cloud_billing_creation_outbox SET delivered_at=NULL WHERE event_id=$1`, `DELETE FROM brand_cloud_billing_creation_outbox WHERE event_id=$1`, `UPDATE brand_cloud_billing_creation_outbox SET owner_user_id=cloud_id WHERE event_id=$1`} {
		if _, err := env.db.Exec(ctx, q, j.EventID); err == nil {
			t.Fatal("creation evidence rewritten")
		}
	}
	// Failed creation cannot leave an event after the cloud transaction rolls back.
	_, err = env.store.SignupDeveloper(ctx, DeveloperSignupInput{Email: "billing-event@example.com", PasswordHash: "hash", OrganizationName: "Must rollback"})
	if err == nil {
		t.Fatal("duplicate signup accepted")
	}
	var count int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM brand_cloud_billing_creation_outbox`).Scan(&count); err != nil || count != 1 {
		t.Fatal("failed signup leaked event", err)
	}
}

func TestBillingCloudCreationFailureRollsBackSignupAndExcludesLegacyOrganizations(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	// Fail at the deferred event insertion, after signup has written its user,
	// organization and membership. No independently committed signup can escape.
	if _, err := env.db.Exec(ctx, `CREATE FUNCTION test_reject_cloud_creation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected creation failure'; END $$;
		CREATE TRIGGER test_reject_cloud_creation BEFORE INSERT ON brand_cloud_billing_creation_outbox FOR EACH ROW EXECUTE FUNCTION test_reject_cloud_creation()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := env.db.Exec(ctx, `DROP TRIGGER IF EXISTS test_reject_cloud_creation ON brand_cloud_billing_creation_outbox; DROP FUNCTION IF EXISTS test_reject_cloud_creation()`); err != nil {
			t.Error(err)
		}
	})
	in := DeveloperSignupInput{Email: "rollback-cloud@example.com", PasswordHash: "hash", OrganizationName: "Rollback", SignupPendingVerification: true}
	if _, err := env.store.SignupDeveloper(ctx, in); err == nil {
		t.Fatal("signup succeeded without its durable Billing event")
	}
	var users, clouds, members, events int
	if err := env.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM users), (SELECT count(*) FROM organizations), (SELECT count(*) FROM organization_members), (SELECT count(*) FROM brand_cloud_billing_creation_outbox)`).Scan(&users, &clouds, &members, &events); err != nil || users != 0 || clouds != 0 || members != 0 || events != 0 {
		t.Fatalf("deferred failure leaked signup: %d/%d/%d/%d: %v", users, clouds, members, events, err)
	}
	if _, err := env.db.Exec(ctx, `DROP TRIGGER test_reject_cloud_creation ON brand_cloud_billing_creation_outbox`); err != nil {
		t.Fatal(err)
	}
	result, err := env.store.SignupDeveloper(ctx, in)
	if err != nil {
		t.Fatal("retry signup", err)
	}
	legacy, err := env.store.CreateOrganization(ctx, result.User.ID, "Legacy customer")
	if err != nil {
		t.Fatal(err)
	}
	var kind string
	if err := env.db.QueryRow(ctx, `SELECT organization_kind FROM organizations WHERE id=$1`, legacy.ID).Scan(&kind); err != nil || kind != "customer_org" {
		t.Fatal("fixture is not a legacy customer organization", kind, err)
	}
	jobs, err := env.store.ClaimBillingCloudCreations(ctx)
	if err != nil || len(jobs) != 1 || jobs[0].CloudID != result.BrandCloud.ID {
		t.Fatal("legacy organization acquired inferred Billing ownership", jobs, err)
	}
}

func TestBillingCloudCreationOutboxPartitionsLeasesAndPreservesEventOnTimeout(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	for _, email := range []string{"cloud-a@example.com", "cloud-b@example.com", "cloud-c@example.com", "cloud-d@example.com", "cloud-e@example.com"} {
		if _, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{Email: email, PasswordHash: "hash", OrganizationName: email}); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	out := make(chan BillingCloudCreationJob, 8)
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			jobs, err := env.store.ClaimBillingCloudCreations(ctx)
			if err != nil {
				errs <- err
				return
			}
			for _, j := range jobs {
				out <- j
			}
		}()
	}
	wg.Wait()
	close(out)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var last BillingCloudCreationJob
	for j := range out {
		if seen[j.EventID] {
			t.Fatal("duplicate lease")
		}
		seen[j.EventID] = true
		last = j
	}
	if len(seen) != 5 {
		t.Fatal("missing events", len(seen))
	}
	if _, err := env.db.Exec(ctx, `UPDATE brand_cloud_billing_creation_outbox SET lease_until=clock_timestamp()-interval '1 second' WHERE event_id=$1`, last.EventID); err != nil {
		t.Fatal(err)
	}
	r := BillingCloudCreationReceipt{BillingCloudCreation: last.BillingCloudCreation, AccountID: last.CloudID}
	if ok, err := env.store.FinishBillingCloudCreation(ctx, last, &r); err != nil || ok {
		t.Fatal("expired lease accepted", err)
	}
	jobs, err := env.store.ClaimBillingCloudCreations(ctx)
	if err != nil || len(jobs) != 1 || jobs[0].EventID != last.EventID {
		t.Fatal("expired event not recovered", err)
	}
	canceled, cancel := context.WithTimeout(ctx, time.Nanosecond)
	defer cancel()
	<-canceled.Done()
	if _, err := env.store.ClaimBillingCloudCreations(canceled); err == nil {
		t.Fatal("unavailable claim became empty success")
	}
}
