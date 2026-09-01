package handoff

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"rtk_account_manager/internal/store"
)

var _ repository = (*store.Store)(nil)

type fakeRepository struct {
	mu      sync.Mutex
	jobs    []store.HandoffJob
	results map[string]store.HandoffStep
	delays  map[string]time.Duration
	advance func(context.Context, store.HandoffJob) (store.HandoffStep, error)
}

func (r *fakeRepository) ClaimHandoffJobs(context.Context, int, time.Duration) ([]store.HandoffJob, error) {
	return r.jobs, nil
}
func (r *fakeRepository) AdvanceHandoffJob(ctx context.Context, j store.HandoffJob) (store.HandoffStep, error) {
	return r.advance(ctx, j)
}
func (r *fakeRepository) FinishHandoffJob(_ context.Context, j store.HandoffJob, outcome string, retry bool, delay time.Duration) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.results == nil {
		r.results = map[string]store.HandoffStep{}
		r.delays = map[string]time.Duration{}
	}
	r.results[j.OperationID] = store.HandoffStep{Outcome: outcome, Retry: retry}
	r.delays[j.OperationID] = delay
	return true, nil
}

func TestHandoffWorkerRetriesWithoutDeadLetterOrLeakingErrors(t *testing.T) {
	r := &fakeRepository{jobs: []store.HandoffJob{{OperationID: "progress"}, {OperationID: "lost"}, {OperationID: "failure", RetryCount: 1000000}, {OperationID: "waiting"}}}
	r.advance = func(_ context.Context, j store.HandoffJob) (store.HandoffStep, error) {
		switch j.OperationID {
		case "progress":
			return store.HandoffStep{Outcome: "progress"}, nil
		case "lost":
			return store.HandoffStep{}, store.ErrHandoffLeaseLost
		case "failure":
			return store.HandoffStep{}, errors.New("secret remote diagnostic")
		default:
			return store.HandoffStep{Outcome: "waiting_confirmation"}, nil
		}
	}
	s, err := NewService(r, Options{})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := s.RunOnce(context.Background())
	if err != nil || stats.Progress != 1 || stats.LeaseLost != 1 || stats.Waiting != 1 || stats.Retrying != 1 {
		t.Fatalf("stats %+v %v", stats, err)
	}
	if r.results["failure"].Outcome != "evidence_unavailable" || !r.results["failure"].Retry || r.delays["failure"] != 5*time.Minute {
		t.Fatal("failure leaked or stopped retry", r.results, r.delays)
	}
	if _, ok := r.results["lost"]; ok {
		t.Fatal("stale lease finished")
	}
	if r.delays["progress"] != 0 || r.delays["waiting"] != 5*time.Second {
		t.Fatal("incorrect progress/wait timing", r.delays)
	}
}

func TestHandoffWorkerStepDeadlinePersistsRetry(t *testing.T) {
	r := &fakeRepository{jobs: []store.HandoffJob{{OperationID: "slow"}}, advance: func(ctx context.Context, _ store.HandoffJob) (store.HandoffStep, error) {
		<-ctx.Done()
		return store.HandoffStep{}, ctx.Err()
	}}
	s, err := NewService(r, Options{StepTimeout: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := s.RunOnce(context.Background())
	if err != nil || stats.Retrying != 1 || r.results["slow"].Outcome != "evidence_unavailable" {
		t.Fatalf("timeout %+v %v", stats, err)
	}
}

func TestHandoffWorkerCancellationLeavesLeaseForRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &fakeRepository{jobs: []store.HandoffJob{{OperationID: "stopped"}}, advance: func(ctx context.Context, _ store.HandoffJob) (store.HandoffStep, error) {
		return store.HandoffStep{}, ctx.Err()
	}}
	s, err := NewService(r, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunOnce(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown ignored %v", err)
	}
	if len(r.results) != 0 {
		t.Fatal("shutdown pretended delivery completed")
	}
}

func TestHandoffWorkerOptionsBoundResourceUse(t *testing.T) {
	for _, opts := range []Options{{BatchSize: -1}, {BatchSize: 129}, {LeaseDuration: 10 * time.Second}, {StepTimeout: 2 * time.Minute}, {PollInterval: -time.Second}, {RetryBase: time.Hour, RetryMax: time.Second}} {
		if _, err := NewService(&fakeRepository{}, opts); err == nil {
			t.Fatal("unsafe worker config", opts)
		}
	}
	if _, err := NewService(nil, Options{}); err == nil {
		t.Fatal("nil repository accepted")
	}
}
