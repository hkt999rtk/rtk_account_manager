package broker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"

	"rtk_account_manager/internal/channel"
)

func TestNATSPublisherConsumerRoundTripAndAck(t *testing.T) {
	srv := startTestNATSServer(t)

	publisher, err := NewPublisher(AdapterNATS, PublisherOptions{
		NATSURL:        srv.ClientURL(),
		NATSName:       "account-manager-test-publisher",
		PartitionCount: 4,
	})
	if err != nil {
		t.Fatalf("NewPublisher() error = %v", err)
	}
	defer publisher.Close(context.Background())

	consumer, err := NewConsumer(AdapterNATS, ConsumerOptions{
		NATSURL:        srv.ClientURL(),
		NATSName:       "account-manager-test-consumer",
		Stream:         channel.StreamAccountVideoCommands,
		ConsumerGroup:  "video-cloud-test",
		PartitionCount: 4,
		ReceiveTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewConsumer() error = %v", err)
	}
	defer consumer.Close(context.Background())

	envelope := channel.Envelope{
		MessageID:     "msg-1",
		CorrelationID: "corr-1",
		OperationID:   "op-1",
		SourceService: channel.ServiceAccountManager,
		TargetService: channel.ServiceRealtekVideoCloud,
		MessageType:   channel.MessageTypeDeviceProvisionRequested,
		SchemaVersion: channel.SchemaVersionV1,
		PartitionKey:  "11111111-1111-1111-1111-111111111111",
		OccurredAt:    time.Date(2026, 5, 31, 13, 30, 0, 0, time.UTC),
		Payload:       json.RawMessage(`{"org_id":"00000000-0000-0000-0000-000000000001","account_device_id":"11111111-1111-1111-1111-111111111111","video_cloud_devid":"device-1","activity_id":"activity-1","clip_public_key":"clip","requested_by":"user-1"}`),
	}
	if err := publisher.Publish(context.Background(), channel.StreamAccountVideoCommands, envelope); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	messages, err := consumer.Receive(context.Background(), 1)
	if err != nil {
		t.Fatalf("Receive() error = %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("Receive() got %d messages, want 1", len(messages))
	}
	if messages[0].Stream != channel.StreamAccountVideoCommands {
		t.Fatalf("unexpected stream: %s", messages[0].Stream)
	}
	if messages[0].Envelope.MessageID != envelope.MessageID || messages[0].Envelope.PartitionKey != envelope.PartitionKey {
		t.Fatalf("unexpected envelope: %+v", messages[0].Envelope)
	}
	if err := messages[0].Acknowledge(context.Background()); err != nil {
		t.Fatalf("Acknowledge() error = %v", err)
	}

	again, err := consumer.Receive(context.Background(), 1)
	if err != nil {
		t.Fatalf("second Receive() error = %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("message redelivered after ack: %+v", again)
	}
}

func TestNATSBrokerRequiresURL(t *testing.T) {
	if _, err := NewPublisher(AdapterNATS, PublisherOptions{}); err == nil || !strings.Contains(err.Error(), "CROSS_SERVICE_NATS_URL") {
		t.Fatalf("NewPublisher() error = %v, want missing URL", err)
	}
	if _, err := NewConsumer(AdapterNATS, ConsumerOptions{Stream: channel.StreamVideoAccountEvents}); err == nil || !strings.Contains(err.Error(), "CROSS_SERVICE_NATS_URL") {
		t.Fatalf("NewConsumer() error = %v, want missing URL", err)
	}
}

func startTestNATSServer(t *testing.T) *natsserver.Server {
	t.Helper()
	srv, err := natsserver.NewServer(&natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server did not become ready")
	}
	t.Cleanup(func() {
		srv.Shutdown()
		srv.WaitForShutdown()
	})
	return srv
}
