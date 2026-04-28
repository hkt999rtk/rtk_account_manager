package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"rtk_account_manager/internal/channel"
)

func TestLogPublisherWritesEnvelopeJSON(t *testing.T) {
	var buf bytes.Buffer
	publisher := NewLogPublisher(&buf)

	envelope := channel.Envelope{
		MessageID:     "msg-1",
		CorrelationID: "corr-1",
		OperationID:   "op-1",
		SourceService: channel.ServiceAccountManager,
		TargetService: channel.ServiceRealtekVideoCloud,
		MessageType:   channel.MessageTypeDeviceProvisionRequested,
		SchemaVersion: channel.SchemaVersionV1,
		PartitionKey:  "device-1",
		OccurredAt:    time.Date(2026, 4, 29, 10, 0, 0, 0, time.UTC),
		Payload:       json.RawMessage(`{"org_id":"org-1","account_device_id":"device-1","video_cloud_devid":"video-1","activity_id":"activity-1","clip_public_key":"clip","requested_by":"user-1"}`),
	}

	if err := publisher.Publish(context.Background(), channel.StreamAccountVideoCommands, envelope); err != nil {
		t.Fatal(err)
	}

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record["stream"] != channel.StreamAccountVideoCommands {
		t.Fatalf("unexpected stream: %+v", record["stream"])
	}
}

func TestLogConsumerReadsEnvelopeJSON(t *testing.T) {
	input := bytes.NewBufferString(`{"stream":"video.account.events","envelope":{"message_id":"msg-1","correlation_id":"corr-1","operation_id":"op-1","source_service":"realtek_video_server","target_service":"rtk_account_manager","message_type":"DeviceOnlineChanged","schema_version":"1.0","partition_key":"11111111-1111-1111-1111-111111111111","occurred_at":"2026-04-29T10:00:00Z","payload":{"org_id":"00000000-0000-0000-0000-000000000001","account_device_id":"11111111-1111-1111-1111-111111111111","video_cloud_devid":"video-1","status":"online","last_seen_at":"2026-04-29T10:00:00Z"}}}` + "\n")
	consumer := NewLogConsumer(input)

	messages, err := consumer.Receive(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 {
		t.Fatalf("expected one message, got %d", len(messages))
	}
	if messages[0].Stream != channel.StreamVideoAccountEvents {
		t.Fatalf("unexpected stream: %s", messages[0].Stream)
	}
	if messages[0].Envelope.MessageType != channel.MessageTypeDeviceOnlineChanged {
		t.Fatalf("unexpected message type: %s", messages[0].Envelope.MessageType)
	}
}

func TestTransientMarker(t *testing.T) {
	err := Transient(errors.New("temporary"))
	if !IsTransient(err) {
		t.Fatal("expected transient marker")
	}
	if IsTransient(errors.New("permanent")) {
		t.Fatal("expected permanent error to stay non-transient")
	}
}
