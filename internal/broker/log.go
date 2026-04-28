package broker

import (
	"context"
	"encoding/json"
	"io"
	"sync"

	"rtk_account_manager/internal/channel"
)

type LogPublisher struct {
	mu     sync.Mutex
	writer io.Writer
}

type logRecord struct {
	Stream   string           `json:"stream"`
	Envelope channel.Envelope `json:"envelope"`
}

func NewLogPublisher(writer io.Writer) *LogPublisher {
	if writer == nil {
		writer = io.Discard
	}
	return &LogPublisher{writer: writer}
}

func (p *LogPublisher) Publish(ctx context.Context, stream string, envelope channel.Envelope) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	record, err := json.Marshal(logRecord{
		Stream:   stream,
		Envelope: envelope,
	})
	if err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if _, err := p.writer.Write(append(record, '\n')); err != nil {
		return err
	}
	return nil
}
