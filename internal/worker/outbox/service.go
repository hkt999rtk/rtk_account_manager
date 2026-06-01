package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	cloudlogger "github.com/hkt999rtk/rtk_cloud_logger"
	"go.uber.org/zap"

	"rtk_account_manager/internal/broker"
	"rtk_account_manager/internal/channel"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

const operationPublishFailedCode = "publish_failed"

type messageStore interface {
	ClaimOutboxMessagesReady(ctx context.Context, readyBefore, claimUntil time.Time, limit int) ([]model.DeviceMessageOutbox, error)
	RecordOutboxPublishTransition(ctx context.Context, in store.OutboxPublishTransitionInput) (store.OutboxPublishTransitionResult, error)
}

type Options struct {
	MaxAttempts   int
	PollInterval  time.Duration
	RetryDelay    time.Duration
	LeaseDuration time.Duration
	BatchSize     int
	Now           func() time.Time
	Logger        *zap.Logger
}

type Service struct {
	store         messageStore
	publisher     broker.Publisher
	maxAttempts   int
	pollInterval  time.Duration
	retryDelay    time.Duration
	leaseDuration time.Duration
	batchSize     int
	now           func() time.Time
	logger        *zap.Logger
}

type Stats struct {
	Claimed      int
	Published    int
	Retrying     int
	DeadLettered int
}

func NewService(store messageStore, publisher broker.Publisher, opts Options) *Service {
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}

	maxAttempts := opts.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 5
	}

	pollInterval := opts.PollInterval
	if pollInterval <= 0 {
		pollInterval = 5 * time.Second
	}

	retryDelay := opts.RetryDelay
	if retryDelay <= 0 {
		retryDelay = pollInterval
	}

	leaseDuration := opts.LeaseDuration
	if leaseDuration <= 0 {
		leaseDuration = 30 * time.Second
	}

	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = 20
	}

	return &Service{
		store:         store,
		publisher:     publisher,
		maxAttempts:   maxAttempts,
		pollInterval:  pollInterval,
		retryDelay:    retryDelay,
		leaseDuration: leaseDuration,
		batchSize:     batchSize,
		now:           nowFn,
		logger:        loggerOrNop(opts.Logger),
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
	now := s.currentTime()
	claimed, err := s.store.ClaimOutboxMessagesReady(ctx, now, now.Add(s.leaseDuration), s.batchSize)
	if err != nil {
		s.logger.Error("outbox claim failed", zap.Error(err))
		return Stats{}, err
	}

	stats := Stats{Claimed: len(claimed)}
	for _, message := range claimed {
		if err := ctx.Err(); err != nil {
			return stats, err
		}

		outcome, err := s.publishMessage(ctx, message)
		if err != nil {
			s.logger.Error("outbox message failed", append(outboxLogFields(message), zap.Error(err))...)
			return stats, err
		}

		switch outcome {
		case model.DeviceMessageOutboxStatusPublished:
			stats.Published++
			s.logger.Info("outbox message published", outboxLogFields(message)...)
		case model.DeviceMessageOutboxStatusRetrying:
			stats.Retrying++
			s.logger.Warn("outbox message retrying", outboxLogFields(message)...)
		case model.DeviceMessageOutboxStatusDeadLettered:
			stats.DeadLettered++
			s.logger.Error("outbox message dead lettered", outboxLogFields(message)...)
		}
	}

	return stats, nil
}

func (s *Service) publishMessage(ctx context.Context, message model.DeviceMessageOutbox) (model.DeviceMessageOutboxStatus, error) {
	attemptedAt := s.currentTime()
	attemptCount := message.AttemptCount + 1

	envelope, err := envelopeFromMessage(message)
	if err != nil {
		applied, err := s.recordDeadLetter(ctx, message, attemptCount, attemptedAt, fmt.Errorf("invalid outbox message: %w", err))
		if err != nil {
			return "", err
		}
		if !applied {
			return "", nil
		}
		return model.DeviceMessageOutboxStatusDeadLettered, nil
	}

	if err := s.publisher.Publish(ctx, message.Stream, envelope); err != nil {
		if errors.Is(err, context.Canceled) {
			return "", err
		}
		if broker.IsTransient(err) && attemptCount < s.maxAttempts {
			applied, err := s.recordRetry(ctx, message, attemptCount, attemptedAt, err)
			if err != nil {
				return "", err
			}
			if !applied {
				return "", nil
			}
			return model.DeviceMessageOutboxStatusRetrying, nil
		}
		applied, err := s.recordDeadLetter(ctx, message, attemptCount, attemptedAt, err)
		if err != nil {
			return "", err
		}
		if !applied {
			return "", nil
		}
		return model.DeviceMessageOutboxStatusDeadLettered, nil
	}

	applied, err := s.recordTransition(ctx, message, store.OutboxPublishTransitionInput{
		MessageStatus:   model.DeviceMessageOutboxStatusPublished,
		AttemptCount:    attemptCount,
		AvailableAt:     attemptedAt,
		PublishedAt:     &attemptedAt,
		OperationStatus: model.DeviceOperationStatusPublished,
	})
	if err != nil {
		return "", err
	}
	if !applied {
		return "", nil
	}
	return model.DeviceMessageOutboxStatusPublished, nil
}

func (s *Service) recordRetry(ctx context.Context, message model.DeviceMessageOutbox, attemptCount int, attemptedAt time.Time, cause error) (bool, error) {
	lastError := cause.Error()
	retryable := true
	return s.recordTransition(ctx, message, store.OutboxPublishTransitionInput{
		MessageStatus:         model.DeviceMessageOutboxStatusRetrying,
		AttemptCount:          attemptCount,
		LastError:             &lastError,
		AvailableAt:           attemptedAt.Add(s.retryDelay),
		OperationStatus:       model.DeviceOperationStatusRetrying,
		OperationErrorCode:    stringPtr(operationPublishFailedCode),
		OperationErrorMessage: &lastError,
		OperationRetryable:    &retryable,
	})
}

func (s *Service) recordDeadLetter(ctx context.Context, message model.DeviceMessageOutbox, attemptCount int, attemptedAt time.Time, cause error) (bool, error) {
	lastError := cause.Error()
	retryable := false
	return s.recordTransition(ctx, message, store.OutboxPublishTransitionInput{
		MessageStatus:         model.DeviceMessageOutboxStatusDeadLettered,
		AttemptCount:          attemptCount,
		LastError:             &lastError,
		AvailableAt:           attemptedAt,
		OperationStatus:       model.DeviceOperationStatusDeadLettered,
		OperationErrorCode:    stringPtr(operationPublishFailedCode),
		OperationErrorMessage: &lastError,
		OperationRetryable:    &retryable,
		OperationCompletedAt:  &attemptedAt,
	})
}

func (s *Service) recordTransition(ctx context.Context, message model.DeviceMessageOutbox, in store.OutboxPublishTransitionInput) (bool, error) {
	in.MessageID = message.MessageID
	in.ExpectedMessageStatus = message.Status
	in.ExpectedAttemptCount = message.AttemptCount
	in.ExpectedAvailableAt = message.AvailableAt

	_, err := s.store.RecordOutboxPublishTransition(ctx, in)
	if errors.Is(err, store.ErrConflict) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func envelopeFromMessage(message model.DeviceMessageOutbox) (channel.Envelope, error) {
	messageType := channel.MessageType(message.MessageType)
	route, err := channel.RouteForMessageType(messageType)
	if err != nil {
		return channel.Envelope{}, err
	}

	rawPayload, err := json.Marshal(message.Payload)
	if err != nil {
		return channel.Envelope{}, err
	}

	envelope := channel.Envelope{
		MessageID:     message.MessageID,
		CorrelationID: message.CorrelationID,
		OperationID:   message.OperationID,
		SourceService: route.SourceService,
		TargetService: route.TargetService,
		MessageType:   messageType,
		SchemaVersion: message.SchemaVersion,
		PartitionKey:  message.PartitionKey,
		OccurredAt:    message.CreatedAt.UTC(),
		Payload:       rawPayload,
	}
	if message.CausationID != nil {
		envelope.CausationID = *message.CausationID
	}

	if err := envelope.Validate(message.Stream); err != nil {
		return channel.Envelope{}, err
	}
	return envelope, nil
}

func (s *Service) currentTime() time.Time {
	return s.now().UTC().Truncate(time.Microsecond)
}

func stringPtr(value string) *string {
	return &value
}

func loggerOrNop(logger *zap.Logger) *zap.Logger {
	if logger == nil {
		return cloudlogger.Nop()
	}
	return logger
}

func outboxLogFields(message model.DeviceMessageOutbox) []zap.Field {
	return []zap.Field{
		zap.String("message_id", message.MessageID),
		zap.String("operation_id", message.OperationID),
		zap.String("trace_id", message.CorrelationID),
		zap.String("request_id", message.CorrelationID),
		zap.String("stream", message.Stream),
		zap.String("message_type", message.MessageType),
		zap.Int("attempt_count", message.AttemptCount+1),
	}
}
