package channel

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

var testTime = time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)

func TestValidateAndDecodeAcceptsEachMessageType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		envelope    Envelope
		stream      string
		wantPayload any
	}{
		{
			name: "DeviceProvisionRequested",
			envelope: validEnvelope(MessageTypeDeviceProvisionRequested, DeviceProvisionRequestedPayload{
				OrgID:           "org-1",
				AccountDeviceID: "device-1",
				VideoCloudDevid: "video-1",
				ActivityID:      "activity-1",
				ClipPublicKey:   "clip-key",
				RequestedBy:     "user-1",
			}),
			stream:      StreamAccountVideoCommands,
			wantPayload: &DeviceProvisionRequestedPayload{},
		},
		{
			name: "DeviceProvisionSucceeded",
			envelope: validEnvelope(MessageTypeDeviceProvisionSucceeded, DeviceProvisionSucceededPayload{
				OrgID:           "org-1",
				AccountDeviceID: "device-1",
				VideoCloudDevid: "video-1",
				ActivityID:      "activity-1",
				ActivatedAt:     testTime,
			}),
			stream:      StreamVideoAccountEvents,
			wantPayload: &DeviceProvisionSucceededPayload{},
		},
		{
			name: "DeviceProvisionFailed",
			envelope: validEnvelope(MessageTypeDeviceProvisionFailed, DeviceProvisionFailedPayload{
				OrgID:           "org-1",
				AccountDeviceID: "device-1",
				VideoCloudDevid: "video-1",
				ActivityID:      "activity-1",
				ErrorCode:       "activation_failed",
				ErrorMessage:    "activation failed",
				Retryable:       true,
				FailedAt:        testTime,
			}),
			stream:      StreamVideoAccountEvents,
			wantPayload: &DeviceProvisionFailedPayload{},
		},
		{
			name: "DeviceDeactivateRequested",
			envelope: validEnvelope(MessageTypeDeviceDeactivateRequested, DeviceDeactivateRequestedPayload{
				OrgID:           "org-1",
				AccountDeviceID: "device-1",
				VideoCloudDevid: "video-1",
				RequestedBy:     "user-1",
				Reason:          "account_device_disabled",
			}),
			stream:      StreamAccountVideoCommands,
			wantPayload: &DeviceDeactivateRequestedPayload{},
		},
		{
			name: "DeviceDeactivateSucceeded",
			envelope: validEnvelope(MessageTypeDeviceDeactivateSucceeded, DeviceDeactivateSucceededPayload{
				OrgID:           "org-1",
				AccountDeviceID: "device-1",
				VideoCloudDevid: "video-1",
				DeactivatedAt:   testTime,
			}),
			stream:      StreamVideoAccountEvents,
			wantPayload: &DeviceDeactivateSucceededPayload{},
		},
		{
			name: "DeviceDeactivateFailed",
			envelope: validEnvelope(MessageTypeDeviceDeactivateFailed, DeviceDeactivateFailedPayload{
				OrgID:           "org-1",
				AccountDeviceID: "device-1",
				VideoCloudDevid: "video-1",
				ErrorCode:       "deactivation_failed",
				ErrorMessage:    "deactivation failed",
				Retryable:       true,
				FailedAt:        testTime,
			}),
			stream:      StreamVideoAccountEvents,
			wantPayload: &DeviceDeactivateFailedPayload{},
		},
		{
			name: "DeviceOnlineChanged",
			envelope: validEnvelope(MessageTypeDeviceOnlineChanged, DeviceOnlineChangedPayload{
				OrgID:           "org-1",
				AccountDeviceID: "device-1",
				VideoCloudDevid: "video-1",
				Status:          OnlineStatusOnline,
				LastSeenAt:      testTime,
			}),
			stream:      StreamVideoAccountEvents,
			wantPayload: &DeviceOnlineChangedPayload{},
		},
		{
			name: "DeviceMetadataChanged",
			envelope: validEnvelope(MessageTypeDeviceMetadataChanged, DeviceMetadataChangedPayload{
				OrgID:           "org-1",
				AccountDeviceID: "device-1",
				VideoCloudDevid: "video-1",
				Metadata: map[string]any{
					"video_cloud_activation_status": "activated",
				},
			}),
			stream:      StreamVideoAccountEvents,
			wantPayload: &DeviceMetadataChangedPayload{},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload, err := tt.envelope.ValidateAndDecode(tt.stream)
			if err != nil {
				t.Fatalf("ValidateAndDecode() error = %v", err)
			}
			if payload.PartitionKey() != "device-1" {
				t.Fatalf("expected account device id device-1, got %q", payload.PartitionKey())
			}
			if reflect.TypeOf(payload) != reflect.TypeOf(tt.wantPayload) {
				t.Fatalf("expected payload type %T, got %T", tt.wantPayload, payload)
			}
		})
	}
}

func TestValidateRejectsInvalidMessagesForEachType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		envelope    Envelope
		stream      string
		wantMessage string
	}{
		{
			name: "DeviceProvisionRequested",
			envelope: validEnvelope(MessageTypeDeviceProvisionRequested, DeviceProvisionRequestedPayload{
				OrgID:           "org-1",
				AccountDeviceID: "device-1",
				VideoCloudDevid: "video-1",
				ActivityID:      "",
				ClipPublicKey:   "clip-key",
				RequestedBy:     "user-1",
			}),
			stream:      StreamAccountVideoCommands,
			wantMessage: "payload.activity_id must be non-empty",
		},
		{
			name: "DeviceProvisionSucceeded",
			envelope: validEnvelope(MessageTypeDeviceProvisionSucceeded, DeviceProvisionSucceededPayload{
				OrgID:           "org-1",
				AccountDeviceID: "device-1",
				VideoCloudDevid: "video-1",
				ActivityID:      "activity-1",
			}),
			stream:      StreamVideoAccountEvents,
			wantMessage: "payload.activated_at must be set",
		},
		{
			name: "DeviceProvisionFailed",
			envelope: validEnvelope(MessageTypeDeviceProvisionFailed, DeviceProvisionFailedPayload{
				OrgID:           "org-1",
				AccountDeviceID: "device-1",
				VideoCloudDevid: "video-1",
				ActivityID:      "activity-1",
				ErrorCode:       "",
				ErrorMessage:    "activation failed",
				Retryable:       true,
				FailedAt:        testTime,
			}),
			stream:      StreamVideoAccountEvents,
			wantMessage: "payload.error_code must be non-empty",
		},
		{
			name: "DeviceDeactivateRequested",
			envelope: validEnvelope(MessageTypeDeviceDeactivateRequested, DeviceDeactivateRequestedPayload{
				OrgID:           "org-1",
				AccountDeviceID: "device-1",
				VideoCloudDevid: "video-1",
				RequestedBy:     "user-1",
				Reason:          "",
			}),
			stream:      StreamAccountVideoCommands,
			wantMessage: "payload.reason must be non-empty",
		},
		{
			name: "DeviceDeactivateSucceeded",
			envelope: validEnvelope(MessageTypeDeviceDeactivateSucceeded, DeviceDeactivateSucceededPayload{
				OrgID:           "org-1",
				AccountDeviceID: "device-1",
				VideoCloudDevid: "video-1",
			}),
			stream:      StreamVideoAccountEvents,
			wantMessage: "payload.deactivated_at must be set",
		},
		{
			name: "DeviceDeactivateFailed",
			envelope: validEnvelope(MessageTypeDeviceDeactivateFailed, DeviceDeactivateFailedPayload{
				OrgID:           "org-1",
				AccountDeviceID: "device-1",
				VideoCloudDevid: "video-1",
				ErrorCode:       "deactivation_failed",
				ErrorMessage:    "",
				Retryable:       true,
				FailedAt:        testTime,
			}),
			stream:      StreamVideoAccountEvents,
			wantMessage: "payload.error_message must be non-empty",
		},
		{
			name: "DeviceOnlineChanged",
			envelope: validEnvelope(MessageTypeDeviceOnlineChanged, DeviceOnlineChangedPayload{
				OrgID:           "org-1",
				AccountDeviceID: "device-1",
				VideoCloudDevid: "video-1",
				Status:          "busy",
				LastSeenAt:      testTime,
			}),
			stream:      StreamVideoAccountEvents,
			wantMessage: "payload.status unsupported value",
		},
		{
			name: "DeviceMetadataChanged",
			envelope: validEnvelope(MessageTypeDeviceMetadataChanged, DeviceMetadataChangedPayload{
				OrgID:           "org-1",
				AccountDeviceID: "device-1",
				VideoCloudDevid: "video-1",
			}),
			stream:      StreamVideoAccountEvents,
			wantMessage: "payload.metadata must be set",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.envelope.Validate(tt.stream)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), tt.wantMessage) {
				t.Fatalf("expected error containing %q, got %q", tt.wantMessage, err.Error())
			}
		})
	}
}

func TestValidateRejectsEnvelopeContractMismatches(t *testing.T) {
	t.Parallel()

	t.Run("unknown message type", func(t *testing.T) {
		t.Parallel()

		envelope := validEnvelope(MessageTypeDeviceProvisionRequested, DeviceProvisionRequestedPayload{
			OrgID:           "org-1",
			AccountDeviceID: "device-1",
			VideoCloudDevid: "video-1",
			ActivityID:      "activity-1",
			ClipPublicKey:   "clip-key",
			RequestedBy:     "user-1",
		})
		envelope.MessageType = MessageType("DeviceMysteryChanged")

		err := envelope.Validate(StreamAccountVideoCommands)
		if err == nil || !strings.Contains(err.Error(), `message_type unsupported value "DeviceMysteryChanged"`) {
			t.Fatalf("expected unknown message type error, got %v", err)
		}
	})

	t.Run("unsupported schema version", func(t *testing.T) {
		t.Parallel()

		envelope := validEnvelope(MessageTypeDeviceProvisionRequested, DeviceProvisionRequestedPayload{
			OrgID:           "org-1",
			AccountDeviceID: "device-1",
			VideoCloudDevid: "video-1",
			ActivityID:      "activity-1",
			ClipPublicKey:   "clip-key",
			RequestedBy:     "user-1",
		})
		envelope.SchemaVersion = "2.0"

		err := envelope.Validate(StreamAccountVideoCommands)
		if err == nil || !strings.Contains(err.Error(), `schema_version unsupported value "2.0"`) {
			t.Fatalf("expected unsupported schema version error, got %v", err)
		}
	})

	t.Run("stream mismatch", func(t *testing.T) {
		t.Parallel()

		envelope := validEnvelope(MessageTypeDeviceProvisionRequested, DeviceProvisionRequestedPayload{
			OrgID:           "org-1",
			AccountDeviceID: "device-1",
			VideoCloudDevid: "video-1",
			ActivityID:      "activity-1",
			ClipPublicKey:   "clip-key",
			RequestedBy:     "user-1",
		})

		err := envelope.Validate(StreamVideoAccountEvents)
		if err == nil || !strings.Contains(err.Error(), `stream message type "DeviceProvisionRequested" must use "account.video.commands"`) {
			t.Fatalf("expected stream mismatch error, got %v", err)
		}
	})

	t.Run("source service mismatch", func(t *testing.T) {
		t.Parallel()

		envelope := validEnvelope(MessageTypeDeviceOnlineChanged, DeviceOnlineChangedPayload{
			OrgID:           "org-1",
			AccountDeviceID: "device-1",
			VideoCloudDevid: "video-1",
			Status:          OnlineStatusOnline,
			LastSeenAt:      testTime,
		})
		envelope.SourceService = ServiceAccountManager

		err := envelope.Validate(StreamVideoAccountEvents)
		if err == nil || !strings.Contains(err.Error(), `source_service message type "DeviceOnlineChanged" must use "realtek_video_server"`) {
			t.Fatalf("expected source service mismatch error, got %v", err)
		}
	})

	t.Run("partition key mismatch", func(t *testing.T) {
		t.Parallel()

		envelope := validEnvelope(MessageTypeDeviceOnlineChanged, DeviceOnlineChangedPayload{
			OrgID:           "org-1",
			AccountDeviceID: "device-1",
			VideoCloudDevid: "video-1",
			Status:          OnlineStatusOnline,
			LastSeenAt:      testTime,
		})
		envelope.PartitionKey = "device-2"

		err := envelope.Validate(StreamVideoAccountEvents)
		if err == nil || !strings.Contains(err.Error(), "partition_key must equal payload.account_device_id") {
			t.Fatalf("expected partition key mismatch error, got %v", err)
		}
	})
}

func validEnvelope(messageType MessageType, payload any) Envelope {
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}

	spec := messageSpecs[messageType]
	return Envelope{
		MessageID:     "msg-1",
		CorrelationID: "corr-1",
		CausationID:   "msg-0",
		OperationID:   "op-1",
		SourceService: spec.sourceService,
		TargetService: spec.targetService,
		MessageType:   messageType,
		SchemaVersion: SchemaVersionV1,
		PartitionKey:  "device-1",
		OccurredAt:    testTime,
		Payload:       raw,
	}
}
