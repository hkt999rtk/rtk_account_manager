package main

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"rtk_account_manager/internal/model"
)

func TestParseOutboxStatuses(t *testing.T) {
	got, err := parseOutboxStatuses(" pending, published, retrying, dead_lettered ")
	if err != nil {
		t.Fatalf("parse statuses: %v", err)
	}
	want := []model.DeviceMessageOutboxStatus{
		model.DeviceMessageOutboxStatusPending,
		model.DeviceMessageOutboxStatusPublished,
		model.DeviceMessageOutboxStatusRetrying,
		model.DeviceMessageOutboxStatusDeadLettered,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("statuses = %#v, want %#v", got, want)
	}
	if got := outboxStatusStrings(got); !reflect.DeepEqual(got, []string{"pending", "published", "retrying", "dead_lettered"}) {
		t.Fatalf("status strings = %#v", got)
	}

	for _, test := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "empty", raw: " , ", want: "at least one outbox status is required"},
		{name: "unsupported", raw: "pending,unknown", want: `unsupported outbox status "unknown"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseOutboxStatuses(test.raw)
			if err == nil || err.Error() != test.want {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestParseInboxStatuses(t *testing.T) {
	got, err := parseInboxStatuses(" processed, failed, retrying, dead_lettered ")
	if err != nil {
		t.Fatalf("parse statuses: %v", err)
	}
	want := []model.DeviceMessageInboxStatus{
		model.DeviceMessageInboxStatusProcessed,
		model.DeviceMessageInboxStatusFailed,
		model.DeviceMessageInboxStatusRetrying,
		model.DeviceMessageInboxStatusDeadLettered,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("statuses = %#v, want %#v", got, want)
	}
	if got := inboxStatusStrings(got); !reflect.DeepEqual(got, []string{"processed", "failed", "retrying", "dead_lettered"}) {
		t.Fatalf("status strings = %#v", got)
	}

	for _, test := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "empty", raw: "", want: "at least one inbox status is required"},
		{name: "unsupported", raw: "processed,unknown", want: `unsupported inbox status "unknown"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseInboxStatuses(test.raw)
			if err == nil || err.Error() != test.want {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLifecycleAdminOutputHelpers(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run(nil, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("run without arguments error = %v", err)
	}

	if got := splitCSV(" first, , second ,, third "); !reflect.DeepEqual(got, []string{"first", "second", "third"}) {
		t.Fatalf("splitCSV = %#v", got)
	}

	var output bytes.Buffer
	if err := writeJSON(&output, map[string]string{"status": "ok"}); err != nil {
		t.Fatalf("write JSON: %v", err)
	}
	if got, want := output.String(), "{\n  \"status\": \"ok\"\n}\n"; got != want {
		t.Fatalf("JSON = %q, want %q", got, want)
	}

	marshalErr := writeJSON(&output, make(chan int))
	if marshalErr == nil {
		t.Fatal("expected unsupported value to fail JSON encoding")
	}

	writeErr := errors.New("write failed")
	if err := writeJSON(errorWriter{err: writeErr}, map[string]string{"status": "ok"}); !errors.Is(err, writeErr) {
		t.Fatalf("write error = %v, want %v", err, writeErr)
	}

	usage := usageText()
	if !strings.Contains(usage, "lifecycle-admin outbox list") || !strings.Contains(usage, "lifecycle-admin inbox requeue") {
		t.Fatalf("unexpected usage text: %q", usage)
	}
	if got := (usageError{message: "bad usage"}).Error(); got != "bad usage" {
		t.Fatalf("usage error = %q", got)
	}
}

type errorWriter struct {
	err error
}

func (w errorWriter) Write([]byte) (int, error) {
	return 0, w.err
}
