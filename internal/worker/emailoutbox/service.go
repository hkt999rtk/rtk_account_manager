package emailoutbox

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"strings"
	"time"

	"go.uber.org/zap"

	"rtk_account_manager/internal/emaildelivery"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

type messageStore interface {
	ClaimEmailOutboxReady(context.Context, time.Time, time.Time, int) ([]model.EmailOutbox, error)
	TransitionEmailOutbox(context.Context, store.EmailOutboxTransitionInput) (bool, error)
}

type sender interface {
	Send(context.Context, emaildelivery.Message) error
}

type Options struct {
	MaxAttempts   int
	PollInterval  time.Duration
	RetryBase     time.Duration
	RetryMax      time.Duration
	LeaseDuration time.Duration
	BatchSize     int
	Now           func() time.Time
	Jitter        func(time.Duration) time.Duration
	Logger        *zap.Logger
}

type Service struct {
	store         messageStore
	cipher        *emaildelivery.Cipher
	renderer      emaildelivery.Renderer
	sender        sender
	maxAttempts   int
	pollInterval  time.Duration
	retryBase     time.Duration
	retryMax      time.Duration
	leaseDuration time.Duration
	batchSize     int
	now           func() time.Time
	jitter        func(time.Duration) time.Duration
	logger        *zap.Logger
}

type Stats struct {
	Claimed      int
	Sent         int
	Retrying     int
	DeadLettered int
	Expired      int
}

func NewService(store messageStore, cipher *emaildelivery.Cipher, renderer emaildelivery.Renderer, sender sender, options Options) *Service {
	if options.MaxAttempts <= 0 {
		options.MaxAttempts = 8
	}
	if options.PollInterval <= 0 {
		options.PollInterval = 5 * time.Second
	}
	if options.RetryBase <= 0 {
		options.RetryBase = 30 * time.Second
	}
	if options.RetryMax <= 0 {
		options.RetryMax = 30 * time.Minute
	}
	if options.LeaseDuration <= 0 {
		options.LeaseDuration = 30 * time.Second
	}
	if options.BatchSize <= 0 {
		options.BatchSize = 20
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Jitter == nil {
		options.Jitter = func(delay time.Duration) time.Duration {
			if delay <= 0 {
				return 0
			}
			// Add up to 20% positive jitter so workers do not retry in lockstep.
			return time.Duration(rand.Int63n(max(int64(delay/5), 1)))
		}
	}
	if options.Logger == nil {
		options.Logger = zap.NewNop()
	}
	return &Service{
		store: store, cipher: cipher, renderer: renderer, sender: sender,
		maxAttempts: options.MaxAttempts, pollInterval: options.PollInterval,
		retryBase: options.RetryBase, retryMax: options.RetryMax,
		leaseDuration: options.LeaseDuration, batchSize: options.BatchSize,
		now: options.Now, jitter: options.Jitter, logger: options.Logger,
	}
}

func (s *Service) Run(ctx context.Context) error {
	for {
		if _, err := s.RunOnce(ctx); err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		timer := time.NewTimer(s.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func (s *Service) RunOnce(ctx context.Context) (Stats, error) {
	now := s.now().UTC()
	messages, err := s.store.ClaimEmailOutboxReady(ctx, now, now.Add(s.leaseDuration), s.batchSize)
	if err != nil {
		return Stats{}, err
	}
	stats := Stats{Claimed: len(messages)}
	type deliveryResult struct {
		outcome model.EmailOutboxStatus
		err     error
	}
	results := make(chan deliveryResult, len(messages))
	for _, item := range messages {
		item := item
		go func() {
			if err := ctx.Err(); err != nil {
				results <- deliveryResult{err: err}
				return
			}
			outcome, err := s.deliver(ctx, item, now)
			results <- deliveryResult{outcome: outcome, err: err}
		}()
	}
	var firstErr error
	for range messages {
		result := <-results
		if result.err != nil {
			if firstErr == nil {
				firstErr = result.err
			}
			continue
		}
		switch result.outcome {
		case model.EmailOutboxStatusSent:
			stats.Sent++
		case model.EmailOutboxStatusRetrying:
			stats.Retrying++
		case model.EmailOutboxStatusDeadLettered:
			stats.DeadLettered++
		case model.EmailOutboxStatusExpired:
			stats.Expired++
		}
	}
	return stats, firstErr
}

func (s *Service) deliver(ctx context.Context, item model.EmailOutbox, now time.Time) (model.EmailOutboxStatus, error) {
	if item.ExpiresAt != nil && !item.ExpiresAt.After(now) {
		return s.transition(ctx, item, model.EmailOutboxStatusExpired, item.AttemptCount, now, nil, true)
	}
	attempt := item.AttemptCount + 1
	payload, err := s.cipher.Decrypt(item.PayloadNonce, item.PayloadCiphertext)
	if err != nil {
		return s.deadLetter(ctx, item, attempt, now, err)
	}
	message, err := s.renderer.Render(item.ID, item.MessageType, payload)
	if err != nil {
		return s.deadLetter(ctx, item, attempt, now, err)
	}
	started := time.Now()
	err = s.sender.Send(ctx, message)
	elapsed := time.Since(started)
	fields := []zap.Field{
		zap.String("email_outbox_id", item.ID),
		zap.String("message_type", item.MessageType),
		zap.Int("attempt", attempt),
		zap.Duration("delivery_latency", elapsed),
	}
	if err == nil {
		sentAt := now
		s.logger.Info("email delivered", fields...)
		return s.transition(ctx, item, model.EmailOutboxStatusSent, attempt, now, &sentAt, true)
	}
	if emaildelivery.IsTransient(err) && attempt < s.maxAttempts {
		delay := s.retryDelay(attempt)
		message := sanitizeError(err)
		s.logger.Warn("email delivery retrying", append(fields, zap.String("error", message), zap.Duration("retry_delay", delay))...)
		return s.transitionWithError(ctx, item, model.EmailOutboxStatusRetrying, attempt, now.Add(delay), message, false)
	}
	s.logger.Error("email delivery dead lettered", append(fields, zap.String("error", sanitizeError(err)))...)
	return s.deadLetter(ctx, item, attempt, now, err)
}

func (s *Service) retryDelay(attempt int) time.Duration {
	factor := math.Pow(2, float64(max(attempt-1, 0)))
	delay := time.Duration(float64(s.retryBase) * factor)
	if delay > s.retryMax {
		delay = s.retryMax
	}
	return delay + s.jitter(delay)
}

func (s *Service) deadLetter(ctx context.Context, item model.EmailOutbox, attempt int, now time.Time, cause error) (model.EmailOutboxStatus, error) {
	return s.transitionWithError(ctx, item, model.EmailOutboxStatusDeadLettered, attempt, now, sanitizeError(cause), false)
}

func (s *Service) transitionWithError(ctx context.Context, item model.EmailOutbox, status model.EmailOutboxStatus, attempt int, availableAt time.Time, message string, clear bool) (model.EmailOutboxStatus, error) {
	return s.transition(ctx, item, status, attempt, availableAt, nil, clear, &message)
}

func (s *Service) transition(ctx context.Context, item model.EmailOutbox, status model.EmailOutboxStatus, attempt int, availableAt time.Time, sentAt *time.Time, clear bool, errors ...*string) (model.EmailOutboxStatus, error) {
	var lastError *string
	if len(errors) > 0 {
		lastError = errors[0]
	}
	applied, err := s.store.TransitionEmailOutbox(ctx, store.EmailOutboxTransitionInput{
		ID: item.ID, FromAttempt: item.AttemptCount, Status: status,
		AttemptCount: attempt, AvailableAt: availableAt, LastError: lastError,
		SentAt: sentAt, ClearPayload: clear,
	})
	if err != nil {
		return "", err
	}
	if !applied {
		return "", nil
	}
	return status, nil
}

func sanitizeError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.ReplaceAll(strings.ReplaceAll(err.Error(), "\r", " "), "\n", " ")
	if len(message) > 500 {
		message = message[:500]
	}
	return message
}
