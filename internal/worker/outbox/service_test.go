package outbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"rtk_account_manager/internal/broker"
	"rtk_account_manager/internal/channel"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

type fakeStore struct {
	claimed       []model.DeviceMessageOutbox
	transitions   []store.OutboxPublishTransitionInput
	transitionErr error
}

func (s *fakeStore) ClaimOutboxMessagesReady(_ context.Context, _ time.Time, _ time.Time, _ int) ([]model.DeviceMessageOutbox, error) {
	return append([]model.DeviceMessageOutbox(nil), s.claimed...), nil
}

func (s *fakeStore) RecordOutboxPublishTransition(_ context.Context, in store.OutboxPublishTransitionInput) (store.OutboxPublishTransitionResult, error) {
	s.transitions = append(s.transitions, in)
	return store.OutboxPublishTransitionResult{}, s.transitionErr
}

type fakePublisher struct {
	err error
}

func (p fakePublisher) Publish(context.Context, string, channel.Envelope) error {
	return p.err
}

func (p fakePublisher) Close(context.Context) error {
	return nil
}

func TestRunOnceMarksSuccessfulPublishes(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	outboxStore := &fakeStore{
		claimed: []model.DeviceMessageOutbox{validMessage(now)},
	}

	service := NewService(outboxStore, fakePublisher{}, Options{
		MaxAttempts:   5,
		PollInterval:  5 * time.Second,
		RetryDelay:    10 * time.Second,
		LeaseDuration: 30 * time.Second,
		Now:           func() time.Time { return now },
	})

	stats, err := service.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Published != 1 || stats.Retrying != 0 || stats.DeadLettered != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	if len(outboxStore.transitions) != 1 {
		t.Fatalf("expected one transition, got %d", len(outboxStore.transitions))
	}
	transition := outboxStore.transitions[0]
	if transition.MessageStatus != model.DeviceMessageOutboxStatusPublished {
		t.Fatalf("expected published status, got %s", transition.MessageStatus)
	}
	if transition.ExpectedMessageStatus != model.DeviceMessageOutboxStatusPending {
		t.Fatalf("expected pending claim status, got %s", transition.ExpectedMessageStatus)
	}
	if transition.ExpectedAttemptCount != 0 {
		t.Fatalf("expected prior attempt count 0, got %d", transition.ExpectedAttemptCount)
	}
	if !transition.ExpectedAvailableAt.Equal(now) {
		t.Fatalf("expected claim lease timestamp %s, got %s", now, transition.ExpectedAvailableAt)
	}
	if transition.OperationStatus != model.DeviceOperationStatusPublished {
		t.Fatalf("expected operation published status, got %s", transition.OperationStatus)
	}
	if transition.PublishedAt == nil || !transition.PublishedAt.Equal(now) {
		t.Fatalf("expected published_at to match worker clock, got %+v", transition.PublishedAt)
	}
}

func TestRunOnceSchedulesTransientRetry(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	outboxStore := &fakeStore{
		claimed: []model.DeviceMessageOutbox{validMessage(now)},
	}

	service := NewService(outboxStore, fakePublisher{err: broker.Transient(errors.New("broker unavailable"))}, Options{
		MaxAttempts: 3,
		RetryDelay:  2 * time.Minute,
		Now:         func() time.Time { return now },
	})

	stats, err := service.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Retrying != 1 || stats.DeadLettered != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	transition := outboxStore.transitions[0]
	if transition.MessageStatus != model.DeviceMessageOutboxStatusRetrying {
		t.Fatalf("expected retrying status, got %s", transition.MessageStatus)
	}
	if transition.AttemptCount != 1 {
		t.Fatalf("expected attempt count 1, got %d", transition.AttemptCount)
	}
	if !transition.AvailableAt.Equal(now.Add(2 * time.Minute)) {
		t.Fatalf("expected retry schedule, got %s", transition.AvailableAt)
	}
}

func TestRunOnceDeadLettersExhaustedPublishFailures(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	message := validMessage(now)
	message.AttemptCount = 2
	outboxStore := &fakeStore{
		claimed: []model.DeviceMessageOutbox{message},
	}

	service := NewService(outboxStore, fakePublisher{err: broker.Transient(errors.New("still failing"))}, Options{
		MaxAttempts: 3,
		Now:         func() time.Time { return now },
	})

	stats, err := service.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.DeadLettered != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	transition := outboxStore.transitions[0]
	if transition.MessageStatus != model.DeviceMessageOutboxStatusDeadLettered {
		t.Fatalf("expected dead-lettered status, got %s", transition.MessageStatus)
	}
	if transition.OperationCompletedAt == nil || !transition.OperationCompletedAt.Equal(now) {
		t.Fatalf("expected completed_at on dead letter, got %+v", transition.OperationCompletedAt)
	}
}

func TestRunOnceDeadLettersInvalidOutboxPayload(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	message := validMessage(now)
	message.MessageType = "UnknownMessageType"
	outboxStore := &fakeStore{
		claimed: []model.DeviceMessageOutbox{message},
	}

	service := NewService(outboxStore, fakePublisher{}, Options{
		MaxAttempts: 5,
		Now:         func() time.Time { return now },
	})

	stats, err := service.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.DeadLettered != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestRunOnceIgnoresStaleLeaseTransitionConflict(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	outboxStore := &fakeStore{
		claimed:       []model.DeviceMessageOutbox{validMessage(now)},
		transitionErr: store.ErrConflict,
	}

	service := NewService(outboxStore, fakePublisher{}, Options{
		MaxAttempts:   5,
		LeaseDuration: 30 * time.Second,
		Now:           func() time.Time { return now },
	})

	stats, err := service.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Claimed != 1 || stats.Published != 0 || stats.Retrying != 0 || stats.DeadLettered != 0 {
		t.Fatalf("expected stale-lease conflict to be a no-op, got %+v", stats)
	}
	if len(outboxStore.transitions) != 1 {
		t.Fatalf("expected one attempted transition, got %d", len(outboxStore.transitions))
	}
}

func TestRunOnceIgnoresConflictWhenRetryLosesToPublished(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	outboxStore := &fakeStore{
		claimed:       []model.DeviceMessageOutbox{validMessage(now)},
		transitionErr: store.ErrConflict,
	}

	service := NewService(outboxStore, fakePublisher{err: broker.Transient(errors.New("publish already succeeded elsewhere"))}, Options{
		MaxAttempts: 5,
		RetryDelay:  time.Minute,
		Now:         func() time.Time { return now },
	})

	stats, err := service.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Claimed != 1 || stats.Published != 0 || stats.Retrying != 0 || stats.DeadLettered != 0 {
		t.Fatalf("expected conflicting retry transition to be a no-op, got %+v", stats)
	}
	if len(outboxStore.transitions) != 1 {
		t.Fatalf("expected one attempted transition, got %d", len(outboxStore.transitions))
	}
	if outboxStore.transitions[0].MessageStatus != model.DeviceMessageOutboxStatusRetrying {
		t.Fatalf("expected retry transition attempt, got %+v", outboxStore.transitions[0])
	}
}

func validMessage(createdAt time.Time) model.DeviceMessageOutbox {
	return model.DeviceMessageOutbox{
		MessageID:     "msg-1",
		OperationID:   "op-1",
		CorrelationID: "corr-1",
		Stream:        channel.StreamAccountVideoCommands,
		MessageType:   string(channel.MessageTypeDeviceProvisionRequested),
		SchemaVersion: channel.SchemaVersionV1,
		PartitionKey:  "11111111-1111-1111-1111-111111111111",
		Payload: map[string]any{
			"org_id":            "00000000-0000-0000-0000-000000000001",
			"account_device_id": "11111111-1111-1111-1111-111111111111",
			"video_cloud_devid": "video-1",
			"activity_id":       "activity-1",
			"clip_public_key":   "clip",
			"requested_by":      "user-1",
		},
		Status:      model.DeviceMessageOutboxStatusPending,
		CreatedAt:   createdAt,
		AvailableAt: createdAt,
	}
}
