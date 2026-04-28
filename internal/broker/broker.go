package broker

import (
	"context"
	"errors"
	"fmt"
	"io"

	"rtk_account_manager/internal/channel"
)

const AdapterLog = "log"

type Publisher interface {
	Publish(ctx context.Context, stream string, envelope channel.Envelope) error
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

func NewPublisher(kind string, writer io.Writer) (Publisher, error) {
	switch kind {
	case "", AdapterLog:
		return NewLogPublisher(writer), nil
	default:
		return nil, fmt.Errorf("unsupported cross-service broker %q", kind)
	}
}
