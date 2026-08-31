package clouddeletion

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"rtk_account_manager/internal/store"
)

var _ repository = (*store.Store)(nil)

type fakeRepository struct {
	jobs    []store.CloudDeletionJob
	claim   func(context.Context) ([]store.CloudDeletionJob, error)
	process func(context.Context, store.CloudDeletionJob) (store.CloudDeletionOperation, error)
	finish  func(context.Context, store.CloudDeletionJob, time.Duration) (bool, error)
	mu      sync.Mutex
	delays  map[string]time.Duration
}

func (r *fakeRepository) ClaimCloudDeletionJobs(ctx context.Context, _ int, _ time.Duration) ([]store.CloudDeletionJob, error) {
	if r.claim != nil {
		return r.claim(ctx)
	}
	return r.jobs, nil
}
func (r *fakeRepository) ProcessCloudDeletionJob(ctx context.Context, j store.CloudDeletionJob) (store.CloudDeletionOperation, error) {
	return r.process(ctx, j)
}
func (r *fakeRepository) FinishCloudDeletionJob(ctx context.Context, j store.CloudDeletionJob, d time.Duration) (bool, error) {
	if r.finish != nil {
		return r.finish(ctx, j, d)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.delays == nil {
		r.delays = map[string]time.Duration{}
	}
	r.delays[j.OperationID] = d
	return true, nil
}

func TestCloudDeletionWorkerBoundedRetryAndSanitizedLogs(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	r := &fakeRepository{jobs: []store.CloudDeletionJob{{OperationID: "done", Attempts: 1}, {OperationID: "canceled", Attempts: 2}, {OperationID: "retry", Attempts: 1000000}, {OperationID: "lost"}}}
	r.process = func(_ context.Context, j store.CloudDeletionJob) (store.CloudDeletionOperation, error) {
		switch j.OperationID {
		case "done":
			return store.CloudDeletionOperation{State: "succeeded"}, nil
		case "canceled":
			return store.CloudDeletionOperation{State: "canceled"}, nil
		case "lost":
			return store.CloudDeletionOperation{}, store.ErrHandoffLeaseLost
		default:
			return store.CloudDeletionOperation{}, errors.New("provider-secret")
		}
	}
	s, err := NewService(r, Options{Logger: zap.New(core)})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := s.RunOnce(context.Background())
	if err != nil || stats.Claimed != 4 || stats.Completed != 2 || stats.LeaseLost != 1 || stats.Retrying != 1 {
		t.Fatalf("stats %+v %v", stats, err)
	}
	if r.delays["done"] != 0 || r.delays["canceled"] != 0 || r.delays["retry"] != 5*time.Minute {
		t.Fatal(r.delays)
	}
	if _, ok := r.delays["lost"]; ok {
		t.Fatal("stale lease finished")
	}
	if logs.FilterMessageSnippet("provider-secret").Len() != 0 || logs.Len() != 3 {
		t.Fatal("unexpected logging", logs.All())
	}
	for _, entry := range logs.All() {
		if len(entry.Context) != 3 {
			t.Fatal("unexpected raw diagnostics", entry)
		}
	}
}

func TestCloudDeletionWorkerStartsBatchBeforeLeasesExpire(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	r := &fakeRepository{jobs: []store.CloudDeletionJob{{OperationID: "one"}, {OperationID: "two"}}}
	r.process = func(ctx context.Context, _ store.CloudDeletionJob) (store.CloudDeletionOperation, error) {
		started <- struct{}{}
		select {
		case <-release:
			return store.CloudDeletionOperation{State: "succeeded"}, nil
		case <-ctx.Done():
			return store.CloudDeletionOperation{}, ctx.Err()
		}
	}
	s, _ := NewService(r, Options{StepTimeout: time.Second})
	done := make(chan error, 1)
	go func() {
		stats, err := s.RunOnce(context.Background())
		if err == nil && stats.Completed != 2 {
			err = errors.New("batch incomplete")
		}
		done <- err
	}()
	for range 2 {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("claimed batch was serialized")
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCloudDeletionWorkerTimeoutAndShutdown(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline", true: "shutdown"}[shutdown], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := &fakeRepository{jobs: []store.CloudDeletionJob{{OperationID: "slow", Attempts: 1}}}
			r.process = func(step context.Context, _ store.CloudDeletionJob) (store.CloudDeletionOperation, error) {
				if shutdown {
					cancel()
				}
				<-step.Done()
				return store.CloudDeletionOperation{}, step.Err()
			}
			s, _ := NewService(r, Options{StepTimeout: time.Millisecond})
			stats, err := s.RunOnce(ctx)
			if shutdown {
				if !errors.Is(err, context.Canceled) || len(r.delays) != 0 {
					t.Fatalf("shutdown %+v %v %v", stats, err, r.delays)
				}
			} else if err != nil || stats.Retrying != 1 || r.delays["slow"] != 5*time.Second {
				t.Fatalf("deadline %+v %v %v", stats, err, r.delays)
			}
		})
	}
}

func TestCloudDeletionWorkerFinishFailureAndLeaseLoss(t *testing.T) {
	for _, fail := range []bool{false, true} {
		r := &fakeRepository{jobs: []store.CloudDeletionJob{{OperationID: "job"}}, process: func(context.Context, store.CloudDeletionJob) (store.CloudDeletionOperation, error) {
			return store.CloudDeletionOperation{State: "succeeded"}, nil
		}}
		r.finish = func(context.Context, store.CloudDeletionJob, time.Duration) (bool, error) {
			if fail {
				return false, errors.New("db failure")
			}
			return false, nil
		}
		s, _ := NewService(r, Options{})
		stats, err := s.RunOnce(context.Background())
		if stats.Completed != 0 || (fail && err == nil) || (!fail && (err != nil || stats.LeaseLost != 1)) {
			t.Fatalf("finish %+v %v", stats, err)
		}
	}
}

func TestCloudDeletionWorkerRunRecoversClaimErrorAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	core, logs := observer.New(zap.ErrorLevel)
	calls := 0
	r := &fakeRepository{claim: func(context.Context) ([]store.CloudDeletionJob, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("postgres://secret")
		}
		cancel()
		return nil, nil
	}}
	s, _ := NewService(r, Options{PollInterval: time.Millisecond, Logger: zap.New(core)})
	if err := s.Run(ctx); err != nil || calls != 2 || logs.Len() != 1 || len(logs.All()[0].Context) != 0 {
		t.Fatalf("run %d %v %+v", calls, err, logs.All())
	}
	if _, err := s.RunOnce(ctx); !errors.Is(err, context.Canceled) || calls != 2 {
		t.Fatal("claim after shutdown", err)
	}
}

func TestCloudDeletionWorkerRejectsInvalidOptions(t *testing.T) {
	for _, opts := range []Options{{BatchSize: -1}, {BatchSize: 129}, {LeaseDuration: 10 * time.Second}, {StepTimeout: 2 * time.Minute}, {StepTimeout: time.Duration(1<<63 - 1)}, {PollInterval: -time.Second}, {RetryBase: time.Hour, RetryMax: time.Second}} {
		if _, err := NewService(&fakeRepository{}, opts); err == nil {
			t.Fatal("invalid options", opts)
		}
	}
	if _, err := NewService(nil, Options{}); err == nil {
		t.Fatal("nil repository")
	}
}
