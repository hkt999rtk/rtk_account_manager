package channel

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

var testTime = time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)

const (
	testOrgID    = "11111111-1111-1111-1111-111111111111"
	testDeviceID = "22222222-2222-2222-2222-222222222222"
)

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
				OrgID:           testOrgID,
				AccountDeviceID: testDeviceID,
				VideoCloudDevid: "video-1",
				ActivityID:      "activity-1",
				ClipPublicKey:   "clip-key",
				ServiceOptions:  []string{"video_streaming", "video_storage"},
				RequestedBy:     "user-1",
			}),
			stream:      StreamAccountVideoCommands,
			wantPayload: &DeviceProvisionRequestedPayload{},
		},
		{
			name: "DeviceProvisionSucceeded",
			envelope: validEnvelope(MessageTypeDeviceProvisionSucceeded, DeviceProvisionSucceededPayload{
				OrgID:           testOrgID,
				AccountDeviceID: testDeviceID,
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
				OrgID:           testOrgID,
				AccountDeviceID: testDeviceID,
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
				OrgID:           testOrgID,
				AccountDeviceID: testDeviceID,
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
				OrgID:           testOrgID,
				AccountDeviceID: testDeviceID,
				VideoCloudDevid: "video-1",
				DeactivatedAt:   testTime,
			}),
			stream:      StreamVideoAccountEvents,
			wantPayload: &DeviceDeactivateSucceededPayload{},
		},
		{
			name: "DeviceDeactivateFailed",
			envelope: validEnvelope(MessageTypeDeviceDeactivateFailed, DeviceDeactivateFailedPayload{
				OrgID:           testOrgID,
				AccountDeviceID: testDeviceID,
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
				OrgID:           testOrgID,
				AccountDeviceID: testDeviceID,
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
				OrgID:           testOrgID,
				AccountDeviceID: testDeviceID,
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
			if payload.PartitionKey() != testDeviceID {
				t.Fatalf("expected account device id %s, got %q", testDeviceID, payload.PartitionKey())
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
				OrgID:           testOrgID,
				AccountDeviceID: testDeviceID,
				VideoCloudDevid: "video-1",
				ActivityID:      "",
				ClipPublicKey:   "clip-key",
				RequestedBy:     "user-1",
			}),
			stream:      StreamAccountVideoCommands,
			wantMessage: "payload.activity_id must be non-empty",
		},
		{
			name: "DeviceProvisionRequested service options",
			envelope: validEnvelope(MessageTypeDeviceProvisionRequested, DeviceProvisionRequestedPayload{
				OrgID:           testOrgID,
				AccountDeviceID: testDeviceID,
				VideoCloudDevid: "video-1",
				ActivityID:      "activity-1",
				ClipPublicKey:   "clip-key",
				ServiceOptions:  []string{"mqtt", "admin"},
				RequestedBy:     "user-1",
			}),
			stream:      StreamAccountVideoCommands,
			wantMessage: "payload.service_options may contain only mqtt, video_streaming, or video_storage",
		},
		{
			name: "DeviceProvisionSucceeded",
			envelope: validEnvelope(MessageTypeDeviceProvisionSucceeded, DeviceProvisionSucceededPayload{
				OrgID:           testOrgID,
				AccountDeviceID: testDeviceID,
				VideoCloudDevid: "video-1",
				ActivityID:      "activity-1",
			}),
			stream:      StreamVideoAccountEvents,
			wantMessage: "payload.activated_at must be set",
		},
		{
			name: "DeviceProvisionFailed",
			envelope: validEnvelope(MessageTypeDeviceProvisionFailed, DeviceProvisionFailedPayload{
				OrgID:           testOrgID,
				AccountDeviceID: testDeviceID,
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
				OrgID:           testOrgID,
				AccountDeviceID: testDeviceID,
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
				OrgID:           testOrgID,
				AccountDeviceID: testDeviceID,
				VideoCloudDevid: "video-1",
			}),
			stream:      StreamVideoAccountEvents,
			wantMessage: "payload.deactivated_at must be set",
		},
		{
			name: "DeviceDeactivateFailed",
			envelope: validEnvelope(MessageTypeDeviceDeactivateFailed, DeviceDeactivateFailedPayload{
				OrgID:           testOrgID,
				AccountDeviceID: testDeviceID,
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
				OrgID:           testOrgID,
				AccountDeviceID: testDeviceID,
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
				OrgID:           testOrgID,
				AccountDeviceID: testDeviceID,
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

func TestValidateAcceptsExplicitFalseRetryable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		envelope Envelope
		stream   string
	}{
		{
			name: "provision failed",
			envelope: validEnvelope(MessageTypeDeviceProvisionFailed, DeviceProvisionFailedPayload{
				OrgID:           testOrgID,
				AccountDeviceID: testDeviceID,
				VideoCloudDevid: "video-1",
				ActivityID:      "activity-1",
				ErrorCode:       "activation_failed",
				ErrorMessage:    "activation failed",
				Retryable:       false,
				FailedAt:        testTime,
			}),
			stream: StreamVideoAccountEvents,
		},
		{
			name: "deactivate failed",
			envelope: validEnvelope(MessageTypeDeviceDeactivateFailed, DeviceDeactivateFailedPayload{
				OrgID:           testOrgID,
				AccountDeviceID: testDeviceID,
				VideoCloudDevid: "video-1",
				ErrorCode:       "deactivation_failed",
				ErrorMessage:    "deactivation failed",
				Retryable:       false,
				FailedAt:        testTime,
			}),
			stream: StreamVideoAccountEvents,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if err := tt.envelope.Validate(tt.stream); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestValidateRejectsLifecycleIDsThatAreNotUUIDs(t *testing.T) {
	t.Parallel()

	t.Run("org id", func(t *testing.T) {
		t.Parallel()

		envelope := validEnvelope(MessageTypeDeviceProvisionRequested, DeviceProvisionRequestedPayload{
			OrgID:           "org-1",
			AccountDeviceID: testDeviceID,
			VideoCloudDevid: "video-1",
			ActivityID:      "activity-1",
			ClipPublicKey:   "clip-key",
			RequestedBy:     "user-1",
		})

		err := envelope.Validate(StreamAccountVideoCommands)
		if err == nil || !strings.Contains(err.Error(), "payload.org_id must be a UUID") {
			t.Fatalf("expected invalid org id error, got %v", err)
		}
	})

	t.Run("account device id", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name     string
			envelope Envelope
			stream   string
		}{
			{
				name: "DeviceProvisionRequested",
				envelope: validEnvelope(MessageTypeDeviceProvisionRequested, DeviceProvisionRequestedPayload{
					OrgID:           testOrgID,
					AccountDeviceID: "device-1",
					VideoCloudDevid: "video-1",
					ActivityID:      "activity-1",
					ClipPublicKey:   "clip-key",
					RequestedBy:     "user-1",
				}),
				stream: StreamAccountVideoCommands,
			},
			{
				name: "DeviceProvisionSucceeded",
				envelope: validEnvelope(MessageTypeDeviceProvisionSucceeded, DeviceProvisionSucceededPayload{
					OrgID:           testOrgID,
					AccountDeviceID: "device-1",
					VideoCloudDevid: "video-1",
					ActivityID:      "activity-1",
					ActivatedAt:     testTime,
				}),
				stream: StreamVideoAccountEvents,
			},
			{
				name: "DeviceProvisionFailed",
				envelope: validEnvelope(MessageTypeDeviceProvisionFailed, DeviceProvisionFailedPayload{
					OrgID:           testOrgID,
					AccountDeviceID: "device-1",
					VideoCloudDevid: "video-1",
					ActivityID:      "activity-1",
					ErrorCode:       "activation_failed",
					ErrorMessage:    "activation failed",
					Retryable:       true,
					FailedAt:        testTime,
				}),
				stream: StreamVideoAccountEvents,
			},
			{
				name: "DeviceDeactivateRequested",
				envelope: validEnvelope(MessageTypeDeviceDeactivateRequested, DeviceDeactivateRequestedPayload{
					OrgID:           testOrgID,
					AccountDeviceID: "device-1",
					VideoCloudDevid: "video-1",
					RequestedBy:     "user-1",
					Reason:          "account_device_disabled",
				}),
				stream: StreamAccountVideoCommands,
			},
			{
				name: "DeviceDeactivateSucceeded",
				envelope: validEnvelope(MessageTypeDeviceDeactivateSucceeded, DeviceDeactivateSucceededPayload{
					OrgID:           testOrgID,
					AccountDeviceID: "device-1",
					VideoCloudDevid: "video-1",
					DeactivatedAt:   testTime,
				}),
				stream: StreamVideoAccountEvents,
			},
			{
				name: "DeviceDeactivateFailed",
				envelope: validEnvelope(MessageTypeDeviceDeactivateFailed, DeviceDeactivateFailedPayload{
					OrgID:           testOrgID,
					AccountDeviceID: "device-1",
					VideoCloudDevid: "video-1",
					ErrorCode:       "deactivation_failed",
					ErrorMessage:    "deactivation failed",
					Retryable:       true,
					FailedAt:        testTime,
				}),
				stream: StreamVideoAccountEvents,
			},
			{
				name: "DeviceOnlineChanged",
				envelope: validEnvelope(MessageTypeDeviceOnlineChanged, DeviceOnlineChangedPayload{
					OrgID:           testOrgID,
					AccountDeviceID: "device-1",
					VideoCloudDevid: "video-1",
					Status:          OnlineStatusOnline,
					LastSeenAt:      testTime,
				}),
				stream: StreamVideoAccountEvents,
			},
			{
				name: "DeviceMetadataChanged",
				envelope: validEnvelope(MessageTypeDeviceMetadataChanged, DeviceMetadataChangedPayload{
					OrgID:           testOrgID,
					AccountDeviceID: "device-1",
					VideoCloudDevid: "video-1",
					Metadata: map[string]any{
						"video_cloud_activation_status": "activated",
					},
				}),
				stream: StreamVideoAccountEvents,
			},
		}

		for _, tt := range tests {
			tt := tt
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				err := tt.envelope.Validate(tt.stream)
				if err == nil || !strings.Contains(err.Error(), "payload.account_device_id must be a UUID") {
					t.Fatalf("expected invalid account device id error, got %v", err)
				}
			})
		}
	})
}

func TestValidateRejectsEnvelopeContractMismatches(t *testing.T) {
	t.Parallel()

	t.Run("unknown message type", func(t *testing.T) {
		t.Parallel()

		envelope := validEnvelope(MessageTypeDeviceProvisionRequested, DeviceProvisionRequestedPayload{
			OrgID:           testOrgID,
			AccountDeviceID: testDeviceID,
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
			OrgID:           testOrgID,
			AccountDeviceID: testDeviceID,
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
			OrgID:           testOrgID,
			AccountDeviceID: testDeviceID,
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
			OrgID:           testOrgID,
			AccountDeviceID: testDeviceID,
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

	t.Run("target service mismatch", func(t *testing.T) {
		t.Parallel()

		envelope := validEnvelope(MessageTypeDeviceOnlineChanged, DeviceOnlineChangedPayload{
			OrgID:           testOrgID,
			AccountDeviceID: testDeviceID,
			VideoCloudDevid: "video-1",
			Status:          OnlineStatusOnline,
			LastSeenAt:      testTime,
		})
		envelope.TargetService = ServiceRealtekVideoCloud

		err := envelope.Validate(StreamVideoAccountEvents)
		if err == nil || !strings.Contains(err.Error(), `target_service message type "DeviceOnlineChanged" must use "rtk_account_manager"`) {
			t.Fatalf("expected target service mismatch error, got %v", err)
		}
	})

	t.Run("partition key mismatch", func(t *testing.T) {
		t.Parallel()

		envelope := validEnvelope(MessageTypeDeviceOnlineChanged, DeviceOnlineChangedPayload{
			OrgID:           testOrgID,
			AccountDeviceID: testDeviceID,
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

	t.Run("non-UTC occurred at", func(t *testing.T) {
		t.Parallel()

		envelope := validEnvelope(MessageTypeDeviceOnlineChanged, DeviceOnlineChangedPayload{
			OrgID:           testOrgID,
			AccountDeviceID: testDeviceID,
			VideoCloudDevid: "video-1",
			Status:          OnlineStatusOnline,
			LastSeenAt:      testTime,
		})
		envelope.OccurredAt = time.Date(2026, 4, 28, 20, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60))

		err := envelope.Validate(StreamVideoAccountEvents)
		if err == nil || !strings.Contains(err.Error(), "occurred_at must use UTC") {
			t.Fatalf("expected UTC timestamp error, got %v", err)
		}
	})

	t.Run("payload unknown field", func(t *testing.T) {
		t.Parallel()

		envelope := validEnvelope(MessageTypeDeviceProvisionRequested, DeviceProvisionRequestedPayload{
			OrgID:           testOrgID,
			AccountDeviceID: testDeviceID,
			VideoCloudDevid: "video-1",
			ActivityID:      "activity-1",
			ClipPublicKey:   "clip-key",
			RequestedBy:     "user-1",
		})
		envelope.Payload = json.RawMessage(`{
			"org_id":"11111111-1111-1111-1111-111111111111",
			"account_device_id":"22222222-2222-2222-2222-222222222222",
			"video_cloud_devid":"video-1",
			"activity_id":"activity-1",
			"clip_public_key":"clip-key",
			"requested_by":"user-1",
			"unexpected":"value"
		}`)

		err := envelope.Validate(StreamAccountVideoCommands)
		if err == nil || !strings.Contains(err.Error(), `payload: json: unknown field "unexpected"`) {
			t.Fatalf("expected unknown payload field error, got %v", err)
		}
	})

	t.Run("missing retryable field", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name        string
			messageType MessageType
			stream      string
			payload     string
			wantMessage string
		}{
			{
				name:        "provision failed",
				messageType: MessageTypeDeviceProvisionFailed,
				stream:      StreamVideoAccountEvents,
				payload: `{
					"org_id":"11111111-1111-1111-1111-111111111111",
					"account_device_id":"22222222-2222-2222-2222-222222222222",
					"video_cloud_devid":"video-1",
					"activity_id":"activity-1",
					"error_code":"activation_failed",
					"error_message":"activation failed",
					"failed_at":"2026-04-28T12:00:00Z"
				}`,
				wantMessage: "payload.retryable must be set",
			},
			{
				name:        "deactivate failed",
				messageType: MessageTypeDeviceDeactivateFailed,
				stream:      StreamVideoAccountEvents,
				payload: `{
					"org_id":"11111111-1111-1111-1111-111111111111",
					"account_device_id":"22222222-2222-2222-2222-222222222222",
					"video_cloud_devid":"video-1",
					"error_code":"deactivation_failed",
					"error_message":"deactivation failed",
					"failed_at":"2026-04-28T12:00:00Z"
				}`,
				wantMessage: "payload.retryable must be set",
			},
		}

		for _, tt := range tests {
			tt := tt
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				envelope := validEnvelope(tt.messageType, DeviceProvisionRequestedPayload{})
				envelope.Payload = json.RawMessage(tt.payload)

				err := envelope.Validate(tt.stream)
				if err == nil || !strings.Contains(err.Error(), tt.wantMessage) {
					t.Fatalf("expected missing retryable error %q, got %v", tt.wantMessage, err)
				}
			})
		}
	})

	t.Run("payload timestamps must use UTC", func(t *testing.T) {
		t.Parallel()

		nonUTC := time.Date(2026, 4, 28, 20, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60))
		tests := []struct {
			name        string
			envelope    Envelope
			stream      string
			wantMessage string
		}{
			{
				name: "provision succeeded",
				envelope: validEnvelope(MessageTypeDeviceProvisionSucceeded, DeviceProvisionSucceededPayload{
					OrgID:           testOrgID,
					AccountDeviceID: testDeviceID,
					VideoCloudDevid: "video-1",
					ActivityID:      "activity-1",
					ActivatedAt:     nonUTC,
				}),
				stream:      StreamVideoAccountEvents,
				wantMessage: "payload.activated_at must use UTC",
			},
			{
				name: "provision failed",
				envelope: validEnvelope(MessageTypeDeviceProvisionFailed, DeviceProvisionFailedPayload{
					OrgID:           testOrgID,
					AccountDeviceID: testDeviceID,
					VideoCloudDevid: "video-1",
					ActivityID:      "activity-1",
					ErrorCode:       "activation_failed",
					ErrorMessage:    "activation failed",
					Retryable:       true,
					FailedAt:        nonUTC,
				}),
				stream:      StreamVideoAccountEvents,
				wantMessage: "payload.failed_at must use UTC",
			},
			{
				name: "deactivate succeeded",
				envelope: validEnvelope(MessageTypeDeviceDeactivateSucceeded, DeviceDeactivateSucceededPayload{
					OrgID:           testOrgID,
					AccountDeviceID: testDeviceID,
					VideoCloudDevid: "video-1",
					DeactivatedAt:   nonUTC,
				}),
				stream:      StreamVideoAccountEvents,
				wantMessage: "payload.deactivated_at must use UTC",
			},
			{
				name: "deactivate failed",
				envelope: validEnvelope(MessageTypeDeviceDeactivateFailed, DeviceDeactivateFailedPayload{
					OrgID:           testOrgID,
					AccountDeviceID: testDeviceID,
					VideoCloudDevid: "video-1",
					ErrorCode:       "deactivation_failed",
					ErrorMessage:    "deactivation failed",
					Retryable:       true,
					FailedAt:        nonUTC,
				}),
				stream:      StreamVideoAccountEvents,
				wantMessage: "payload.failed_at must use UTC",
			},
			{
				name: "online changed",
				envelope: validEnvelope(MessageTypeDeviceOnlineChanged, DeviceOnlineChangedPayload{
					OrgID:           testOrgID,
					AccountDeviceID: testDeviceID,
					VideoCloudDevid: "video-1",
					Status:          OnlineStatusOnline,
					LastSeenAt:      nonUTC,
				}),
				stream:      StreamVideoAccountEvents,
				wantMessage: "payload.last_seen_at must use UTC",
			},
		}

		for _, tt := range tests {
			tt := tt
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				err := tt.envelope.Validate(tt.stream)
				if err == nil || !strings.Contains(err.Error(), tt.wantMessage) {
					t.Fatalf("expected UTC payload timestamp error %q, got %v", tt.wantMessage, err)
				}
			})
		}
	})
}

func TestValidateRejectsMissingEnvelopeFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		mutate      func(*Envelope)
		wantMessage string
	}{
		{
			name: "message id",
			mutate: func(envelope *Envelope) {
				envelope.MessageID = " "
			},
			wantMessage: "message_id must be non-empty",
		},
		{
			name: "correlation id",
			mutate: func(envelope *Envelope) {
				envelope.CorrelationID = ""
			},
			wantMessage: "correlation_id must be non-empty",
		},
		{
			name: "operation id",
			mutate: func(envelope *Envelope) {
				envelope.OperationID = ""
			},
			wantMessage: "operation_id must be non-empty",
		},
		{
			name: "source service",
			mutate: func(envelope *Envelope) {
				envelope.SourceService = ""
			},
			wantMessage: "source_service must be non-empty",
		},
		{
			name: "target service",
			mutate: func(envelope *Envelope) {
				envelope.TargetService = ""
			},
			wantMessage: "target_service must be non-empty",
		},
		{
			name: "schema version",
			mutate: func(envelope *Envelope) {
				envelope.SchemaVersion = ""
			},
			wantMessage: "schema_version must be non-empty",
		},
		{
			name: "message type",
			mutate: func(envelope *Envelope) {
				envelope.MessageType = ""
			},
			wantMessage: "message_type must be non-empty",
		},
		{
			name: "partition key",
			mutate: func(envelope *Envelope) {
				envelope.PartitionKey = ""
			},
			wantMessage: "partition_key must be non-empty",
		},
		{
			name: "occurred at",
			mutate: func(envelope *Envelope) {
				envelope.OccurredAt = time.Time{}
			},
			wantMessage: "occurred_at must be set",
		},
		{
			name: "payload",
			mutate: func(envelope *Envelope) {
				envelope.Payload = nil
			},
			wantMessage: "payload must be set",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			envelope := validEnvelope(MessageTypeDeviceProvisionRequested, DeviceProvisionRequestedPayload{
				OrgID:           testOrgID,
				AccountDeviceID: testDeviceID,
				VideoCloudDevid: "video-1",
				ActivityID:      "activity-1",
				ClipPublicKey:   "clip-key",
				RequestedBy:     "user-1",
			})
			tt.mutate(&envelope)

			err := envelope.Validate(StreamAccountVideoCommands)
			if err == nil || !strings.Contains(err.Error(), tt.wantMessage) {
				t.Fatalf("expected missing field error %q, got %v", tt.wantMessage, err)
			}
		})
	}
}

func TestEnvelopeUnmarshalRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	var envelope Envelope
	err := json.Unmarshal([]byte(`{
		"message_id":"msg-1",
		"correlation_id":"corr-1",
		"operation_id":"op-1",
		"source_service":"rtk_account_manager",
		"target_service":"realtek_video_server",
		"message_type":"DeviceProvisionRequested",
		"schema_version":"1.0",
		"partition_key":"22222222-2222-2222-2222-222222222222",
		"occurred_at":"2026-04-28T12:00:00Z",
		"payload":{},
		"unexpected":"value"
	}`), &envelope)
	if err == nil || !strings.Contains(err.Error(), `json: unknown field "unexpected"`) {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}

func TestDecodeStrictJSONRejectsMultipleJSONValues(t *testing.T) {
	t.Parallel()

	var envelope Envelope
	err := decodeStrictJSON([]byte(`{
		"message_id":"msg-1",
		"correlation_id":"corr-1",
		"operation_id":"op-1",
		"source_service":"rtk_account_manager",
		"target_service":"realtek_video_server",
		"message_type":"DeviceProvisionRequested",
		"schema_version":"1.0",
		"partition_key":"22222222-2222-2222-2222-222222222222",
		"occurred_at":"2026-04-28T12:00:00Z",
		"payload":{}
	} {}`), &envelope)
	if err == nil || !strings.Contains(err.Error(), "must contain a single JSON value") {
		t.Fatalf("expected multiple JSON values error, got %v", err)
	}
}

func FuzzEnvelopeStrictJSONAndValidation(f *testing.F) {
	valid := validEnvelope(MessageTypeDeviceProvisionRequested, DeviceProvisionRequestedPayload{
		OrgID:           testOrgID,
		AccountDeviceID: testDeviceID,
		VideoCloudDevid: "video-1",
		ActivityID:      "activity-1",
		ClipPublicKey:   "clip-key",
		RequestedBy:     "user-1",
	})
	validJSON, err := json.Marshal(valid)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(string(validJSON), StreamAccountVideoCommands)
	f.Add(`{"message_id":"msg-1","correlation_id":"corr-1","operation_id":"op-1","source_service":"rtk_account_manager","target_service":"realtek_video_server","message_type":"DeviceProvisionRequested","schema_version":"1.0","partition_key":"22222222-2222-2222-2222-222222222222","occurred_at":"2026-04-28T12:00:00Z","payload":{},"unexpected":"value"}`, StreamAccountVideoCommands)
	f.Add(string(validJSON)+` {}`, StreamAccountVideoCommands)
	f.Add(strings.ReplaceAll(string(validJSON), "DeviceProvisionRequested", "UnknownMessage"), StreamAccountVideoCommands)
	f.Add(strings.ReplaceAll(string(validJSON), StreamAccountVideoCommands, StreamVideoAccountEvents), StreamVideoAccountEvents)
	f.Add(strings.ReplaceAll(string(validJSON), testDeviceID, "33333333-3333-3333-3333-333333333333"), StreamAccountVideoCommands)
	f.Add(`not json`, StreamAccountVideoCommands)

	f.Fuzz(func(t *testing.T, data string, stream string) {
		var envelope Envelope
		err := json.Unmarshal([]byte(data), &envelope)
		if err != nil {
			return
		}
		payload, err := envelope.ValidateAndDecode(stream)
		if err != nil {
			return
		}
		if payload.PartitionKey() == "" {
			t.Fatalf("validated payload has empty partition key: %+v", payload)
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
		PartitionKey:  testDeviceID,
		OccurredAt:    testTime,
		Payload:       raw,
	}
}
