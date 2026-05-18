package broker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	azeventhubs "github.com/Azure/azure-sdk-for-go/sdk/messaging/azeventhubs/v2"

	"rtk_account_manager/internal/channel"
)

type fakeAzureProducerClient struct {
	newBatchOptions *azeventhubs.EventDataBatchOptions
	batch           *fakeAzureProducerBatch
	sendBatch       azureProducerBatch
	newBatchErr     error
	sendErr         error
	closed          bool
}

type fakeAzureProducerBatch struct {
	events []*azeventhubs.EventData
	addErr error
}

func (c *fakeAzureProducerClient) NewEventDataBatch(_ context.Context, options *azeventhubs.EventDataBatchOptions) (azureProducerBatch, error) {
	if c.newBatchErr != nil {
		return nil, c.newBatchErr
	}
	c.newBatchOptions = options
	if c.batch == nil {
		c.batch = &fakeAzureProducerBatch{}
	}
	return c.batch, nil
}

func (c *fakeAzureProducerClient) SendEventDataBatch(_ context.Context, batch azureProducerBatch) error {
	c.sendBatch = batch
	return c.sendErr
}

func (c *fakeAzureProducerClient) Close(context.Context) error {
	c.closed = true
	return nil
}

func (b *fakeAzureProducerBatch) AddEventData(event *azeventhubs.EventData, _ *azeventhubs.AddEventDataOptions) error {
	if b.addErr != nil {
		return b.addErr
	}
	b.events = append(b.events, event)
	return nil
}

func (b *fakeAzureProducerBatch) NumEvents() int {
	return len(b.events)
}

type fakeAzureConsumerClient struct {
	properties  azeventhubs.EventHubProperties
	partitions  map[string]*fakeAzurePartitionClient
	propsErr    error
	newErr      error
	newErrByID  map[string]error
	optionsByID map[string]azeventhubs.PartitionClientOptions
	closed      bool
}

type fakeAzurePartitionClient struct {
	events   []*azeventhubs.ReceivedEventData
	err      error
	closed   bool
	received []int
}

func (c *fakeAzureConsumerClient) GetEventHubProperties(context.Context, *azeventhubs.GetEventHubPropertiesOptions) (azeventhubs.EventHubProperties, error) {
	if c.propsErr != nil {
		return azeventhubs.EventHubProperties{}, c.propsErr
	}
	return c.properties, nil
}

func (c *fakeAzureConsumerClient) NewPartitionClient(partitionID string, options *azeventhubs.PartitionClientOptions) (azurePartitionClient, error) {
	if err := c.newErrByID[partitionID]; err != nil {
		return nil, err
	}
	if c.newErr != nil {
		return nil, c.newErr
	}
	if options != nil {
		if c.optionsByID == nil {
			c.optionsByID = make(map[string]azeventhubs.PartitionClientOptions)
		}
		c.optionsByID[partitionID] = *options
	}
	return c.partitions[partitionID], nil
}

func (c *fakeAzureConsumerClient) Close(context.Context) error {
	c.closed = true
	return nil
}

func (c *fakeAzurePartitionClient) ReceiveEvents(_ context.Context, count int, _ *azeventhubs.ReceiveEventsOptions) ([]*azeventhubs.ReceivedEventData, error) {
	c.received = append(c.received, count)
	if c.err != nil {
		return nil, c.err
	}
	events := c.events
	if len(events) > count {
		events = events[:count]
	}
	c.events = c.events[len(events):]
	return events, nil
}

func (c *fakeAzurePartitionClient) Close(context.Context) error {
	c.closed = true
	return nil
}

func TestAzureEventHubsPublisherPublishesJSONRecord(t *testing.T) {
	client := &fakeAzureProducerClient{}
	publisher := newAzureEventHubsPublisher(client, channel.StreamAccountVideoCommands)

	envelope := channel.Envelope{
		MessageID:     "msg-1",
		CorrelationID: "corr-1",
		OperationID:   "op-1",
		SourceService: channel.ServiceAccountManager,
		TargetService: channel.ServiceRealtekVideoCloud,
		MessageType:   channel.MessageTypeDeviceProvisionRequested,
		SchemaVersion: channel.SchemaVersionV1,
		PartitionKey:  "device-1",
		OccurredAt:    time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC),
		Payload:       json.RawMessage(`{"hello":"world"}`),
	}

	if err := publisher.Publish(context.Background(), channel.StreamAccountVideoCommands, envelope); err != nil {
		t.Fatal(err)
	}

	if client.newBatchOptions == nil || client.newBatchOptions.PartitionKey == nil || *client.newBatchOptions.PartitionKey != envelope.PartitionKey {
		t.Fatalf("expected partition key to be forwarded, got %+v", client.newBatchOptions)
	}
	if client.batch == nil || len(client.batch.events) != 1 {
		t.Fatalf("expected one event in azure batch, got %+v", client.batch)
	}
	if client.sendBatch == nil {
		t.Fatal("expected send to be invoked")
	}

	var record logRecord
	if err := json.Unmarshal(client.batch.events[0].Body, &record); err != nil {
		t.Fatal(err)
	}
	if record.Stream != channel.StreamAccountVideoCommands {
		t.Fatalf("unexpected stream: %q", record.Stream)
	}
	if record.Envelope.MessageID != envelope.MessageID {
		t.Fatalf("unexpected message id: %q", record.Envelope.MessageID)
	}
}

func TestAzureEventHubsPublisherMarksConnectionLossTransient(t *testing.T) {
	client := &fakeAzureProducerClient{
		sendErr: &azeventhubs.Error{Code: azeventhubs.ErrorCodeConnectionLost},
	}
	publisher := newAzureEventHubsPublisher(client, channel.StreamAccountVideoCommands)

	err := publisher.Publish(context.Background(), channel.StreamAccountVideoCommands, channel.Envelope{
		MessageID:     "msg-2",
		CorrelationID: "corr-2",
		OperationID:   "op-2",
		SourceService: channel.ServiceAccountManager,
		TargetService: channel.ServiceRealtekVideoCloud,
		MessageType:   channel.MessageTypeDeviceProvisionRequested,
		SchemaVersion: channel.SchemaVersionV1,
		PartitionKey:  "device-2",
		OccurredAt:    time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC),
		Payload:       json.RawMessage(`{"hello":"world"}`),
	})
	if !IsTransient(err) {
		t.Fatalf("expected transient azure publish error, got %v", err)
	}
}

func TestAzureEventHubsPublisherCloseClosesClient(t *testing.T) {
	client := &fakeAzureProducerClient{}
	publisher := newAzureEventHubsPublisher(client, channel.StreamAccountVideoCommands)

	if err := publisher.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !client.closed {
		t.Fatal("expected publisher close to close azure client")
	}
}

func TestAzureCheckpointFileDefaultsAndSanitizesComponents(t *testing.T) {
	explicit := resolveAzureCheckpointFile("/tmp/custom-checkpoint.json", "ignored stream", "ignored group")
	if explicit != "/tmp/custom-checkpoint.json" {
		t.Fatalf("expected explicit checkpoint path to win, got %q", explicit)
	}

	resolved := resolveAzureCheckpointFile("", "account.video.commands", "$Default/Group")
	want := filepath.Join(defaultAzureCheckpointDir, "account_video_commands___Default_Group.json")
	if resolved != want {
		t.Fatalf("expected sanitized default checkpoint path %q, got %q", want, resolved)
	}

	if got := sanitizeAzureCheckpointComponent("AZaz09._-/"); got != "AZaz09____" {
		t.Fatalf("unexpected sanitized component: %q", got)
	}
}

func TestAzureEventHubsPublisherMarksBatchErrorsTransient(t *testing.T) {
	t.Run("new batch", func(t *testing.T) {
		client := &fakeAzureProducerClient{
			newBatchErr: &azeventhubs.Error{Code: azeventhubs.ErrorCodeConnectionLost},
		}
		publisher := newAzureEventHubsPublisher(client, channel.StreamAccountVideoCommands)

		err := publisher.Publish(context.Background(), channel.StreamAccountVideoCommands, channel.Envelope{
			MessageID:    "msg-batch",
			PartitionKey: "device-batch",
		})
		if !IsTransient(err) {
			t.Fatalf("expected transient batch error, got %v", err)
		}
	})

	t.Run("add event", func(t *testing.T) {
		client := &fakeAzureProducerClient{
			batch: &fakeAzureProducerBatch{
				addErr: &azeventhubs.Error{Code: azeventhubs.ErrorCodeConnectionLost},
			},
		}
		publisher := newAzureEventHubsPublisher(client, channel.StreamAccountVideoCommands)

		err := publisher.Publish(context.Background(), channel.StreamAccountVideoCommands, channel.Envelope{
			MessageID:    "msg-add",
			PartitionKey: "device-add",
		})
		if !IsTransient(err) {
			t.Fatalf("expected transient add-event error, got %v", err)
		}
	})
}

func TestAzureEventHubsConsumerReadsAcrossPartitions(t *testing.T) {
	partitionOne := &fakeAzurePartitionClient{
		events: []*azeventhubs.ReceivedEventData{
			{
				EventData: azeventhubs.EventData{Body: []byte(`{"stream":"video.account.events","envelope":{"message_id":"msg-1","correlation_id":"corr-1","operation_id":"op-1","source_service":"realtek_video_server","target_service":"rtk_account_manager","message_type":"DeviceOnlineChanged","schema_version":"1.0","partition_key":"device-1","occurred_at":"2026-04-29T12:00:00Z","payload":{"status":"online"}}}`)},
			},
		},
	}
	partitionTwo := &fakeAzurePartitionClient{
		events: []*azeventhubs.ReceivedEventData{
			{
				EventData: azeventhubs.EventData{Body: []byte(`{"envelope":{"message_id":"msg-2","correlation_id":"corr-2","operation_id":"op-2","source_service":"realtek_video_server","target_service":"rtk_account_manager","message_type":"DeviceMetadataChanged","schema_version":"1.0","partition_key":"device-2","occurred_at":"2026-04-29T12:00:01Z","payload":{"video_cloud_devid":"vc-2"}}}`)},
			},
		},
	}

	client := &fakeAzureConsumerClient{
		properties: azeventhubs.EventHubProperties{PartitionIDs: []string{"1", "0"}},
		partitions: map[string]*fakeAzurePartitionClient{
			"0": partitionOne,
			"1": partitionTwo,
		},
	}

	consumer, err := newAzureEventHubsConsumer(client, channel.StreamVideoAccountEvents, 50*time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}

	messages, err := consumer.Receive(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatalf("expected two messages, got %d", len(messages))
	}
	if messages[0].Envelope.MessageID != "msg-1" || messages[1].Envelope.MessageID != "msg-2" {
		t.Fatalf("unexpected messages: %+v", messages)
	}
	if messages[1].Stream != channel.StreamVideoAccountEvents {
		t.Fatalf("expected default stream fallback, got %q", messages[1].Stream)
	}
}

func TestAzureEventHubsConsumerAcknowledgesAndResumesFromCheckpoint(t *testing.T) {
	checkpointFile := filepath.Join(t.TempDir(), "consumer-checkpoints.json")
	client := &fakeAzureConsumerClient{
		properties: azeventhubs.EventHubProperties{PartitionIDs: []string{"0"}},
		partitions: map[string]*fakeAzurePartitionClient{
			"0": {
				events: []*azeventhubs.ReceivedEventData{
					{
						EventData:      azeventhubs.EventData{Body: []byte(`{"stream":"video.account.events","envelope":{"message_id":"msg-1","correlation_id":"corr-1","operation_id":"op-1","source_service":"realtek_video_server","target_service":"rtk_account_manager","message_type":"DeviceOnlineChanged","schema_version":"1.0","partition_key":"device-1","occurred_at":"2026-04-29T12:00:00Z","payload":{"status":"online"}}}`)},
						SequenceNumber: 41,
					},
				},
			},
		},
	}

	consumer, err := newAzureEventHubsConsumer(
		client,
		channel.StreamVideoAccountEvents,
		50*time.Millisecond,
		newAzureFileCheckpointStore(checkpointFile),
	)
	if err != nil {
		t.Fatal(err)
	}

	messages, err := consumer.Receive(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 {
		t.Fatalf("expected one message, got %d", len(messages))
	}
	if err := messages[0].Acknowledge(context.Background()); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(checkpointFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"sequence_number": 41`) {
		t.Fatalf("expected checkpoint file to persist the acknowledged sequence number, got %s", string(data))
	}

	resumedClient := &fakeAzureConsumerClient{
		properties: azeventhubs.EventHubProperties{PartitionIDs: []string{"0", "1"}},
		partitions: map[string]*fakeAzurePartitionClient{
			"0": {},
			"1": {},
		},
	}

	if _, err := newAzureEventHubsConsumer(
		resumedClient,
		channel.StreamVideoAccountEvents,
		50*time.Millisecond,
		newAzureFileCheckpointStore(checkpointFile),
	); err != nil {
		t.Fatal(err)
	}

	resumed := resumedClient.optionsByID["0"].StartPosition
	if resumed.SequenceNumber == nil || *resumed.SequenceNumber != 41 || resumed.Inclusive {
		t.Fatalf("expected partition 0 to resume after sequence 41, got %+v", resumedClient.optionsByID["0"].StartPosition)
	}

	unseen := resumedClient.optionsByID["1"].StartPosition
	if unseen.Earliest == nil || !*unseen.Earliest {
		t.Fatalf("expected partition 1 without a checkpoint to start from earliest, got %+v", unseen)
	}
}

func TestAzureEventHubsConsumerDoesNotAdvanceCheckpointPastEarlierUnacknowledgedMessage(t *testing.T) {
	checkpointFile := filepath.Join(t.TempDir(), "consumer-checkpoints.json")
	if err := os.WriteFile(checkpointFile, []byte("{\n  \"partitions\": {\n    \"0\": {\n      \"sequence_number\": 40,\n      \"updated_at\": \"2026-04-29T12:00:00Z\"\n    }\n  }\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	client := &fakeAzureConsumerClient{
		properties: azeventhubs.EventHubProperties{PartitionIDs: []string{"0"}},
		partitions: map[string]*fakeAzurePartitionClient{
			"0": {
				events: []*azeventhubs.ReceivedEventData{
					{
						EventData:      azeventhubs.EventData{Body: []byte(`{"stream":"video.account.events","envelope":{"message_id":"msg-41","correlation_id":"corr-41","operation_id":"op-41","source_service":"realtek_video_server","target_service":"rtk_account_manager","message_type":"DeviceOnlineChanged","schema_version":"1.0","partition_key":"device-1","occurred_at":"2026-04-29T12:00:00Z","payload":{"status":"online"}}}`)},
						SequenceNumber: 41,
					},
					{
						EventData:      azeventhubs.EventData{Body: []byte(`{"stream":"video.account.events","envelope":{"message_id":"msg-42","correlation_id":"corr-42","operation_id":"op-42","source_service":"realtek_video_server","target_service":"rtk_account_manager","message_type":"DeviceMetadataChanged","schema_version":"1.0","partition_key":"device-1","occurred_at":"2026-04-29T12:00:01Z","payload":{"video_cloud_devid":"vc-42"}}}`)},
						SequenceNumber: 42,
					},
				},
			},
		},
	}

	consumer, err := newAzureEventHubsConsumer(
		client,
		channel.StreamVideoAccountEvents,
		50*time.Millisecond,
		newAzureFileCheckpointStore(checkpointFile),
	)
	if err != nil {
		t.Fatal(err)
	}

	messages, err := consumer.Receive(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatalf("expected two messages, got %d", len(messages))
	}

	if err := messages[1].Acknowledge(context.Background()); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(checkpointFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"sequence_number": 40`) {
		t.Fatalf("expected checkpoint to remain at 40 until the earlier message is acknowledged, got %s", string(data))
	}

	if err := messages[0].Acknowledge(context.Background()); err != nil {
		t.Fatal(err)
	}

	data, err = os.ReadFile(checkpointFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"sequence_number": 42`) {
		t.Fatalf("expected checkpoint to advance to 42 after the ordered prefix is acknowledged, got %s", string(data))
	}
}

func TestOpenAzurePartitionsUsesStoredCheckpointWhenPresent(t *testing.T) {
	checkpointFile := filepath.Join(t.TempDir(), "consumer-checkpoints.json")
	if err := os.WriteFile(checkpointFile, []byte("{\n  \"partitions\": {\n    \"1\": {\n      \"sequence_number\": 17,\n      \"updated_at\": \"2026-04-29T12:00:00Z\"\n    }\n  }\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	client := &fakeAzureConsumerClient{
		properties: azeventhubs.EventHubProperties{PartitionIDs: []string{"1", "0"}},
		partitions: map[string]*fakeAzurePartitionClient{
			"0": {},
			"1": {},
		},
	}

	partitions, err := openAzurePartitions(context.Background(), client, newAzureFileCheckpointStore(checkpointFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(partitions) != 2 {
		t.Fatalf("expected two partitions, got %d", len(partitions))
	}

	partitionZero := client.optionsByID["0"].StartPosition
	if partitionZero.Earliest == nil || !*partitionZero.Earliest {
		t.Fatalf("expected missing checkpoint partition to start from earliest, got %+v", partitionZero)
	}

	partitionOne := client.optionsByID["1"].StartPosition
	if partitionOne.SequenceNumber == nil || *partitionOne.SequenceNumber != 17 || partitionOne.Inclusive {
		t.Fatalf("expected checkpointed partition to resume after sequence 17, got %+v", partitionOne)
	}
}

func TestAzureEventHubsConsumerTreatsReceiveTimeoutAsEmptyPoll(t *testing.T) {
	client := &fakeAzureConsumerClient{
		properties: azeventhubs.EventHubProperties{PartitionIDs: []string{"0"}},
		partitions: map[string]*fakeAzurePartitionClient{
			"0": {err: context.DeadlineExceeded},
		},
	}

	consumer, err := newAzureEventHubsConsumer(client, channel.StreamVideoAccountEvents, 10*time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}

	messages, err := consumer.Receive(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 0 {
		t.Fatalf("expected no messages on timeout poll, got %+v", messages)
	}
}

func TestAzureEventHubsConsumerMarksConnectionLossTransient(t *testing.T) {
	client := &fakeAzureConsumerClient{
		properties: azeventhubs.EventHubProperties{PartitionIDs: []string{"0"}},
		partitions: map[string]*fakeAzurePartitionClient{
			"0": {
				err: &azeventhubs.Error{Code: azeventhubs.ErrorCodeConnectionLost},
			},
		},
	}

	consumer, err := newAzureEventHubsConsumer(client, channel.StreamVideoAccountEvents, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}

	_, err = consumer.Receive(context.Background(), 1)
	if !IsTransient(err) {
		t.Fatalf("expected transient azure receive error, got %v", err)
	}
}

func TestNewAzureEventHubsConsumerClosesClientWhenPartitionOpenFails(t *testing.T) {
	client := &fakeAzureConsumerClient{
		properties: azeventhubs.EventHubProperties{PartitionIDs: []string{"0"}},
		newErr:     errors.New("open failed"),
	}

	consumer, err := newAzureEventHubsConsumer(client, channel.StreamVideoAccountEvents, time.Second, nil)
	if err == nil {
		t.Fatal("expected consumer construction error")
	}
	if consumer != nil {
		t.Fatalf("expected nil consumer on error, got %+v", consumer)
	}
	if !client.closed {
		t.Fatal("expected constructor failure to close client")
	}
}

func TestOpenAzurePartitionsClosesEarlierPartitionsOnFailure(t *testing.T) {
	first := &fakeAzurePartitionClient{}
	client := &fakeAzureConsumerClient{
		properties: azeventhubs.EventHubProperties{PartitionIDs: []string{"1", "0"}},
		partitions: map[string]*fakeAzurePartitionClient{
			"0": first,
		},
		newErrByID: map[string]error{
			"1": errors.New("open failed"),
		},
	}

	partitions, err := openAzurePartitions(context.Background(), client, nil)
	if err == nil {
		t.Fatal("expected partition open error")
	}
	if len(partitions) != 0 {
		t.Fatalf("expected no partitions on error, got %+v", partitions)
	}
	if !first.closed {
		t.Fatal("expected already-opened partition to be closed on failure")
	}
}

func TestNewAzureEventHubsConstructorsRequireConfig(t *testing.T) {
	if _, err := NewAzureEventHubsPublisherFromConnectionString("", "account.video.commands"); err == nil {
		t.Fatal("expected publisher config error")
	}
	if _, err := NewAzureEventHubsConsumerFromConnectionString("", "video.account.events", "group", time.Second, ""); err == nil {
		t.Fatal("expected consumer config error")
	}
}

func TestAzureEventHubsConsumerCloseClosesPartitions(t *testing.T) {
	partition := &fakeAzurePartitionClient{}
	client := &fakeAzureConsumerClient{
		properties: azeventhubs.EventHubProperties{PartitionIDs: []string{"0"}},
		partitions: map[string]*fakeAzurePartitionClient{
			"0": partition,
		},
	}

	consumer, err := newAzureEventHubsConsumer(client, channel.StreamVideoAccountEvents, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := consumer.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !partition.closed || !client.closed {
		t.Fatalf("expected azure consumer close to release partition and client, got partition=%v client=%v", partition.closed, client.closed)
	}
}

func TestAzureEventHubsPublisherRejectsUnexpectedStream(t *testing.T) {
	publisher := newAzureEventHubsPublisher(&fakeAzureProducerClient{}, channel.StreamAccountVideoCommands)
	err := publisher.Publish(context.Background(), channel.StreamVideoAccountEvents, channel.Envelope{})
	if err == nil {
		t.Fatal("expected stream mismatch error")
	}
}

func TestAzureMessageDecodeRejectsInvalidJSON(t *testing.T) {
	_, err := messageFromAzureEvent(channel.StreamVideoAccountEvents, &azeventhubs.ReceivedEventData{
		EventData: azeventhubs.EventData{Body: []byte("not-json")},
	}, nil)
	if err == nil {
		t.Fatal("expected invalid JSON error")
	}
}

func TestAzureMessageDecodeRejectsNilEvent(t *testing.T) {
	if _, err := messageFromAzureEvent(channel.StreamVideoAccountEvents, nil, nil); err == nil {
		t.Fatal("expected nil event error")
	}
}

func TestClassifyAzurePublishErrorLeavesUnauthorizedPermanent(t *testing.T) {
	err := classifyAzurePublishError(&azeventhubs.Error{Code: azeventhubs.ErrorCodeUnauthorizedAccess})
	if err == nil || IsTransient(err) {
		t.Fatalf("expected permanent unauthorized error, got %v", err)
	}
}

func TestClassifyAzurePublishErrorLeavesContextErrorsUntouched(t *testing.T) {
	if err := classifyAzurePublishError(context.Canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled error, got %v", err)
	}
}

func TestClassifyAzureReceiveErrorLeavesUnauthorizedPermanent(t *testing.T) {
	err := classifyAzureReceiveError(&azeventhubs.Error{Code: azeventhubs.ErrorCodeUnauthorizedAccess})
	if err == nil || IsTransient(err) {
		t.Fatalf("expected permanent unauthorized error, got %v", err)
	}
}

func TestClassifyAzureReceiveErrorLeavesContextErrorsUntouched(t *testing.T) {
	if err := classifyAzureReceiveError(context.Canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled error, got %v", err)
	}
}
