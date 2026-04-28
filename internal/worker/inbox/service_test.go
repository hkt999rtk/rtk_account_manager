package inbox

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"rtk_account_manager/internal/broker"
	"rtk_account_manager/internal/channel"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

type fakeStore struct {
	message      model.DeviceMessageInbox
	created      bool
	createInputs []store.DeviceMessageInboxCreateInput
	transitions  []store.InboxProcessTransitionInput
}

func (s *fakeStore) CreateOrGetInboxMessage(_ context.Context, in store.DeviceMessageInboxCreateInput) (model.DeviceMessageInbox, bool, error) {
	s.createInputs = append(s.createInputs, in)
	if s.message.MessageID == "" {
		s.message = model.DeviceMessageInbox{
			MessageID:     in.MessageID,
			OperationID:   in.OperationID,
			CorrelationID: in.CorrelationID,
			Stream:        in.Stream,
			MessageType:   in.MessageType,
			SchemaVersion: in.SchemaVersion,
			PartitionKey:  in.PartitionKey,
			Payload:       in.Payload,
			Status:        in.Status,
		}
	}
	return s.message, s.created, nil
}

func (s *fakeStore) RecordInboxProcessTransition(_ context.Context, in store.InboxProcessTransitionInput) (store.InboxProcessTransitionResult, error) {
	s.transitions = append(s.transitions, in)
	return store.InboxProcessTransitionResult{}, nil
}

type fakeConsumer struct {
	messages []broker.Message
	err      error
}

func (c fakeConsumer) Receive(context.Context, int) ([]broker.Message, error) {
	return append([]broker.Message(nil), c.messages...), c.err
}

func TestRunOnceProcessesProvisionSuccess(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	inboxStore := &fakeStore{created: true}
	service := NewService(inboxStore, fakeConsumer{
		messages: []broker.Message{validProvisionSucceededMessage(now)},
	}, Options{
		Now: func() time.Time { return now },
	})

	stats, err := service.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Processed != 1 || stats.DeadLettered != 0 || stats.Retrying != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	if len(inboxStore.transitions) != 1 {
		t.Fatalf("expected one transition, got %d", len(inboxStore.transitions))
	}
	transition := inboxStore.transitions[0]
	if transition.MessageStatus != model.DeviceMessageInboxStatusProcessed {
		t.Fatalf("expected processed status, got %s", transition.MessageStatus)
	}
	if transition.OperationStatus == nil || *transition.OperationStatus != model.DeviceOperationStatusSucceeded {
		t.Fatalf("expected succeeded operation status, got %+v", transition.OperationStatus)
	}
	if transition.Projection == nil {
		t.Fatal("expected device projection")
	}
}

func TestRunOnceSkipsPreviouslyProcessedDuplicates(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	inboxStore := &fakeStore{
		created: false,
		message: model.DeviceMessageInbox{
			MessageID:    "msg-1",
			Status:       model.DeviceMessageInboxStatusProcessed,
			AttemptCount: 1,
		},
	}
	service := NewService(inboxStore, fakeConsumer{
		messages: []broker.Message{validProvisionSucceededMessage(now)},
	}, Options{
		Now: func() time.Time { return now },
	})

	stats, err := service.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Skipped != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	if len(inboxStore.transitions) != 0 {
		t.Fatalf("expected no transitions, got %d", len(inboxStore.transitions))
	}
}

func TestRunOnceDeadLettersInvalidMessages(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	message := validProvisionSucceededMessage(now)
	message.Stream = "wrong.stream"
	inboxStore := &fakeStore{created: true}
	service := NewService(inboxStore, fakeConsumer{
		messages: []broker.Message{message},
	}, Options{
		Now: func() time.Time { return now },
	})

	stats, err := service.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.DeadLettered != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	if got := inboxStore.transitions[0].MessageStatus; got != model.DeviceMessageInboxStatusDeadLettered {
		t.Fatalf("expected dead-lettered status, got %s", got)
	}
}

func TestRunOnceRetriesTransientProjectionFailures(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	inboxStore := &fakeStore{created: true}
	service := NewService(inboxStore, fakeConsumer{
		messages: []broker.Message{validProvisionSucceededMessage(now)},
	}, Options{
		Now:         func() time.Time { return now },
		MaxAttempts: 3,
	})

	original := transitionForPayloadFunc
	defer func() { transitionForPayloadFunc = original }()
	transitionForPayloadFunc = func(channel.Envelope, channel.Payload) (store.InboxProcessTransitionInput, error) {
		return store.InboxProcessTransitionInput{}, broker.Transient(errors.New("temporary projection failure"))
	}

	stats, err := service.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Retrying != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	if got := inboxStore.transitions[0].MessageStatus; got != model.DeviceMessageInboxStatusRetrying {
		t.Fatalf("expected retrying status, got %s", got)
	}
}

func validProvisionSucceededMessage(now time.Time) broker.Message {
	payload := channel.DeviceProvisionSucceededPayload{
		OrgID:           "00000000-0000-0000-0000-000000000001",
		AccountDeviceID: "11111111-1111-1111-1111-111111111111",
		VideoCloudDevid: "video-1",
		ActivityID:      "activity-1",
		ActivatedAt:     now,
	}
	rawPayload, _ := json.Marshal(payload)
	return broker.Message{
		Stream: channel.StreamVideoAccountEvents,
		Envelope: channel.Envelope{
			MessageID:     "msg-1",
			CorrelationID: "corr-1",
			OperationID:   "op-1",
			SourceService: channel.ServiceRealtekVideoCloud,
			TargetService: channel.ServiceAccountManager,
			MessageType:   channel.MessageTypeDeviceProvisionSucceeded,
			SchemaVersion: channel.SchemaVersionV1,
			PartitionKey:  payload.AccountDeviceID,
			OccurredAt:    now,
			Payload:       rawPayload,
		},
	}
}
