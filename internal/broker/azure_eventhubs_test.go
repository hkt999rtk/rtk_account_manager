package broker

import (
	"context"
	"encoding/json"
	"errors"
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
	properties azeventhubs.EventHubProperties
	partitions map[string]*fakeAzurePartitionClient
	propsErr   error
	newErr     error
	newErrByID map[string]error
	closed     bool
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

func (c *fakeAzureConsumerClient) NewPartitionClient(partitionID string, _ *azeventhubs.PartitionClientOptions) (azurePartitionClient, error) {
	if err := c.newErrByID[partitionID]; err != nil {
		return nil, err
	}
	if c.newErr != nil {
		return nil, c.newErr
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

	consumer, err := newAzureEventHubsConsumer(client, channel.StreamVideoAccountEvents, 50*time.Millisecond)
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

func TestAzureEventHubsConsumerTreatsReceiveTimeoutAsEmptyPoll(t *testing.T) {
	client := &fakeAzureConsumerClient{
		properties: azeventhubs.EventHubProperties{PartitionIDs: []string{"0"}},
		partitions: map[string]*fakeAzurePartitionClient{
			"0": {err: context.DeadlineExceeded},
		},
	}

	consumer, err := newAzureEventHubsConsumer(client, channel.StreamVideoAccountEvents, 10*time.Millisecond)
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

func TestNewAzureEventHubsConsumerClosesClientWhenPartitionOpenFails(t *testing.T) {
	client := &fakeAzureConsumerClient{
		properties: azeventhubs.EventHubProperties{PartitionIDs: []string{"0"}},
		newErr:     errors.New("open failed"),
	}

	consumer, err := newAzureEventHubsConsumer(client, channel.StreamVideoAccountEvents, time.Second)
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

	partitions, err := openAzurePartitions(context.Background(), client)
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
	if _, err := NewAzureEventHubsConsumerFromConnectionString("", "video.account.events", "group", time.Second); err == nil {
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

	consumer, err := newAzureEventHubsConsumer(client, channel.StreamVideoAccountEvents, time.Second)
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
	})
	if err == nil {
		t.Fatal("expected invalid JSON error")
	}
}

func TestAzureMessageDecodeRejectsNilEvent(t *testing.T) {
	if _, err := messageFromAzureEvent(channel.StreamVideoAccountEvents, nil); err == nil {
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
