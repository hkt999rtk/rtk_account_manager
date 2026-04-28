package broker

import (
	"bufio"
	"bytes"
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

type LogConsumer struct {
	mu        sync.Mutex
	scanner   *bufio.Scanner
	exhausted bool
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

func NewLogConsumer(reader io.Reader) *LogConsumer {
	if reader == nil {
		reader = bytes.NewReader(nil)
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	return &LogConsumer{scanner: scanner}
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

func (c *LogConsumer) Receive(ctx context.Context, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 1
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.exhausted {
		return nil, nil
	}

	messages := make([]Message, 0, limit)
	for len(messages) < limit {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !c.scanner.Scan() {
			if err := c.scanner.Err(); err != nil {
				return nil, err
			}
			c.exhausted = true
			break
		}

		var record logRecord
		if err := json.Unmarshal(c.scanner.Bytes(), &record); err != nil {
			return nil, err
		}
		messages = append(messages, Message{
			Stream:   record.Stream,
			Envelope: record.Envelope,
		})
	}

	return messages, nil
}
