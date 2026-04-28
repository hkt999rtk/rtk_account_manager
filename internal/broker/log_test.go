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

func TestTransientMarker(t *testing.T) {
	err := Transient(errors.New("temporary"))
	if !IsTransient(err) {
		t.Fatal("expected transient marker")
	}
	if IsTransient(errors.New("permanent")) {
		t.Fatal("expected permanent error to stay non-transient")
	}
}
