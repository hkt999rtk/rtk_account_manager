package store

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestOTAPeriodGrantSealIncludesHistoricalZeroUsageProducts(t *testing.T) {
	org := "11111111-1111-4111-8111-111111111111"
	p1 := "22222222-2222-4222-8222-222222222222"
	p2 := "33333333-3333-4333-8333-333333333333"
	p3 := "44444444-4444-4444-8444-444444444444"
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	at := func(day int, month time.Month) time.Time {
		return time.Date(2026, month, day, 0, 0, 0, 0, time.UTC)
	}
	history := []otaGrantHistoryRow{
		{ProductID: p3, Revision: 2, CreatedAt: start, Options: []string{"mqtt"}, SnapshotSHA256: strings.Repeat("c", 64)},
		{ProductID: p2, Revision: 1, CreatedAt: start, Options: []string{"ota", "mqtt"}, SnapshotSHA256: strings.Repeat("d", 64)},
		{ProductID: p1, Revision: 2, CreatedAt: at(10, time.September), Options: []string{"mqtt"}, SnapshotSHA256: strings.Repeat("b", 64)},
		{ProductID: p1, Revision: 1, CreatedAt: at(15, time.August), Options: []string{"mqtt", "ota"}, SnapshotSHA256: strings.Repeat("a", 64)},
		{ProductID: p3, Revision: 1, CreatedAt: at(15, time.August), Options: []string{"ota", "mqtt"}, SnapshotSHA256: strings.Repeat("e", 64)},
	}
	seal, err := newOTAPeriodGrantSeal(org, start, end, history)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(seal.ProductIDs, []string{p1, p2, p3}) {
		t.Fatalf("historical OTA Product set = %v, want all historically granted", seal.ProductIDs)
	}
	if seal.IssuerKind != "platform_grants" || len(seal.MetricCounts) != 0 ||
		seal.SealedAt != end || len(seal.SourceHighWater) == 0 {
		t.Fatalf("invalid Platform seal: %+v", seal)
	}
	replay, err := newOTAPeriodGrantSeal(org, start, end, history)
	if err != nil || seal.SealID != replay.SealID || seal.SourceSHA256 != replay.SourceSHA256 ||
		string(seal.SourceHighWater) != string(replay.SourceHighWater) {
		t.Fatalf("seal not deterministic: %+v %+v %v", seal, replay, err)
	}
	if !billingCreationUUID(seal.SealID) {
		t.Fatalf("seal ID is not a canonical UUID: %q", seal.SealID)
	}
}

func TestOTAPeriodGrantSealExplicitEmptySetAndMonthValidation(t *testing.T) {
	org := "11111111-1111-4111-8111-111111111111"
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	seal, err := newOTAPeriodGrantSeal(org, start, end, nil)
	if err != nil {
		t.Fatal(err)
	}
	empty := sha256.Sum256([]byte("[]"))
	if seal.ProductIDs == nil || len(seal.ProductIDs) != 0 ||
		seal.SourceSHA256 != hex.EncodeToString(empty[:]) {
		t.Fatalf("zero-usage seal = %+v", seal)
	}
	if _, err := newOTAPeriodGrantSeal(org, start.Add(time.Hour), end, nil); err == nil {
		t.Fatal("partial UTC month accepted")
	}
}
