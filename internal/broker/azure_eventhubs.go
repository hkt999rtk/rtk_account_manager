package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	azeventhubs "github.com/Azure/azure-sdk-for-go/sdk/messaging/azeventhubs/v2"

	"rtk_account_manager/internal/channel"
)

const defaultAzureReceiveTimeout = time.Second

type azureProducerBatch interface {
	AddEventData(*azeventhubs.EventData, *azeventhubs.AddEventDataOptions) error
	NumEvents() int
}

type azureProducerClient interface {
	NewEventDataBatch(context.Context, *azeventhubs.EventDataBatchOptions) (azureProducerBatch, error)
	SendEventDataBatch(context.Context, azureProducerBatch) error
	Close(context.Context) error
}

type azureConsumerClient interface {
	GetEventHubProperties(context.Context, *azeventhubs.GetEventHubPropertiesOptions) (azeventhubs.EventHubProperties, error)
	NewPartitionClient(string, *azeventhubs.PartitionClientOptions) (azurePartitionClient, error)
	Close(context.Context) error
}

type azurePartitionClient interface {
	ReceiveEvents(context.Context, int, *azeventhubs.ReceiveEventsOptions) ([]*azeventhubs.ReceivedEventData, error)
	Close(context.Context) error
}

type sdkProducerClient struct {
	client *azeventhubs.ProducerClient
}

type sdkProducerBatch struct {
	batch *azeventhubs.EventDataBatch
}

func (b sdkProducerBatch) AddEventData(event *azeventhubs.EventData, options *azeventhubs.AddEventDataOptions) error {
	return b.batch.AddEventData(event, options)
}

func (b sdkProducerBatch) NumEvents() int {
	return int(b.batch.NumEvents())
}

func (c sdkProducerClient) NewEventDataBatch(ctx context.Context, options *azeventhubs.EventDataBatchOptions) (azureProducerBatch, error) {
	batch, err := c.client.NewEventDataBatch(ctx, options)
	if err != nil {
		return nil, err
	}
	return sdkProducerBatch{batch: batch}, nil
}

func (c sdkProducerClient) SendEventDataBatch(ctx context.Context, batch azureProducerBatch) error {
	typed, ok := batch.(sdkProducerBatch)
	if !ok {
		return fmt.Errorf("unsupported azure event hubs batch type %T", batch)
	}
	return c.client.SendEventDataBatch(ctx, typed.batch, nil)
}

func (c sdkProducerClient) Close(ctx context.Context) error {
	return c.client.Close(ctx)
}

type sdkConsumerClient struct {
	client *azeventhubs.ConsumerClient
}

type sdkPartitionClient struct {
	client *azeventhubs.PartitionClient
}

func (c sdkConsumerClient) GetEventHubProperties(ctx context.Context, options *azeventhubs.GetEventHubPropertiesOptions) (azeventhubs.EventHubProperties, error) {
	return c.client.GetEventHubProperties(ctx, options)
}

func (c sdkConsumerClient) NewPartitionClient(partitionID string, options *azeventhubs.PartitionClientOptions) (azurePartitionClient, error) {
	client, err := c.client.NewPartitionClient(partitionID, options)
	if err != nil {
		return nil, err
	}
	return sdkPartitionClient{client: client}, nil
}

func (c sdkConsumerClient) Close(ctx context.Context) error {
	return c.client.Close(ctx)
}

func (c sdkPartitionClient) ReceiveEvents(ctx context.Context, count int, options *azeventhubs.ReceiveEventsOptions) ([]*azeventhubs.ReceivedEventData, error) {
	return c.client.ReceiveEvents(ctx, count, options)
}

func (c sdkPartitionClient) Close(ctx context.Context) error {
	return c.client.Close(ctx)
}

type AzureEventHubsPublisher struct {
	client azureProducerClient
	stream string
}

func NewAzureEventHubsPublisherFromConnectionString(connectionString, stream string) (*AzureEventHubsPublisher, error) {
	if connectionString == "" {
		return nil, fmt.Errorf("AZURE_EVENTHUB_CONNECTION_STRING is required when CROSS_SERVICE_BROKER=%q", AdapterAzureEventHubs)
	}
	if stream == "" {
		return nil, errors.New("azure event hubs publisher stream is required")
	}

	client, err := azeventhubs.NewProducerClientFromConnectionString(connectionString, stream, nil)
	if err != nil {
		return nil, err
	}
	return newAzureEventHubsPublisher(sdkProducerClient{client: client}, stream), nil
}

func newAzureEventHubsPublisher(client azureProducerClient, stream string) *AzureEventHubsPublisher {
	return &AzureEventHubsPublisher{
		client: client,
		stream: stream,
	}
}

func (p *AzureEventHubsPublisher) Publish(ctx context.Context, stream string, envelope channel.Envelope) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if stream != p.stream {
		return fmt.Errorf("azure event hubs publisher configured for stream %q, got %q", p.stream, stream)
	}

	record, err := json.Marshal(logRecord{
		Stream:   stream,
		Envelope: envelope,
	})
	if err != nil {
		return err
	}

	batch, err := p.client.NewEventDataBatch(ctx, &azeventhubs.EventDataBatchOptions{
		PartitionKey: &envelope.PartitionKey,
	})
	if err != nil {
		return classifyAzurePublishError(err)
	}

	contentType := "application/json"
	if err := batch.AddEventData(&azeventhubs.EventData{
		Body:          record,
		ContentType:   &contentType,
		MessageID:     &envelope.MessageID,
		CorrelationID: envelope.CorrelationID,
	}, nil); err != nil {
		return classifyAzurePublishError(err)
	}
	if batch.NumEvents() != 1 {
		return fmt.Errorf("azure event hubs batch rejected message %q", envelope.MessageID)
	}

	if err := p.client.SendEventDataBatch(ctx, batch); err != nil {
		return classifyAzurePublishError(err)
	}
	return nil
}

func (p *AzureEventHubsPublisher) Close(ctx context.Context) error {
	return p.client.Close(ctx)
}

type azurePartitionReceiver struct {
	id     string
	client azurePartitionClient
}

type AzureEventHubsConsumer struct {
	client         azureConsumerClient
	stream         string
	receiveTimeout time.Duration

	mu         sync.Mutex
	partitions []azurePartitionReceiver
	nextIndex  int
}

func NewAzureEventHubsConsumerFromConnectionString(connectionString, stream, consumerGroup string, receiveTimeout time.Duration) (*AzureEventHubsConsumer, error) {
	if connectionString == "" {
		return nil, fmt.Errorf("AZURE_EVENTHUB_CONNECTION_STRING is required when CROSS_SERVICE_BROKER=%q", AdapterAzureEventHubs)
	}
	if stream == "" {
		return nil, errors.New("azure event hubs consumer stream is required")
	}
	if consumerGroup == "" {
		consumerGroup = azeventhubs.DefaultConsumerGroup
	}
	if receiveTimeout <= 0 {
		receiveTimeout = defaultAzureReceiveTimeout
	}

	client, err := azeventhubs.NewConsumerClientFromConnectionString(connectionString, stream, consumerGroup, nil)
	if err != nil {
		return nil, err
	}
	return newAzureEventHubsConsumer(sdkConsumerClient{client: client}, stream, receiveTimeout)
}

func newAzureEventHubsConsumer(client azureConsumerClient, stream string, receiveTimeout time.Duration) (*AzureEventHubsConsumer, error) {
	partitions, err := openAzurePartitions(context.Background(), client)
	if err != nil {
		_ = client.Close(context.Background())
		return nil, err
	}

	return &AzureEventHubsConsumer{
		client:         client,
		stream:         stream,
		receiveTimeout: receiveTimeout,
		partitions:     partitions,
	}, nil
}

func openAzurePartitions(ctx context.Context, client azureConsumerClient) ([]azurePartitionReceiver, error) {
	properties, err := client.GetEventHubProperties(ctx, nil)
	if err != nil {
		return nil, err
	}

	partitionIDs := append([]string(nil), properties.PartitionIDs...)
	sort.Strings(partitionIDs)

	partitions := make([]azurePartitionReceiver, 0, len(partitionIDs))
	for _, partitionID := range partitionIDs {
		partitionClient, err := client.NewPartitionClient(partitionID, &azeventhubs.PartitionClientOptions{
			StartPosition: azeventhubs.StartPosition{Earliest: boolPtr(true)},
		})
		if err != nil {
			closeAzurePartitions(context.Background(), partitions)
			return nil, err
		}
		partitions = append(partitions, azurePartitionReceiver{
			id:     partitionID,
			client: partitionClient,
		})
	}
	return partitions, nil
}

func (c *AzureEventHubsConsumer) Receive(ctx context.Context, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 1
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	partitions := c.snapshotPartitions()
	if len(partitions) == 0 {
		return nil, nil
	}

	messages := make([]Message, 0, limit)
	for checked := 0; checked < len(partitions) && len(messages) < limit; checked++ {
		partition := partitions[(c.nextPartitionIndex())%len(partitions)]
		receiveCtx, cancel := context.WithTimeout(ctx, c.receiveTimeout)
		events, err := partition.client.ReceiveEvents(receiveCtx, limit-len(messages), nil)
		cancel()
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				continue
			}
			return nil, classifyAzureReceiveError(err)
		}

		for _, event := range events {
			message, err := messageFromAzureEvent(c.stream, event)
			if err != nil {
				return nil, err
			}
			messages = append(messages, message)
			if len(messages) >= limit {
				break
			}
		}
	}

	return messages, nil
}

func (c *AzureEventHubsConsumer) Close(ctx context.Context) error {
	c.mu.Lock()
	partitions := c.partitions
	c.partitions = nil
	c.mu.Unlock()

	closeAzurePartitions(ctx, partitions)
	return c.client.Close(ctx)
}

func (c *AzureEventHubsConsumer) snapshotPartitions() []azurePartitionReceiver {
	c.mu.Lock()
	defer c.mu.Unlock()

	partitions := make([]azurePartitionReceiver, len(c.partitions))
	copy(partitions, c.partitions)
	return partitions
}

func (c *AzureEventHubsConsumer) nextPartitionIndex() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	index := c.nextIndex
	c.nextIndex++
	return index
}

func closeAzurePartitions(ctx context.Context, partitions []azurePartitionReceiver) {
	for _, partition := range partitions {
		_ = partition.client.Close(ctx)
	}
}

func messageFromAzureEvent(defaultStream string, event *azeventhubs.ReceivedEventData) (Message, error) {
	if event == nil {
		return Message{}, errors.New("received nil azure event")
	}

	var record logRecord
	if err := json.Unmarshal(event.Body, &record); err != nil {
		return Message{}, err
	}
	if record.Stream == "" {
		record.Stream = defaultStream
	}
	return Message{
		Stream:   record.Stream,
		Envelope: record.Envelope,
	}, nil
}

func classifyAzurePublishError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}

	var eventHubErr *azeventhubs.Error
	if errors.As(err, &eventHubErr) && eventHubErr.Code != azeventhubs.ErrorCodeUnauthorizedAccess {
		return Transient(err)
	}
	return err
}

func classifyAzureReceiveError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}

	var eventHubErr *azeventhubs.Error
	if errors.As(err, &eventHubErr) && eventHubErr.Code != azeventhubs.ErrorCodeUnauthorizedAccess {
		return Transient(err)
	}
	return err
}

func boolPtr(value bool) *bool {
	return &value
}
