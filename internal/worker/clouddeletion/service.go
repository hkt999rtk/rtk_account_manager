package clouddeletion

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"
	"rtk_account_manager/internal/store"
)

type repository interface {
	ClaimCloudDeletionJobs(context.Context, int, time.Duration) ([]store.CloudDeletionJob, error)
	ProcessCloudDeletionJob(context.Context, store.CloudDeletionJob) (store.CloudDeletionOperation, error)
	FinishCloudDeletionJob(context.Context, store.CloudDeletionJob, time.Duration) (bool, error)
}

type Options struct {
	PollInterval, LeaseDuration, StepTimeout, RetryBase, RetryMax time.Duration
	BatchSize                                                     int
	Logger                                                        *zap.Logger
}
type Service struct {
	repo repository
	opts Options
}
type Stats struct{ Claimed, Completed, Retrying, LeaseLost int }

func NewService(repo repository, opts Options) (*Service, error) {
	if repo == nil {
		return nil, fmt.Errorf("cloud deletion repository is required")
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
	if opts.PollInterval <= 0 || opts.PollInterval > time.Minute || opts.LeaseDuration < 30*time.Second || opts.LeaseDuration > 5*time.Minute || opts.StepTimeout <= 0 || opts.StepTimeout >= opts.LeaseDuration-5*time.Second || opts.RetryBase <= 0 || opts.RetryMax < opts.RetryBase || opts.RetryMax > time.Hour || opts.BatchSize < 1 || opts.BatchSize > 128 {
		return nil, fmt.Errorf("invalid cloud deletion worker timing or batch size")
	}
	return &Service{repo: repo, opts: opts}, nil
}

func (s *Service) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		if _, err := s.RunOnce(ctx); err != nil && ctx.Err() == nil {
			// Do not log provider errors, credentials, or database connection strings.
			s.opts.Logger.Error("cloud deletion worker iteration unavailable")
		}
		timer := time.NewTimer(s.opts.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func (s *Service) retryDelay(attempt int64) time.Duration {
	delay := s.opts.RetryBase
	for i := int64(1); i < attempt && delay < s.opts.RetryMax; i++ {
		if delay > s.opts.RetryMax/2 {
			return s.opts.RetryMax
		}
		delay *= 2
	}
	return min(delay, s.opts.RetryMax)
}

func (s *Service) RunOnce(ctx context.Context) (Stats, error) {
	if err := ctx.Err(); err != nil {
		return Stats{}, err
	}
	claimCtx, claimCancel := context.WithTimeout(ctx, s.opts.StepTimeout)
	jobs, err := s.repo.ClaimCloudDeletionJobs(claimCtx, s.opts.BatchSize, s.opts.LeaseDuration)
	claimCancel()
	if err != nil {
		return Stats{}, err
	}
	stats := Stats{Claimed: len(jobs)}
	type result struct {
		completed, lost bool
		err             error
	}
	results := make(chan result, len(jobs))
	// Start every claimed job immediately, bounded by BatchSize: a sequential
	// batch would consume later jobs' leases while earlier HTTP calls wait.
	for _, job := range jobs {
		go func(job store.CloudDeletionJob) {
			stepCtx, cancel := context.WithTimeout(ctx, s.opts.StepTimeout)
			op, err := s.repo.ProcessCloudDeletionJob(stepCtx, job)
			cancel()
			if errors.Is(err, store.ErrHandoffLeaseLost) {
				results <- result{lost: true}
				return
			}
			if ctx.Err() != nil {
				results <- result{err: ctx.Err()}
				return
			}
			completed := err == nil && (op.State == "succeeded" || op.State == "canceled")
			delay := s.retryDelay(job.Attempts)
			outcome := "retrying"
			if completed {
				delay = 0
				outcome = "complete"
			}
			// Use the parent context so a step deadline can persist its retry.
			finishCtx, finishCancel := context.WithTimeout(ctx, 5*time.Second)
			applied, finishErr := s.repo.FinishCloudDeletionJob(finishCtx, job, delay)
			finishCancel()
			if finishErr == nil && applied {
				s.opts.Logger.Info("cloud deletion recovery step", zap.String("operation_id", job.OperationID), zap.String("cloud_id", job.CloudID), zap.String("outcome", outcome))
			}
			results <- result{completed: completed, lost: finishErr == nil && !applied, err: finishErr}
		}(job)
	}
	var firstErr error
	for range jobs {
		r := <-results
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			continue
		}
		if r.lost {
			stats.LeaseLost++
		} else if r.completed {
			stats.Completed++
		} else {
			stats.Retrying++
		}
	}
	return stats, firstErr
}
