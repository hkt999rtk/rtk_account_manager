package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestFactoryCancellationFencesAbsentAndDelayedAdmissions(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	_, in := factoryAdmissionFixture(t, env, 1)
	first, err := env.store.BeginFactoryEnrollmentCancellation(ctx, in)
	if err != nil || first.Admitted || first.Status != "cancel_requested" || first.CompletedAt != nil || first.EvidenceSHA256 != nil {
		t.Fatalf("intent: %+v %v", first, err)
	}
	for range 3 {
		out, err := New(env.db).BeginFactoryEnrollmentCancellation(ctx, in)
		if err != nil || out.ID != first.ID {
			t.Fatal("intent replay changed binding")
		}
	}
	if _, err := env.store.ReserveFactoryEnrollment(ctx, in); !errors.Is(err, ErrConflict) {
		t.Fatalf("delayed admission not fenced: %v", err)
	}
	extra := in
	extra.RequestID = "extra"
	if _, err := env.store.ReserveFactoryEnrollment(ctx, extra); err != nil {
		t.Fatal("never-admitted cancellation invented a quota reservation", err)
	}
	if _, err := env.store.CompleteFactoryEnrollment(ctx, factoryResult(in, first, "issued")); !errors.Is(err, ErrConflict) {
		t.Fatal("never-admitted cancellation became an issued result")
	}
	for _, change := range []func(*FactoryEnrollmentAdmission){func(b *FactoryEnrollmentAdmission) { b.DeviceID = "different" }, func(b *FactoryEnrollmentAdmission) { b.RequestSHA256 = strings.Repeat("b", 64) }} {
		bad := in
		change(&bad)
		if _, err := env.store.BeginFactoryEnrollmentCancellation(ctx, bad); !errors.Is(err, ErrConflict) {
			t.Fatal("changed cancellation binding accepted")
		}
	}
	result := factoryResult(in, first, "not_issued")
	if _, err := env.store.CompleteFactoryEnrollment(ctx, result); err != nil {
		t.Fatal(err)
	}
	if replay, err := env.store.BeginFactoryEnrollmentCancellation(ctx, in); err != nil || replay.Status != "not_issued" || replay.ID != first.ID {
		t.Fatal("terminal intent reopened")
	}
	if _, err := env.store.ReserveFactoryEnrollment(ctx, extra); err != nil {
		t.Fatal("definitive result did not release capacity", err)
	}
	var audits, issued int
	if err := env.db.QueryRow(ctx, `SELECT issued_quantity,(SELECT count(*) FROM audit_events WHERE subject_id=$2) FROM factory_production_runs WHERE id=$1`, in.RunID, first.ID).Scan(&issued, &audits); err != nil || issued != 0 || audits != 2 {
		t.Fatalf("unexpected counter/audit: %d %d %v", issued, audits, err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE factory_enrollment_reservations SET status='reserved',completed_at=NULL,evidence_sha256=NULL WHERE id=$1`, first.ID); err == nil {
		t.Fatal("terminal cancellation reopened")
	}
}

func TestFactoryCancellationRacesAdmissionAndPreservesIssuedOutcomes(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner, in := factoryAdmissionFixture(t, env, 1)
	if _, err := env.store.ReserveFactoryEnrollment(ctx, in); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	ids := make(chan string, 12)
	for i := range 12 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			var out FactoryEnrollmentReservation
			var err error
			if i%2 == 0 {
				out, err = env.store.BeginFactoryEnrollmentCancellation(ctx, in)
			} else {
				out, err = env.store.ReserveFactoryEnrollment(ctx, in)
			}
			if errors.Is(err, ErrConflict) && i%2 != 0 {
				return
			}
			if err != nil {
				t.Error(err)
				return
			}
			ids <- out.ID
		}(i)
	}
	close(start)
	wg.Wait()
	close(ids)
	r, err := env.store.LookupFactoryEnrollment(ctx, in)
	if err != nil || !r.Admitted || r.Status != "cancel_requested" {
		t.Fatalf("intent lost race: %+v %v", r, err)
	}
	for id := range ids {
		if id != r.ID {
			t.Fatal("race allocated multiple reservation UUIDs")
		}
	}
	extra := in
	extra.RequestID = "still-held"
	if _, err := env.store.ReserveFactoryEnrollment(ctx, extra); !errors.Is(err, ErrProductionRunCapacity) {
		t.Fatal("admitted cancellation released quota before issuer evidence")
	}
	if _, err := env.db.Exec(ctx, `UPDATE users SET disabled_at=now() WHERE id=$1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.BeginFactoryEnrollmentCancellation(ctx, in); err != nil {
		t.Fatal("revoked owner blocked trusted recovery", err)
	}
	// Signing might have claimed before cancellation. Its real issued outcome
	// still must be recorded, never guessed as non-issuance or signed again.
	result := factoryResult(in, r, "issued")
	for range 2 {
		if _, err := env.store.CompleteFactoryEnrollment(ctx, result); err != nil {
			t.Fatal(err)
		}
	}
	if replay, err := env.store.BeginFactoryEnrollmentCancellation(ctx, in); err != nil || replay.Status != "issued" {
		t.Fatal("issued outcome was canceled")
	}
	var issued int
	if err := env.db.QueryRow(ctx, `SELECT issued_quantity FROM factory_production_runs WHERE id=$1`, in.RunID).Scan(&issued); err != nil || issued != 1 {
		t.Fatalf("issued count=%d %v", issued, err)
	}
}

func TestFactoryCancellationValidationAndPersistenceFailures(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	_, in := factoryAdmissionFixture(t, env, 1)
	for _, change := range []func(*FactoryEnrollmentAdmission){func(b *FactoryEnrollmentAdmission) { b.RequestID = " " }, func(b *FactoryEnrollmentAdmission) { b.DeviceID = strings.Repeat("x", 257) }, func(b *FactoryEnrollmentAdmission) { b.RequestSHA256 = "bad" }, func(b *FactoryEnrollmentAdmission) { b.CloudID = "missing" }, func(b *FactoryEnrollmentAdmission) { b.RunID = "missing" }, func(b *FactoryEnrollmentAdmission) { b.ProductID = "missing" }} {
		bad := in
		change(&bad)
		if _, err := env.store.BeginFactoryEnrollmentCancellation(ctx, bad); err == nil {
			t.Fatal("invalid scope accepted")
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := env.store.BeginFactoryEnrollmentCancellation(canceled, in); err == nil {
		t.Fatal("canceled context accepted")
	}
	if _, err := env.db.Exec(ctx, `ALTER TABLE audit_events ADD CONSTRAINT isolated_cancel_audit_failure CHECK(false) NOT VALID`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		env.db.Exec(context.Background(), `ALTER TABLE audit_events DROP CONSTRAINT IF EXISTS isolated_cancel_audit_failure`)
	})
	if _, err := env.store.BeginFactoryEnrollmentCancellation(ctx, in); err == nil {
		t.Fatal("audit failure ignored")
	}
	if _, err := env.store.LookupFactoryEnrollment(ctx, in); !errors.Is(err, ErrNotFound) {
		t.Fatal("failed cancellation partially committed", err)
	}
}
