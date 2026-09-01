package handoff

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"
	"rtk_account_manager/internal/store"
)

type repository interface {
	ClaimHandoffJobs(context.Context, int, time.Duration) ([]store.HandoffJob, error)
	AdvanceHandoffJob(context.Context, store.HandoffJob) (store.HandoffStep, error)
	FinishHandoffJob(context.Context, store.HandoffJob, string, bool, time.Duration) (bool, error)
}

type Options struct {
	PollInterval, LeaseDuration, StepTimeout, RetryBase, RetryMax time.Duration
	BatchSize                                                     int
	Logger                                                        *zap.Logger
}
type Service struct {
	store   repository
	options Options
}
type Stats struct{ Claimed, Progress, Waiting, Retrying, LeaseLost int }

func NewService(repo repository, opts Options) (*Service, error) {
	if repo == nil {
		return nil, fmt.Errorf("handoff repository is required")
	}
	if opts.PollInterval == 0 {
		opts.PollInterval = 5 * time.Second
	}
	if opts.LeaseDuration == 0 {
		opts.LeaseDuration = 2 * time.Minute
	}
	if opts.StepTimeout == 0 {
		opts.StepTimeout = 45 * time.Second
	}
	if opts.RetryBase == 0 {
		opts.RetryBase = 5 * time.Second
	}
	if opts.RetryMax == 0 {
		opts.RetryMax = 5 * time.Minute
	}
	if opts.BatchSize == 0 {
		opts.BatchSize = 10
	}
	if opts.Logger == nil {
		opts.Logger = zap.NewNop()
	}
	if opts.PollInterval <= 0 || opts.PollInterval > time.Minute || opts.LeaseDuration < 30*time.Second || opts.LeaseDuration > 5*time.Minute || opts.StepTimeout <= 0 || opts.StepTimeout+5*time.Second >= opts.LeaseDuration || opts.RetryBase <= 0 || opts.RetryMax < opts.RetryBase || opts.RetryMax > time.Hour || opts.BatchSize < 1 || opts.BatchSize > 128 {
		return nil, fmt.Errorf("invalid handoff worker timing or batch size")
	}
	return &Service{store: repo, options: opts}, nil
}
func (s *Service) Run(ctx context.Context) error {
	for {
		if _, err := s.RunOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// Database failures do not kill recovery or imply permission to clear a
			// hold. Persisted leases expire and exact protocol work is retried.
			s.options.Logger.Error("handoff worker iteration unavailable")
		}
		timer := time.NewTimer(s.options.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}
func (s *Service) retryDelay(attempt int) time.Duration {
	delay := s.options.RetryBase
	for i := 0; i < attempt && delay < s.options.RetryMax; i++ {
		if delay > s.options.RetryMax/2 {
			return s.options.RetryMax
		}
		delay *= 2
	}
	return min(delay, s.options.RetryMax)
}
func (s *Service) RunOnce(ctx context.Context) (Stats, error) {
	jobs, err := s.store.ClaimHandoffJobs(ctx, s.options.BatchSize, s.options.LeaseDuration)
	if err != nil {
		return Stats{}, err
	}
	stats := Stats{Claimed: len(jobs)}
	type result struct {
		step store.HandoffStep
		lost bool
		err  error
	}
	results := make(chan result, len(jobs))
	for _, job := range jobs {
		go func(job store.HandoffJob) {
			stepCtx, cancel := context.WithTimeout(ctx, s.options.StepTimeout)
			step, err := s.store.AdvanceHandoffJob(stepCtx, job)
			cancel()
			if errors.Is(err, store.ErrHandoffLeaseLost) {
				results <- result{lost: true}
				return
			}
			if ctx.Err() != nil {
				results <- result{err: ctx.Err()}
				return
			}
			if err != nil {
				step = store.HandoffStep{Outcome: "evidence_unavailable", Retry: true}
			}
			delay := s.options.PollInterval
			if step.Outcome == "progress" {
				delay = 0
			} else if step.Retry {
				delay = s.retryDelay(job.RetryCount)
			}
			applied, finishErr := s.store.FinishHandoffJob(ctx, job, step.Outcome, step.Retry, delay)
			if finishErr == nil && applied {
				s.options.Logger.Info("handoff step", zap.String("operation_id", job.OperationID), zap.String("cloud_id", job.CloudID), zap.String("outcome", step.Outcome), zap.Bool("retry", step.Retry))
			}
			results <- result{step: step, lost: !applied, err: finishErr}
		}(job)
	}
	var firstErr error
	for range jobs {
		r := <-results
		if r.err != nil && firstErr == nil {
			firstErr = r.err
		}
		if r.lost {
			stats.LeaseLost++
		} else if r.step.Retry {
			stats.Retrying++
		} else if r.step.Outcome == "progress" || r.step.Outcome == "complete" {
			stats.Progress++
		} else {
			stats.Waiting++
		}
	}
	return stats, firstErr
}
