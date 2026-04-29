package inbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"rtk_account_manager/internal/broker"
	"rtk_account_manager/internal/channel"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

type messageStore interface {
	CreateOrGetInboxMessage(ctx context.Context, in store.DeviceMessageInboxCreateInput) (model.DeviceMessageInbox, bool, error)
	GetDeviceOperation(ctx context.Context, operationID string) (model.DeviceOperation, error)
	RecordInboxProcessTransition(ctx context.Context, in store.InboxProcessTransitionInput) (store.InboxProcessTransitionResult, error)
}

type Options struct {
	Stream        string
	ConsumerGroup string
	MaxAttempts   int
	PollInterval  time.Duration
	BatchSize     int
	Now           func() time.Time
}

type Service struct {
	store         messageStore
	consumer      broker.Consumer
	stream        string
	consumerGroup string
	maxAttempts   int
	pollInterval  time.Duration
	batchSize     int
	now           func() time.Time
}

type Stats struct {
	Received     int
	Processed    int
	Retrying     int
	DeadLettered int
	Skipped      int
}

var transitionForPayloadFunc = buildTransitionForPayload

const (
	payloadSnapshotRawKey               = "_raw_payload"
	payloadSnapshotBase64Key            = "_raw_payload_base64"
	payloadSnapshotErrorKey             = "_payload_decode_error"
	payloadSnapshotEnvelopePartitionKey = "_envelope_partition_key"
)

func NewService(store messageStore, consumer broker.Consumer, opts Options) *Service {
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}

	pollInterval := opts.PollInterval
	if pollInterval <= 0 {
		pollInterval = 5 * time.Second
	}

	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = 20
	}

	maxAttempts := opts.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 5
	}

	stream := opts.Stream
	if stream == "" {
		stream = channel.StreamVideoAccountEvents
	}

	return &Service{
		store:         store,
		consumer:      consumer,
		stream:        stream,
		consumerGroup: opts.ConsumerGroup,
		maxAttempts:   maxAttempts,
		pollInterval:  pollInterval,
		batchSize:     batchSize,
		now:           nowFn,
	}
}

func (s *Service) Run(ctx context.Context) error {
	for {
		if _, err := s.RunOnce(ctx); err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			if broker.IsTransient(err) {
				if err := sleepContext(ctx, s.pollInterval); err != nil {
					return nil
				}
				continue
			}
			return err
		}

		if err := sleepContext(ctx, s.pollInterval); err != nil {
			return nil
		}
	}
}

func (s *Service) RunOnce(ctx context.Context) (Stats, error) {
	records, err := s.consumer.Receive(ctx, s.batchSize)
	if err != nil {
		return Stats{}, err
	}

	stats := Stats{Received: len(records)}
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return stats, err
		}

		outcome, err := s.processMessage(ctx, record)
		if err != nil {
			return stats, err
		}
		if err := record.Acknowledge(ctx); err != nil {
			return stats, err
		}

		switch outcome {
		case model.DeviceMessageInboxStatusProcessed:
			stats.Processed++
		case model.DeviceMessageInboxStatusRetrying:
			stats.Retrying++
		case model.DeviceMessageInboxStatusDeadLettered:
			stats.DeadLettered++
		case "":
			stats.Skipped++
		}
	}

	return stats, nil
}

func (s *Service) processMessage(ctx context.Context, record broker.Message) (model.DeviceMessageInboxStatus, error) {
	receivedAt := s.receivedAt(record.Envelope)
	payloadMap, payloadErr := payloadMapFromEnvelope(record.Envelope)

	message, created, err := s.createInboxMessage(ctx, store.DeviceMessageInboxCreateInput{
		MessageID:     record.Envelope.MessageID,
		OperationID:   record.Envelope.OperationID,
		CorrelationID: record.Envelope.CorrelationID,
		CausationID:   causationIDPtr(record.Envelope.CausationID),
		Stream:        record.Stream,
		MessageType:   string(record.Envelope.MessageType),
		SchemaVersion: record.Envelope.SchemaVersion,
		PartitionKey:  record.Envelope.PartitionKey,
		Payload:       payloadMap,
		Status:        model.DeviceMessageInboxStatusRetrying,
		AttemptCount:  0,
		ReceivedAt:    receivedAt,
	})
	if err != nil {
		return "", err
	}

	if !created {
		switch message.Status {
		case model.DeviceMessageInboxStatusProcessed, model.DeviceMessageInboxStatusDeadLettered:
			return "", nil
		}
	}

	attemptCount := message.AttemptCount + 1

	if record.Stream != s.stream {
		return model.DeviceMessageInboxStatusDeadLettered, s.recordDeadLetter(ctx, message, attemptCount, fmt.Errorf("unexpected stream %q", record.Stream))
	}

	payload, err := record.Envelope.ValidateAndDecode(s.stream)
	if err != nil {
		if payloadErr != nil {
			err = payloadErr
		}
		return model.DeviceMessageInboxStatusDeadLettered, s.recordDeadLetter(ctx, message, attemptCount, err)
	}

	transition, err := transitionForPayloadFunc(record.Envelope, payload)
	if err != nil {
		if isTransientProcessingError(err) && attemptCount < s.maxAttempts {
			return model.DeviceMessageInboxStatusRetrying, s.recordRetry(ctx, message, attemptCount, err)
		}
		return model.DeviceMessageInboxStatusDeadLettered, s.recordDeadLetter(ctx, message, attemptCount, err)
	}

	if transition.OperationStatus != nil {
		operation, err := s.store.GetDeviceOperation(ctx, message.OperationID)
		switch {
		case err == nil:
			if !partitionKeyMatchesOperationDevice(message.PartitionKey, operation.DeviceID) {
				return model.DeviceMessageInboxStatusDeadLettered, s.recordDeadLetter(ctx, message, attemptCount, fmt.Errorf("partition key %q does not match operation device %q", message.PartitionKey, operation.DeviceID))
			}
			if isCompletedLifecycleOperation(operation) {
				return model.DeviceMessageInboxStatusProcessed, s.markProcessedWithoutProjection(ctx, message, attemptCount)
			}
		case errors.Is(err, store.ErrNotFound):
		default:
			return "", err
		}
	}

	transition.MessageID = message.MessageID
	transition.MessageStatus = model.DeviceMessageInboxStatusProcessed
	transition.AttemptCount = attemptCount
	processedAt := s.currentTime()
	transition.ProcessedAt = &processedAt

	if _, err := s.store.RecordInboxProcessTransition(ctx, transition); err != nil {
		return "", err
	}
	return model.DeviceMessageInboxStatusProcessed, nil
}

func (s *Service) createInboxMessage(ctx context.Context, in store.DeviceMessageInboxCreateInput) (model.DeviceMessageInbox, bool, error) {
	if strings.TrimSpace(in.PartitionKey) == "" {
		originalPartitionKey := in.PartitionKey
		operation, err := s.store.GetDeviceOperation(ctx, in.OperationID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return model.DeviceMessageInbox{}, false, err
		}
		if err == nil {
			in.PartitionKey = operation.DeviceID
			in.Payload = withEnvelopePartitionKeySnapshot(in.Payload, originalPartitionKey, in.PartitionKey)
		}
	}
	return s.store.CreateOrGetInboxMessage(ctx, in)
}

func (s *Service) markProcessedWithoutProjection(ctx context.Context, message model.DeviceMessageInbox, attemptCount int) error {
	processedAt := s.currentTime()
	_, err := s.store.RecordInboxProcessTransition(ctx, store.InboxProcessTransitionInput{
		MessageID:     message.MessageID,
		MessageStatus: model.DeviceMessageInboxStatusProcessed,
		AttemptCount:  attemptCount,
		ProcessedAt:   &processedAt,
	})
	return err
}

func (s *Service) recordRetry(ctx context.Context, message model.DeviceMessageInbox, attemptCount int, cause error) error {
	lastError := cause.Error()
	_, err := s.store.RecordInboxProcessTransition(ctx, store.InboxProcessTransitionInput{
		MessageID:     message.MessageID,
		MessageStatus: model.DeviceMessageInboxStatusRetrying,
		AttemptCount:  attemptCount,
		LastError:     &lastError,
	})
	return err
}

func (s *Service) recordDeadLetter(ctx context.Context, message model.DeviceMessageInbox, attemptCount int, cause error) error {
	lastError := cause.Error()
	processedAt := s.currentTime()
	_, err := s.store.RecordInboxProcessTransition(ctx, store.InboxProcessTransitionInput{
		MessageID:     message.MessageID,
		MessageStatus: model.DeviceMessageInboxStatusDeadLettered,
		AttemptCount:  attemptCount,
		LastError:     &lastError,
		ProcessedAt:   &processedAt,
	})
	return err
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func partitionKeyMatchesOperationDevice(partitionKey, deviceID string) bool {
	return strings.TrimSpace(partitionKey) != "" && partitionKey == deviceID
}

func buildTransitionForPayload(envelope channel.Envelope, payload channel.Payload) (store.InboxProcessTransitionInput, error) {
	switch typed := payload.(type) {
	case *channel.DeviceProvisionSucceededPayload:
		return successTransition(
			typed.OrgID,
			typed.AccountDeviceID,
			typed.ActivatedAt.UTC(),
			store.ProvisionSucceededProjection(*typed),
			map[string]any{
				"video_cloud_devid": typed.VideoCloudDevid,
				"activity_id":       typed.ActivityID,
				"activated_at":      typed.ActivatedAt.UTC(),
			},
		), nil
	case *channel.DeviceProvisionFailedPayload:
		return failureTransition(
			typed.OrgID,
			typed.AccountDeviceID,
			typed.FailedAt.UTC(),
			store.ProvisionFailedProjection(*typed),
			map[string]any{
				"video_cloud_devid": typed.VideoCloudDevid,
				"activity_id":       typed.ActivityID,
				"error_code":        typed.ErrorCode,
				"error_message":     typed.ErrorMessage,
				"retryable":         typed.Retryable,
				"failed_at":         typed.FailedAt.UTC(),
			},
			typed.ErrorCode,
			typed.ErrorMessage,
			typed.Retryable,
		), nil
	case *channel.DeviceDeactivateSucceededPayload:
		return successTransition(
			typed.OrgID,
			typed.AccountDeviceID,
			typed.DeactivatedAt.UTC(),
			store.DeactivateSucceededProjection(*typed),
			map[string]any{
				"video_cloud_devid": typed.VideoCloudDevid,
				"deactivated_at":    typed.DeactivatedAt.UTC(),
			},
		), nil
	case *channel.DeviceDeactivateFailedPayload:
		return failureTransition(
			typed.OrgID,
			typed.AccountDeviceID,
			typed.FailedAt.UTC(),
			store.DeactivateFailedProjection(*typed),
			map[string]any{
				"video_cloud_devid": typed.VideoCloudDevid,
				"error_code":        typed.ErrorCode,
				"error_message":     typed.ErrorMessage,
				"retryable":         typed.Retryable,
				"failed_at":         typed.FailedAt.UTC(),
			},
			typed.ErrorCode,
			typed.ErrorMessage,
			typed.Retryable,
		), nil
	case *channel.DeviceOnlineChangedPayload:
		return store.InboxProcessTransitionInput{
			OrganizationID: typed.OrgID,
			DeviceID:       typed.AccountDeviceID,
			Projection:     projectionPtr(store.OnlineChangedProjection(*typed)),
		}, nil
	case *channel.DeviceMetadataChangedPayload:
		return store.InboxProcessTransitionInput{
			OrganizationID: typed.OrgID,
			DeviceID:       typed.AccountDeviceID,
			Projection:     projectionPtr(store.MetadataChangedProjection(*typed)),
		}, nil
	default:
		return store.InboxProcessTransitionInput{}, fmt.Errorf("unsupported message type %q", envelope.MessageType)
	}
}

func successTransition(orgID, deviceID string, completedAt time.Time, projection store.DeviceProjectionInput, result map[string]any) store.InboxProcessTransitionInput {
	status := model.DeviceOperationStatusSucceeded
	return store.InboxProcessTransitionInput{
		OperationStatus:      &status,
		OperationResult:      result,
		OperationCompletedAt: &completedAt,
		OrganizationID:       orgID,
		DeviceID:             deviceID,
		Projection:           projectionPtr(projection),
	}
}

func failureTransition(orgID, deviceID string, completedAt time.Time, projection store.DeviceProjectionInput, result map[string]any, errorCode, errorMessage string, retryable bool) store.InboxProcessTransitionInput {
	status := model.DeviceOperationStatusFailed
	return store.InboxProcessTransitionInput{
		OperationStatus:       &status,
		OperationResult:       result,
		OperationErrorCode:    stringPtr(errorCode),
		OperationErrorMessage: stringPtr(errorMessage),
		OperationRetryable:    boolPtr(retryable),
		OperationCompletedAt:  &completedAt,
		OrganizationID:        orgID,
		DeviceID:              deviceID,
		Projection:            projectionPtr(projection),
	}
}

func payloadMapFromEnvelope(envelope channel.Envelope) (map[string]any, error) {
	if len(envelope.Payload) == 0 {
		return map[string]any{}, nil
	}
	var payload map[string]any
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		snapshot := map[string]any{
			payloadSnapshotBase64Key: base64.StdEncoding.EncodeToString(envelope.Payload),
			payloadSnapshotErrorKey:  err.Error(),
		}
		if utf8.Valid(envelope.Payload) {
			snapshot[payloadSnapshotRawKey] = string(envelope.Payload)
		}
		return snapshot, err
	}
	if payload == nil {
		return map[string]any{}, nil
	}
	return payload, nil
}

func withEnvelopePartitionKeySnapshot(payload map[string]any, originalPartitionKey, persistedPartitionKey string) map[string]any {
	if originalPartitionKey == persistedPartitionKey {
		return payload
	}

	snapshot := clonePayloadMap(payload)
	snapshot[payloadSnapshotEnvelopePartitionKey] = originalPartitionKey
	return snapshot
}

func clonePayloadMap(payload map[string]any) map[string]any {
	if len(payload) == 0 {
		return map[string]any{}
	}

	cloned := make(map[string]any, len(payload))
	for key, value := range payload {
		cloned[key] = value
	}
	return cloned
}

func isCompletedLifecycleOperation(operation model.DeviceOperation) bool {
	switch operation.Status {
	case model.DeviceOperationStatusSucceeded, model.DeviceOperationStatusFailed:
		return operation.OperationType == model.DeviceOperationTypeProvision || operation.OperationType == model.DeviceOperationTypeDeactivate
	default:
		return false
	}
}

func projectionPtr(projection store.DeviceProjectionInput) *store.DeviceProjectionInput {
	return &projection
}

func stringPtr(value string) *string {
	return &value
}

func boolPtr(value bool) *bool {
	return &value
}

func causationIDPtr(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func (s *Service) currentTime() time.Time {
	return s.now().UTC().Truncate(time.Microsecond)
}

func (s *Service) receivedAt(envelope channel.Envelope) time.Time {
	if envelope.OccurredAt.IsZero() {
		return s.currentTime()
	}
	return envelope.OccurredAt.UTC().Truncate(time.Microsecond)
}

func isTransientProcessingError(err error) bool {
	return broker.IsTransient(err)
}
