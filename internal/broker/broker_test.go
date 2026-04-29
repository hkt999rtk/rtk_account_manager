package broker

import (
	"context"
	"errors"
	"testing"
)

func TestTransientHelpersExposeWrappedError(t *testing.T) {
	root := errors.New("temporary")
	err := Transient(root)
	if err == nil {
		t.Fatal("expected wrapped transient error")
	}
	if err.Error() != root.Error() {
		t.Fatalf("expected wrapped error string %q, got %q", root.Error(), err.Error())
	}
	if !errors.Is(err, root) {
		t.Fatalf("expected wrapped error to unwrap to %v", root)
	}
	if got := Transient(nil); got != nil {
		t.Fatalf("expected nil transient passthrough, got %v", got)
	}
}

func TestNewPublisherCreatesLogPublisherAndRejectsUnsupportedKinds(t *testing.T) {
	publisher, err := NewPublisher("", PublisherOptions{})
	if err != nil {
		t.Fatal(err)
	}
	logPublisher, ok := publisher.(*LogPublisher)
	if !ok {
		t.Fatalf("expected log publisher, got %T", publisher)
	}
	if err := logPublisher.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	if _, err := NewPublisher(AdapterAzureEventHubs, PublisherOptions{}); err == nil {
		t.Fatal("expected azure publisher config error")
	}
	if _, err := NewPublisher("unsupported", PublisherOptions{}); err == nil {
		t.Fatal("expected unsupported publisher error")
	}
}

func TestNewConsumerCreatesLogConsumerAndRejectsUnsupportedKinds(t *testing.T) {
	consumer, err := NewConsumer("", ConsumerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	logConsumer, ok := consumer.(*LogConsumer)
	if !ok {
		t.Fatalf("expected log consumer, got %T", consumer)
	}
	messages, err := logConsumer.Receive(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 0 {
		t.Fatalf("expected empty log consumer on nil reader, got %+v", messages)
	}
	if err := logConsumer.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	if _, err := NewConsumer(AdapterAzureEventHubs, ConsumerOptions{}); err == nil {
		t.Fatal("expected azure consumer config error")
	}
	if _, err := NewConsumer("unsupported", ConsumerOptions{}); err == nil {
		t.Fatal("expected unsupported consumer error")
	}
}
