package database

import (
	"context"
	"strings"
	"testing"
)

func TestPreflightIdentityCorrectionRejectsNilPool(t *testing.T) {
	report, err := PreflightIdentityCorrection(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "database pool") {
		t.Fatalf("nil pool: report=%+v err=%v", report, err)
	}
	if report.Migration != identityCorrectionMigration || report.Ready || report.RolledBack {
		t.Fatalf("unexpected report: %+v", report)
	}
}
