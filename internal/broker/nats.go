package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"rtk_account_manager/internal/channel"
)

const (
	accountVideoCommandsStreamName = "ACCOUNT_VIDEO_COMMANDS"
	videoAccountEventsStreamName   = "VIDEO_ACCOUNT_EVENTS"
)

type NATSOptions struct {
	URL            string
	Name           string
	PartitionCount int
	Stream         string
	ConsumerGroup  string
	ReceiveTimeout time.Duration
}

type natsStreamSpec struct {
	logicalName    string
	jetStreamName  string
	subjectPattern string
	subjectPrefix  string
}

type NATSPublisher struct {
	conn           *nats.Conn
	js             jetstream.JetStream
	partitionCount int
}

type NATSConsumer struct {
	conn           *nats.Conn
	js             jetstream.JetStream
	stream         string
	consumerGroup  string
	partitionCount int
	receiveTimeout time.Duration
}

func NewNATSPublisher(opts NATSOptions) (*NATSPublisher, error) {
	conn, js, partitionCount, err := connectNATS(opts)
	if err != nil {
		return nil, err
	}
	return &NATSPublisher{conn: conn, js: js, partitionCount: partitionCount}, nil
}

func NewNATSConsumer(opts NATSOptions) (*NATSConsumer, error) {
	if strings.TrimSpace(opts.URL) == "" {
		return nil, fmt.Errorf("CROSS_SERVICE_NATS_URL is required when CROSS_SERVICE_BROKER=%q", AdapterNATS)
	}
	if strings.TrimSpace(opts.Stream) == "" {
		return nil, fmt.Errorf("stream is required for nats consumer")
	}
	if strings.TrimSpace(opts.ConsumerGroup) == "" {
		return nil, fmt.Errorf("consumer group is required for nats consumer")
	}
	conn, js, partitionCount, err := connectNATS(opts)
	if err != nil {
		return nil, err
	}
	receiveTimeout := opts.ReceiveTimeout
	if receiveTimeout <= 0 {
		receiveTimeout = 5 * time.Second
	}
	return &NATSConsumer{
		conn:           conn,
		js:             js,
		stream:         opts.Stream,
		consumerGroup:  opts.ConsumerGroup,
		partitionCount: partitionCount,
		receiveTimeout: receiveTimeout,
	}, nil
}

func (p *NATSPublisher) Publish(ctx context.Context, stream string, envelope channel.Envelope) error {
	if err := envelope.Validate(stream); err != nil {
		return err
	}
	spec, err := natsSpecForStream(stream)
	if err != nil {
		return err
	}
	if err := ensureNATSStream(ctx, p.js, spec); err != nil {
		return Transient(err)
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	if _, err := p.js.Publish(ctx, spec.subjectForPartition(p.partitionCount, envelope.PartitionKey), body); err != nil {
		return Transient(fmt.Errorf("publish message: %w", err))
	}
	return nil
}

func (p *NATSPublisher) Close(ctx context.Context) error {
	if p == nil || p.conn == nil {
		return nil
	}
	return closeNATS(ctx, p.conn)
}

func (c *NATSConsumer) Receive(ctx context.Context, limit int) ([]Message, error) {
	if limit <= 0 {
		return nil, nil
	}
	spec, err := natsSpecForStream(c.stream)
	if err != nil {
		return nil, err
	}
	if err := ensureNATSStream(ctx, c.js, spec); err != nil {
		return nil, Transient(err)
	}
	consumer, err := c.js.CreateOrUpdateConsumer(ctx, spec.jetStreamName, jetstream.ConsumerConfig{
		Durable:    c.consumerGroup,
		AckPolicy:  jetstream.AckExplicitPolicy,
		AckWait:    30 * time.Second,
		MaxDeliver: 5,
	})
	if err != nil {
		return nil, Transient(fmt.Errorf("ensure consumer %s: %w", c.consumerGroup, err))
	}
	batch, err := consumer.Fetch(limit, jetstream.FetchMaxWait(c.receiveTimeout))
	if err != nil {
		if errors.Is(err, nats.ErrTimeout) || errors.Is(err, jetstream.ErrNoMessages) {
			return nil, nil
		}
		return nil, Transient(fmt.Errorf("fetch messages: %w", err))
	}

	messages := make([]Message, 0, limit)
	for msg := range batch.Messages() {
		var envelope channel.Envelope
		if err := json.Unmarshal(msg.Data(), &envelope); err != nil {
			return nil, fmt.Errorf("decode envelope: %w", err)
		}
		if err := envelope.Validate(c.stream); err != nil {
			return nil, err
		}
		jetStreamMsg := msg
		messages = append(messages, Message{
			Stream:   c.stream,
			Envelope: envelope,
		}.WithAck(func(ctx context.Context) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := jetStreamMsg.Ack(); err != nil {
				return fmt.Errorf("ack message: %w", err)
			}
			return nil
		}))
	}
	if err := batch.Error(); err != nil && !errors.Is(err, nats.ErrTimeout) && !errors.Is(err, jetstream.ErrNoMessages) {
		return nil, Transient(fmt.Errorf("receive batch: %w", err))
	}
	return messages, nil
}

func (c *NATSConsumer) Close(ctx context.Context) error {
	if c == nil || c.conn == nil {
		return nil
	}
	return closeNATS(ctx, c.conn)
}

func connectNATS(opts NATSOptions) (*nats.Conn, jetstream.JetStream, int, error) {
	url := strings.TrimSpace(opts.URL)
	if url == "" {
		return nil, nil, 0, fmt.Errorf("CROSS_SERVICE_NATS_URL is required when CROSS_SERVICE_BROKER=%q", AdapterNATS)
	}
	natsOpts := []nats.Option{}
	if strings.TrimSpace(opts.Name) != "" {
		natsOpts = append(natsOpts, nats.Name(opts.Name))
	}
	conn, err := nats.Connect(url, natsOpts...)
	if err != nil {
		return nil, nil, 0, Transient(fmt.Errorf("connect nats: %w", err))
	}
	js, err := jetstream.New(conn)
	if err != nil {
		conn.Close()
		return nil, nil, 0, fmt.Errorf("create jetstream client: %w", err)
	}
	partitionCount := opts.PartitionCount
	if partitionCount <= 0 {
		partitionCount = 4
	}
	return conn, js, partitionCount, nil
}

func closeNATS(ctx context.Context, conn *nats.Conn) error {
	done := make(chan error, 1)
	go func() {
		done <- conn.Drain()
		conn.Close()
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		conn.Close()
		return ctx.Err()
	}
}

func ensureNATSStream(ctx context.Context, js jetstream.JetStream, spec natsStreamSpec) error {
	_, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:        spec.jetStreamName,
		Description: "Cross-service stream for " + spec.logicalName,
		Subjects:    []string{spec.subjectPattern},
		MaxAge:      24 * time.Hour,
		MaxMsgSize:  1048576,
		Storage:     jetstream.FileStorage,
		Replicas:    1,
		Discard:     jetstream.DiscardOld,
	})
	if err != nil {
		return fmt.Errorf("ensure stream %s: %w", spec.jetStreamName, err)
	}
	return nil
}

func natsSpecForStream(stream string) (natsStreamSpec, error) {
	switch stream {
	case channel.StreamAccountVideoCommands:
		return natsStreamSpec{
			logicalName:    channel.StreamAccountVideoCommands,
			jetStreamName:  accountVideoCommandsStreamName,
			subjectPattern: "account.video.commands.*",
			subjectPrefix:  "account.video.commands",
		}, nil
	case channel.StreamVideoAccountEvents:
		return natsStreamSpec{
			logicalName:    channel.StreamVideoAccountEvents,
			jetStreamName:  videoAccountEventsStreamName,
			subjectPattern: "video.account.events.*",
			subjectPrefix:  "video.account.events",
		}, nil
	default:
		return natsStreamSpec{}, fmt.Errorf("unsupported nats stream %q", stream)
	}
}

func (s natsStreamSpec) subjectForPartition(partitionCount int, partitionKey string) string {
	if partitionCount <= 0 {
		partitionCount = 4
	}
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(partitionKey))
	return fmt.Sprintf("%s.%d", s.subjectPrefix, int(hash.Sum32()%uint32(partitionCount)))
}
