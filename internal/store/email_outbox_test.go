package store

import (
	"errors"
	"strings"
	"testing"
)

type failingEmailOutboxRow struct{}

func (failingEmailOutboxRow) Scan(...any) error {
	return errors.New("scan failed")
}

func TestScanEmailOutboxWrapsScanError(t *testing.T) {
	_, err := scanEmailOutbox(failingEmailOutboxRow{})
	if err == nil || !strings.Contains(err.Error(), "scan email outbox: scan failed") {
		t.Fatalf("scan error = %v", err)
	}
}
