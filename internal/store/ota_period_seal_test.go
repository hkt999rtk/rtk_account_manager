package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"slices"
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

func TestOTAPeriodGrantSealRejectsNonMonotonicGrantHistory(t *testing.T) {
	org := "11111111-1111-4111-8111-111111111111"
	product := "22222222-2222-4222-8222-222222222222"
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	base := []otaGrantHistoryRow{
		{ProductID: product, Revision: 1, CreatedAt: start.Add(time.Hour), Options: []string{"ota"}, SnapshotSHA256: strings.Repeat("a", 64)},
		{ProductID: product, Revision: 2, CreatedAt: start.Add(2 * time.Hour), Options: []string{"mqtt"}, SnapshotSHA256: strings.Repeat("b", 64)},
	}
	for _, tc := range []struct {
		name   string
		change func([]otaGrantHistoryRow)
	}{
		{"duplicate revision", func(rows []otaGrantHistoryRow) { rows[1].Revision = 1 }},
		{"backdated revision", func(rows []otaGrantHistoryRow) { rows[1].CreatedAt = start }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			history := slices.Clone(base)
			tc.change(history)
			if _, err := newOTAPeriodGrantSeal(org, start, start.AddDate(0, 1, 0), history); !errors.Is(err, ErrOTAPeriodSealInvalid) {
				t.Fatalf("non-monotonic grant history was sealed: %v", err)
			}
		})
	}
}

func TestOTAPeriodGrantSealCanonicalizesGrantRowOrder(t *testing.T) {
	org := "11111111-1111-4111-8111-111111111111"
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	rows := []otaGrantHistoryRow{
		{ProductID: "22222222-2222-4222-8222-222222222222", Revision: 1, CreatedAt: start, Options: []string{"ota"}, SnapshotSHA256: strings.Repeat("a", 64)},
		{ProductID: "33333333-3333-4333-8333-333333333333", Revision: 1, CreatedAt: start, Options: []string{"mqtt"}, SnapshotSHA256: strings.Repeat("b", 64)},
	}
	end := start.AddDate(0, 1, 0)
	ascending, err := newOTAPeriodGrantSeal(org, start, end, slices.Clone(rows))
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(rows)
	descending, err := newOTAPeriodGrantSeal(org, start, end, rows)
	if err != nil {
		t.Fatal(err)
	}
	if ascending.SealID != descending.SealID || ascending.SourceSHA256 != descending.SourceSHA256 ||
		string(ascending.SourceHighWater) != string(descending.SourceHighWater) {
		t.Fatalf("grant input order changed OTA period seal: %+v %+v", ascending, descending)
	}
}
