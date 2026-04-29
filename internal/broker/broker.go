package broker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"rtk_account_manager/internal/channel"
)

const AdapterLog = "log"
const AdapterAzureEventHubs = "azure_eventhubs"

type Publisher interface {
	Publish(ctx context.Context, stream string, envelope channel.Envelope) error
	Close(ctx context.Context) error
}

type Message struct {
	Stream   string
	Envelope channel.Envelope
	ack      func(context.Context) error
}

func (m Message) Acknowledge(ctx context.Context) error {
	if m.ack == nil {
		return nil
	}
	return m.ack(ctx)
}

func (m Message) WithAck(ack func(context.Context) error) Message {
	m.ack = ack
	return m
}

type Consumer interface {
	Receive(ctx context.Context, limit int) ([]Message, error)
	Close(ctx context.Context) error
}

type PublisherOptions struct {
	LogWriter                      io.Writer
	AzureEventHubsConnectionString string
	Stream                         string
}

type ConsumerOptions struct {
	LogReader                      io.Reader
	AzureEventHubsConnectionString string
	Stream                         string
	ConsumerGroup                  string
	ReceiveTimeout                 time.Duration
	CheckpointFile                 string
}

type publishError struct {
	err       error
	transient bool
}

func (e *publishError) Error() string {
	return e.err.Error()
}

func (e *publishError) Unwrap() error {
	return e.err
}

func (e *publishError) Transient() bool {
	return e.transient
}

func Transient(err error) error {
	if err == nil {
		return nil
	}
	return &publishError{err: err, transient: true}
}

func IsTransient(err error) bool {
	type transient interface {
		Transient() bool
	}
	var target transient
	if !errors.As(err, &target) {
		return false
	}
	return target.Transient()
}

func NewPublisher(kind string, opts PublisherOptions) (Publisher, error) {
	switch kind {
	case "", AdapterLog:
		return NewLogPublisher(opts.LogWriter), nil
	case AdapterAzureEventHubs:
		return NewAzureEventHubsPublisherFromConnectionString(opts.AzureEventHubsConnectionString, opts.Stream)
	default:
		return nil, fmt.Errorf("unsupported cross-service broker %q", kind)
	}
}

func NewConsumer(kind string, opts ConsumerOptions) (Consumer, error) {
	switch kind {
	case "", AdapterLog:
		return NewLogConsumer(opts.LogReader), nil
	case AdapterAzureEventHubs:
		return NewAzureEventHubsConsumerFromConnectionString(
			opts.AzureEventHubsConnectionString,
			opts.Stream,
			opts.ConsumerGroup,
			opts.ReceiveTimeout,
			opts.CheckpointFile,
		)
	default:
		return nil, fmt.Errorf("unsupported cross-service broker %q", kind)
	}
}
