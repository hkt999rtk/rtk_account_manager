package inbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"rtk_account_manager/internal/broker"
	"rtk_account_manager/internal/channel"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

type fakeStore struct {
	message       model.DeviceMessageInbox
	operation     model.DeviceOperation
	created       bool
	createInputs  []store.DeviceMessageInboxCreateInput
	transitions   []store.InboxProcessTransitionInput
	createErr     error
	transitionErr error
	operationErr  error
}

func (s *fakeStore) CreateOrGetInboxMessage(_ context.Context, in store.DeviceMessageInboxCreateInput) (model.DeviceMessageInbox, bool, error) {
	if s.createErr != nil {
		return model.DeviceMessageInbox{}, false, s.createErr
	}
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
	if s.transitionErr != nil {
		return store.InboxProcessTransitionResult{}, s.transitionErr
	}
	s.transitions = append(s.transitions, in)
	return store.InboxProcessTransitionResult{}, nil
}

func (s *fakeStore) GetDeviceOperation(_ context.Context, operationID string) (model.DeviceOperation, error) {
	if s.operationErr != nil {
		return model.DeviceOperation{}, s.operationErr
	}
	if s.operation.OperationID == "" {
		return model.DeviceOperation{}, store.ErrNotFound
	}
	if s.operation.OperationID != operationID {
		return model.DeviceOperation{}, store.ErrNotFound
	}
	return s.operation, nil
}

type fakeConsumer struct {
	messages []broker.Message
	err      error
}

func (c fakeConsumer) Receive(context.Context, int) ([]broker.Message, error) {
	return append([]broker.Message(nil), c.messages...), c.err
}

func (c fakeConsumer) Close(context.Context) error {
	return nil
}

type scriptedConsumer struct {
	results []scriptedReceiveResult
	calls   int
}

type scriptedReceiveResult struct {
	messages []broker.Message
	err      error
}

func (c *scriptedConsumer) Receive(context.Context, int) ([]broker.Message, error) {
	if c.calls >= len(c.results) {
		return nil, context.Canceled
	}
	result := c.results[c.calls]
	c.calls++
	return append([]broker.Message(nil), result.messages...), result.err
}

func (c *scriptedConsumer) Close(context.Context) error {
	return nil
}

func TestRunOnceProcessesProvisionSuccess(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	inboxStore := &fakeStore{created: true}
	acked := 0
	service := NewService(inboxStore, fakeConsumer{
		messages: []broker.Message{validProvisionSucceededMessage(now).WithAck(func(context.Context) error {
			acked++
			return nil
		})},
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
	if acked != 1 {
		t.Fatalf("expected processed message to be acknowledged once, got %d", acked)
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

func TestRunOnceDoesNotAcknowledgeWhenTransitionFails(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	inboxStore := &fakeStore{
		created:       true,
		transitionErr: errors.New("write failed"),
	}
	acked := 0
	service := NewService(inboxStore, fakeConsumer{
		messages: []broker.Message{validProvisionSucceededMessage(now).WithAck(func(context.Context) error {
			acked++
			return nil
		})},
	}, Options{
		Now: func() time.Time { return now },
	})

	if _, err := service.RunOnce(context.Background()); err == nil {
		t.Fatal("expected transition failure")
	}
	if acked != 0 {
		t.Fatalf("expected message to remain unacknowledged on transition failure, got %d", acked)
	}
}

func TestRunOnceDoesNotAcknowledgeRetryingMessages(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	inboxStore := &fakeStore{
		created: true,
		operation: model.DeviceOperation{
			OperationID: "op-1",
			DeviceID:    "11111111-1111-1111-1111-111111111111",
		},
	}
	acked := 0
	service := NewService(inboxStore, fakeConsumer{
		messages: []broker.Message{validProvisionSucceededMessage(now).WithAck(func(context.Context) error {
			acked++
			return nil
		})},
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
	if acked != 0 {
		t.Fatalf("expected retrying message to remain unacknowledged, got %d", acked)
	}
	if got := inboxStore.transitions[0].MessageStatus; got != model.DeviceMessageInboxStatusRetrying {
		t.Fatalf("expected retrying status, got %s", got)
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
	acked := 0
	service := NewService(inboxStore, fakeConsumer{
		messages: []broker.Message{validProvisionSucceededMessage(now).WithAck(func(context.Context) error {
			acked++
			return nil
		})},
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
	if acked != 1 {
		t.Fatalf("expected skipped duplicate to be acknowledged once, got %d", acked)
	}
}

func TestRunOnceSkipsCompletedLifecycleReplayWithNewMessageID(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	inboxStore := &fakeStore{
		created: true,
		operation: model.DeviceOperation{
			OperationID:   "op-1",
			DeviceID:      "11111111-1111-1111-1111-111111111111",
			OperationType: model.DeviceOperationTypeProvision,
			Status:        model.DeviceOperationStatusSucceeded,
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
	if stats.Processed != 1 || stats.DeadLettered != 0 || stats.Retrying != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	if len(inboxStore.transitions) != 1 {
		t.Fatalf("expected one transition, got %d", len(inboxStore.transitions))
	}
	transition := inboxStore.transitions[0]
	if transition.MessageStatus != model.DeviceMessageInboxStatusProcessed {
		t.Fatalf("expected replay inbox row to be marked processed, got %s", transition.MessageStatus)
	}
	if transition.OperationStatus != nil {
		t.Fatalf("expected completed operation replay to skip operation update, got %+v", transition.OperationStatus)
	}
	if transition.Projection != nil {
		t.Fatalf("expected completed operation replay to skip device projection, got %+v", transition.Projection)
	}
}

func TestRunOnceProcessesOnlineProjectionWhenOperationAlreadyCompleted(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	inboxStore := &fakeStore{
		created: true,
		operation: model.DeviceOperation{
			OperationID:   "op-1",
			DeviceID:      "11111111-1111-1111-1111-111111111111",
			OperationType: model.DeviceOperationTypeProvision,
			Status:        model.DeviceOperationStatusSucceeded,
		},
	}
	message := eventMessage(t, "msg-online-replay", channel.MessageTypeDeviceOnlineChanged, now, channel.DeviceOnlineChangedPayload{
		OrgID:           "00000000-0000-0000-0000-000000000001",
		AccountDeviceID: "11111111-1111-1111-1111-111111111111",
		VideoCloudDevid: "video-1",
		Status:          "online",
		LastSeenAt:      now,
	})
	message.Envelope.OperationID = "op-1"

	service := NewService(inboxStore, fakeConsumer{
		messages: []broker.Message{message},
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
	if transition.OperationStatus != nil {
		t.Fatalf("expected online projection to skip operation updates, got %+v", transition.OperationStatus)
	}
	if transition.Projection == nil {
		t.Fatal("expected online projection to be applied")
	}
}

func TestRunOnceSkipsCompletedLifecycleReplayForRetryingMessage(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	inboxStore := &fakeStore{
		created: false,
		message: model.DeviceMessageInbox{
			MessageID:    "msg-1",
			OperationID:  "op-1",
			PartitionKey: "11111111-1111-1111-1111-111111111111",
			Status:       model.DeviceMessageInboxStatusRetrying,
			AttemptCount: 1,
		},
		operation: model.DeviceOperation{
			OperationID:   "op-1",
			DeviceID:      "11111111-1111-1111-1111-111111111111",
			OperationType: model.DeviceOperationTypeProvision,
			Status:        model.DeviceOperationStatusSucceeded,
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
	if stats.Processed != 1 || stats.DeadLettered != 0 || stats.Retrying != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	if len(inboxStore.transitions) != 1 {
		t.Fatalf("expected one transition, got %d", len(inboxStore.transitions))
	}
	transition := inboxStore.transitions[0]
	if transition.MessageStatus != model.DeviceMessageInboxStatusProcessed {
		t.Fatalf("expected retrying replay inbox row to be marked processed, got %s", transition.MessageStatus)
	}
	if transition.OperationStatus != nil {
		t.Fatalf("expected completed operation replay to skip operation update, got %+v", transition.OperationStatus)
	}
	if transition.Projection != nil {
		t.Fatalf("expected completed operation replay to skip device projection, got %+v", transition.Projection)
	}
}

func TestRunOnceDeadLettersInvalidMessages(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	message := validProvisionSucceededMessage(now)
	message.Stream = "wrong.stream"
	inboxStore := &fakeStore{
		created: true,
		operation: model.DeviceOperation{
			OperationID: "op-1",
			DeviceID:    "11111111-1111-1111-1111-111111111111",
		},
	}
	acked := 0
	service := NewService(inboxStore, fakeConsumer{
		messages: []broker.Message{message.WithAck(func(context.Context) error {
			acked++
			return nil
		})},
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
	if acked != 1 {
		t.Fatalf("expected dead-lettered message to be acknowledged once, got %d", acked)
	}
}

func TestRunOnceDeadLettersLifecycleMessagesWithMismatchedPartitionKeys(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	inboxStore := &fakeStore{
		created: true,
		operation: model.DeviceOperation{
			OperationID: "op-1",
			DeviceID:    "device-other",
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
	if stats.DeadLettered != 1 || stats.Processed != 0 || stats.Retrying != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	if len(inboxStore.transitions) != 1 {
		t.Fatalf("expected one transition, got %d", len(inboxStore.transitions))
	}
	if got := inboxStore.transitions[0].MessageStatus; got != model.DeviceMessageInboxStatusDeadLettered {
		t.Fatalf("expected dead-lettered status, got %s", got)
	}
	if inboxStore.transitions[0].LastError == nil || *inboxStore.transitions[0].LastError == "" {
		t.Fatalf("expected dead-letter to record a partition-key error, got %+v", inboxStore.transitions[0].LastError)
	}
}

func TestRunOnceRetriesTransientProjectionFailures(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	inboxStore := &fakeStore{
		created: true,
		operation: model.DeviceOperation{
			OperationID: "op-1",
			DeviceID:    "11111111-1111-1111-1111-111111111111",
		},
	}
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

func TestRunOnceProcessesFailureAndProjectionEvents(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		message   broker.Message
		assertion func(t *testing.T, transition store.InboxProcessTransitionInput)
	}{
		{
			name: "provision failure marks operation failed",
			message: eventMessage(t, "msg-provision-failed", channel.MessageTypeDeviceProvisionFailed, now, channel.DeviceProvisionFailedPayload{
				OrgID:           "00000000-0000-0000-0000-000000000001",
				AccountDeviceID: "11111111-1111-1111-1111-111111111111",
				VideoCloudDevid: "video-1",
				ActivityID:      "activity-1",
				ErrorCode:       "video_timeout",
				ErrorMessage:    "projection timed out",
				Retryable:       true,
				FailedAt:        now,
			}),
			assertion: func(t *testing.T, transition store.InboxProcessTransitionInput) {
				t.Helper()
				if transition.OperationStatus == nil || *transition.OperationStatus != model.DeviceOperationStatusFailed {
					t.Fatalf("expected failed operation status, got %+v", transition.OperationStatus)
				}
				if transition.OperationRetryable == nil || !*transition.OperationRetryable {
					t.Fatalf("expected retryable failure, got %+v", transition.OperationRetryable)
				}
				if transition.OperationErrorCode == nil || *transition.OperationErrorCode != "video_timeout" {
					t.Fatalf("expected failure code to be recorded, got %+v", transition.OperationErrorCode)
				}
				if transition.Projection == nil || transition.Projection.Metadata[model.DeviceMetadataVideoCloudLastError] == nil {
					t.Fatalf("expected failure projection metadata, got %+v", transition.Projection)
				}
			},
		},
		{
			name: "deactivate success keeps deactivation projection",
			message: eventMessage(t, "msg-deactivate-succeeded", channel.MessageTypeDeviceDeactivateSucceeded, now, channel.DeviceDeactivateSucceededPayload{
				OrgID:           "00000000-0000-0000-0000-000000000001",
				AccountDeviceID: "11111111-1111-1111-1111-111111111111",
				VideoCloudDevid: "video-1",
				DeactivatedAt:   now,
			}),
			assertion: func(t *testing.T, transition store.InboxProcessTransitionInput) {
				t.Helper()
				if transition.OperationStatus == nil || *transition.OperationStatus != model.DeviceOperationStatusSucceeded {
					t.Fatalf("expected succeeded operation status, got %+v", transition.OperationStatus)
				}
				if transition.Projection == nil || transition.Projection.Metadata[model.DeviceMetadataVideoCloudActivationStatus] != model.VideoCloudActivationStatusDeactivated {
					t.Fatalf("expected deactivated metadata projection, got %+v", transition.Projection)
				}
			},
		},
		{
			name: "deactivate failure records retryable error",
			message: eventMessage(t, "msg-deactivate-failed", channel.MessageTypeDeviceDeactivateFailed, now, channel.DeviceDeactivateFailedPayload{
				OrgID:           "00000000-0000-0000-0000-000000000001",
				AccountDeviceID: "11111111-1111-1111-1111-111111111111",
				VideoCloudDevid: "video-1",
				ErrorCode:       "upstream_500",
				ErrorMessage:    "server error",
				Retryable:       false,
				FailedAt:        now,
			}),
			assertion: func(t *testing.T, transition store.InboxProcessTransitionInput) {
				t.Helper()
				if transition.OperationStatus == nil || *transition.OperationStatus != model.DeviceOperationStatusFailed {
					t.Fatalf("expected failed operation status, got %+v", transition.OperationStatus)
				}
				if transition.OperationRetryable == nil || *transition.OperationRetryable {
					t.Fatalf("expected non-retryable failure, got %+v", transition.OperationRetryable)
				}
				if transition.OperationErrorMessage == nil || *transition.OperationErrorMessage != "server error" {
					t.Fatalf("expected failure message to be recorded, got %+v", transition.OperationErrorMessage)
				}
			},
		},
		{
			name: "unprovision success updates operation without device projection",
			message: eventMessage(t, "msg-unprovision-succeeded", channel.MessageTypeDeviceUnprovisionSucceeded, now, channel.DeviceUnprovisionSucceededPayload{
				OrgID:           "00000000-0000-0000-0000-000000000001",
				AccountDeviceID: "11111111-1111-1111-1111-111111111111",
				VideoCloudDevid: "video-1",
				UnprovisionedAt: now,
			}),
			assertion: func(t *testing.T, transition store.InboxProcessTransitionInput) {
				t.Helper()
				if transition.OperationStatus == nil || *transition.OperationStatus != model.DeviceOperationStatusSucceeded {
					t.Fatalf("expected succeeded operation status, got %+v", transition.OperationStatus)
				}
				if transition.Projection != nil {
					t.Fatalf("expected no device projection for deleted account binding, got %+v", transition.Projection)
				}
				if transition.OperationResult["video_cloud_devid"] != "video-1" {
					t.Fatalf("expected video devid in operation result, got %+v", transition.OperationResult)
				}
			},
		},
		{
			name: "unprovision failure updates operation without device projection",
			message: eventMessage(t, "msg-unprovision-failed", channel.MessageTypeDeviceUnprovisionFailed, now, channel.DeviceUnprovisionFailedPayload{
				OrgID:           "00000000-0000-0000-0000-000000000001",
				AccountDeviceID: "11111111-1111-1111-1111-111111111111",
				VideoCloudDevid: "video-1",
				ErrorCode:       "unprovision_failed",
				ErrorMessage:    "server error",
				Retryable:       true,
				FailedAt:        now,
			}),
			assertion: func(t *testing.T, transition store.InboxProcessTransitionInput) {
				t.Helper()
				if transition.OperationStatus == nil || *transition.OperationStatus != model.DeviceOperationStatusFailed {
					t.Fatalf("expected failed operation status, got %+v", transition.OperationStatus)
				}
				if transition.Projection != nil {
					t.Fatalf("expected no device projection for deleted account binding, got %+v", transition.Projection)
				}
				if transition.OperationRetryable == nil || !*transition.OperationRetryable {
					t.Fatalf("expected retryable failure, got %+v", transition.OperationRetryable)
				}
			},
		},
		{
			name: "online changed updates device status projection only",
			message: eventMessage(t, "msg-online", channel.MessageTypeDeviceOnlineChanged, now, channel.DeviceOnlineChangedPayload{
				OrgID:           "00000000-0000-0000-0000-000000000001",
				AccountDeviceID: "11111111-1111-1111-1111-111111111111",
				VideoCloudDevid: "video-1",
				Status:          channel.OnlineStatusOffline,
				LastSeenAt:      now,
			}),
			assertion: func(t *testing.T, transition store.InboxProcessTransitionInput) {
				t.Helper()
				if transition.OperationStatus != nil {
					t.Fatalf("expected no operation update, got %+v", transition.OperationStatus)
				}
				if transition.Projection == nil || transition.Projection.Status == nil || *transition.Projection.Status != model.DeviceStatusOffline {
					t.Fatalf("expected offline status projection, got %+v", transition.Projection)
				}
			},
		},
		{
			name: "metadata changed filters non video-cloud keys",
			message: eventMessage(t, "msg-metadata", channel.MessageTypeDeviceMetadataChanged, now, channel.DeviceMetadataChangedPayload{
				OrgID:           "00000000-0000-0000-0000-000000000001",
				AccountDeviceID: "11111111-1111-1111-1111-111111111111",
				VideoCloudDevid: "video-1",
				Metadata: map[string]any{
					model.DeviceMetadataVideoCloudActivityID: "activity-2",
					"location":                               "ignore-me",
				},
			}),
			assertion: func(t *testing.T, transition store.InboxProcessTransitionInput) {
				t.Helper()
				if transition.OperationStatus != nil {
					t.Fatalf("expected no operation update, got %+v", transition.OperationStatus)
				}
				if transition.Projection == nil {
					t.Fatal("expected metadata projection")
				}
				if got := transition.Projection.Metadata[model.DeviceMetadataVideoCloudActivityID]; got != "activity-2" {
					t.Fatalf("expected activity metadata patch, got %+v", got)
				}
				if _, ok := transition.Projection.Metadata["location"]; ok {
					t.Fatalf("expected non video-cloud metadata to be filtered, got %+v", transition.Projection.Metadata)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inboxStore := &fakeStore{
				created: true,
				operation: model.DeviceOperation{
					OperationID: "op-1",
					DeviceID:    "11111111-1111-1111-1111-111111111111",
				},
			}
			service := NewService(inboxStore, fakeConsumer{messages: []broker.Message{tc.message}}, Options{
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
			tc.assertion(t, transition)
		})
	}
}

func TestRunOnceDeadLettersMalformedAndUnmappedMessages(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		message   broker.Message
		assertion func(t *testing.T, inboxStore *fakeStore)
	}{
		{
			name: "malformed payload keeps inspectable inbox row",
			message: broker.Message{
				Stream: channel.StreamVideoAccountEvents,
				Envelope: channel.Envelope{
					MessageID:     "msg-malformed",
					CorrelationID: "corr-malformed",
					OperationID:   "op-malformed",
					SourceService: channel.ServiceRealtekVideoCloud,
					TargetService: channel.ServiceAccountManager,
					MessageType:   channel.MessageTypeDeviceProvisionSucceeded,
					SchemaVersion: channel.SchemaVersionV1,
					PartitionKey:  "11111111-1111-1111-1111-111111111111",
					OccurredAt:    now,
					Payload:       []byte("{not-json"),
				},
			},
			assertion: func(t *testing.T, inboxStore *fakeStore) {
				t.Helper()
				got := inboxStore.createInputs[0].Payload
				if got[payloadSnapshotRawKey] != "{not-json" {
					t.Fatalf("expected raw malformed payload to be preserved, got %+v", got)
				}
				if got[payloadSnapshotBase64Key] != base64.StdEncoding.EncodeToString([]byte("{not-json")) {
					t.Fatalf("expected raw malformed payload bytes to be preserved, got %+v", got)
				}
				if got[payloadSnapshotErrorKey] == nil {
					t.Fatalf("expected payload decode error metadata, got %+v", got)
				}
			},
		},
		{
			name: "malformed payload with invalid utf-8 keeps exact bytes",
			message: broker.Message{
				Stream: channel.StreamVideoAccountEvents,
				Envelope: channel.Envelope{
					MessageID:     "msg-invalid-utf8",
					CorrelationID: "corr-invalid-utf8",
					OperationID:   "op-invalid-utf8",
					SourceService: channel.ServiceRealtekVideoCloud,
					TargetService: channel.ServiceAccountManager,
					MessageType:   channel.MessageTypeDeviceProvisionSucceeded,
					SchemaVersion: channel.SchemaVersionV1,
					PartitionKey:  "11111111-1111-1111-1111-111111111111",
					OccurredAt:    now,
					Payload:       []byte{0xff, 0xfe, '{', 'b', 'a', 'd', '}'},
				},
			},
			assertion: func(t *testing.T, inboxStore *fakeStore) {
				t.Helper()
				got := inboxStore.createInputs[0].Payload
				if _, ok := got[payloadSnapshotRawKey]; ok {
					t.Fatalf("expected lossy raw payload string to be omitted for invalid utf-8, got %+v", got)
				}
				if got[payloadSnapshotBase64Key] != base64.StdEncoding.EncodeToString([]byte{0xff, 0xfe, '{', 'b', 'a', 'd', '}'}) {
					t.Fatalf("expected exact malformed payload bytes to be preserved, got %+v", got)
				}
				if got[payloadSnapshotErrorKey] == nil {
					t.Fatalf("expected payload decode error metadata, got %+v", got)
				}
			},
		},
		{
			name: "command-only message on events stream dead-letters",
			message: eventMessage(t, "msg-unmapped", channel.MessageTypeDeviceDeactivateRequested, now, channel.DeviceDeactivateRequestedPayload{
				OrgID:           "00000000-0000-0000-0000-000000000001",
				AccountDeviceID: "11111111-1111-1111-1111-111111111111",
				VideoCloudDevid: "video-1",
				RequestedBy:     "user-1",
				Reason:          "cleanup",
			}),
			assertion: func(t *testing.T, inboxStore *fakeStore) {
				t.Helper()
				if got := inboxStore.createInputs[0].MessageType; got != string(channel.MessageTypeDeviceDeactivateRequested) {
					t.Fatalf("expected stored unmapped message type, got %s", got)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inboxStore := &fakeStore{created: true}
			service := NewService(inboxStore, fakeConsumer{messages: []broker.Message{tc.message}}, Options{
				Now: func() time.Time { return now },
			})

			stats, err := service.RunOnce(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if stats.DeadLettered != 1 || stats.Processed != 0 {
				t.Fatalf("unexpected stats: %+v", stats)
			}
			if len(inboxStore.transitions) != 1 {
				t.Fatalf("expected one transition, got %d", len(inboxStore.transitions))
			}
			if got := inboxStore.transitions[0].MessageStatus; got != model.DeviceMessageInboxStatusDeadLettered {
				t.Fatalf("expected dead-lettered status, got %s", got)
			}
			tc.assertion(t, inboxStore)
		})
	}
}

func TestRunOnceDeadLettersInvalidPartitionKeysAfterPersistingInboxRow(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	deviceID := "11111111-1111-1111-1111-111111111111"
	tests := []struct {
		name              string
		message           broker.Message
		store             *fakeStore
		wantPartitionKey  string
		wantSnapshotValue any
		wantErrorFragment string
	}{
		{
			name: "nonblank mismatched partition key dead-letters",
			message: eventMessage(t, "msg-mismatch", channel.MessageTypeDeviceMetadataChanged, now, channel.DeviceMetadataChangedPayload{
				OrgID:           "00000000-0000-0000-0000-000000000001",
				AccountDeviceID: deviceID,
				VideoCloudDevid: "video-1",
				Metadata:        map[string]any{},
			}),
			store:             &fakeStore{created: true},
			wantPartitionKey:  "22222222-2222-2222-2222-222222222222",
			wantSnapshotValue: nil,
			wantErrorFragment: "partition_key must equal payload.account_device_id",
		},
		{
			name: "blank partition key is normalized for storage and dead-lettered",
			message: eventMessage(t, "msg-blank", channel.MessageTypeDeviceMetadataChanged, now, channel.DeviceMetadataChangedPayload{
				OrgID:           "00000000-0000-0000-0000-000000000001",
				AccountDeviceID: deviceID,
				VideoCloudDevid: "video-1",
				Metadata:        map[string]any{},
			}),
			store: &fakeStore{
				created: true,
				operation: model.DeviceOperation{
					OperationID: "op-msg-blank",
					DeviceID:    deviceID,
				},
			},
			wantPartitionKey:  deviceID,
			wantSnapshotValue: "   ",
			wantErrorFragment: "partition_key must be non-empty",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.wantSnapshotValue == nil {
				tc.message.Envelope.PartitionKey = tc.wantPartitionKey
			} else {
				tc.message.Envelope.PartitionKey = tc.wantSnapshotValue.(string)
			}

			service := NewService(tc.store, fakeConsumer{messages: []broker.Message{tc.message}}, Options{
				Now: func() time.Time { return now },
			})

			stats, err := service.RunOnce(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if stats.DeadLettered != 1 || stats.Processed != 0 || stats.Retrying != 0 {
				t.Fatalf("unexpected stats: %+v", stats)
			}
			if len(tc.store.createInputs) != 1 {
				t.Fatalf("expected one create input, got %d", len(tc.store.createInputs))
			}
			if got := tc.store.createInputs[0].PartitionKey; got != tc.wantPartitionKey {
				t.Fatalf("expected persisted partition key %q, got %q", tc.wantPartitionKey, got)
			}
			if got := tc.store.createInputs[0].Payload[payloadSnapshotEnvelopePartitionKey]; got != tc.wantSnapshotValue {
				t.Fatalf("expected partition-key snapshot %+v, got %+v", tc.wantSnapshotValue, got)
			}
			if len(tc.store.transitions) != 1 {
				t.Fatalf("expected one transition, got %d", len(tc.store.transitions))
			}
			if got := tc.store.transitions[0].MessageStatus; got != model.DeviceMessageInboxStatusDeadLettered {
				t.Fatalf("expected dead-lettered status, got %s", got)
			}
			if tc.store.transitions[0].LastError == nil || !strings.Contains(*tc.store.transitions[0].LastError, tc.wantErrorFragment) {
				t.Fatalf("expected dead-letter error containing %q, got %+v", tc.wantErrorFragment, tc.store.transitions[0].LastError)
			}
		})
	}
}

func TestRunOnceDeadLettersTransientProjectionFailureAtAttemptLimit(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	inboxStore := &fakeStore{
		created: false,
		message: model.DeviceMessageInbox{
			MessageID:    "msg-1",
			Status:       model.DeviceMessageInboxStatusRetrying,
			AttemptCount: 2,
		},
	}
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
	if stats.DeadLettered != 1 || stats.Retrying != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	if got := inboxStore.transitions[0].MessageStatus; got != model.DeviceMessageInboxStatusDeadLettered {
		t.Fatalf("expected dead-lettered status, got %s", got)
	}
}

func TestRunOnceUsesWorkerClockWhenEnvelopeTimeIsMissing(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 123456789, time.UTC)
	inboxStore := &fakeStore{created: true}
	message := validProvisionSucceededMessage(now)
	message.Envelope.OccurredAt = time.Time{}
	message.Envelope.CausationID = "cause-1"

	service := NewService(inboxStore, fakeConsumer{
		messages: []broker.Message{message},
	}, Options{
		Now: func() time.Time { return now },
	})

	if _, err := service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(inboxStore.createInputs) != 1 {
		t.Fatalf("expected one create input, got %d", len(inboxStore.createInputs))
	}
	if got := inboxStore.createInputs[0].ReceivedAt; !got.Equal(now.Truncate(time.Microsecond)) {
		t.Fatalf("expected worker clock fallback, got %s", got)
	}
	if inboxStore.createInputs[0].CausationID == nil || *inboxStore.createInputs[0].CausationID != "cause-1" {
		t.Fatalf("expected causation id to be preserved, got %+v", inboxStore.createInputs[0].CausationID)
	}
}

func TestRunRetriesTransientReceiveErrors(t *testing.T) {
	consumer := &scriptedConsumer{
		results: []scriptedReceiveResult{
			{err: broker.Transient(errors.New("temporary event hubs receive failure"))},
			{err: context.Canceled},
		},
	}

	service := NewService(&fakeStore{}, consumer, Options{
		PollInterval: time.Millisecond,
	})

	if err := service.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if consumer.calls != 2 {
		t.Fatalf("expected service to retry after transient receive failure, got %d calls", consumer.calls)
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
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
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

func eventMessage(tb interface {
	Helper()
	Fatal(...any)
}, messageID string, messageType channel.MessageType, now time.Time, payload any) broker.Message {
	tb.Helper()
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		tb.Fatal(err)
	}

	return broker.Message{
		Stream: channel.StreamVideoAccountEvents,
		Envelope: channel.Envelope{
			MessageID:     messageID,
			CorrelationID: "corr-" + messageID,
			OperationID:   "op-" + messageID,
			SourceService: channel.ServiceRealtekVideoCloud,
			TargetService: channel.ServiceAccountManager,
			MessageType:   messageType,
			SchemaVersion: channel.SchemaVersionV1,
			PartitionKey:  payloadPartitionKey(payload),
			OccurredAt:    now,
			Payload:       rawPayload,
		},
	}
}

func payloadPartitionKey(payload any) string {
	switch typed := payload.(type) {
	case channel.DeviceProvisionSucceededPayload:
		return typed.AccountDeviceID
	case channel.DeviceProvisionFailedPayload:
		return typed.AccountDeviceID
	case channel.DeviceDeactivateRequestedPayload:
		return typed.AccountDeviceID
	case channel.DeviceDeactivateSucceededPayload:
		return typed.AccountDeviceID
	case channel.DeviceDeactivateFailedPayload:
		return typed.AccountDeviceID
	case channel.DeviceUnprovisionSucceededPayload:
		return typed.AccountDeviceID
	case channel.DeviceUnprovisionFailedPayload:
		return typed.AccountDeviceID
	case channel.DeviceOnlineChangedPayload:
		return typed.AccountDeviceID
	case channel.DeviceMetadataChangedPayload:
		return typed.AccountDeviceID
	default:
		return ""
	}
}
