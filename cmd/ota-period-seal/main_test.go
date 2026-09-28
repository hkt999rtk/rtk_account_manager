package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"rtk_account_manager/internal/store"
)

type fakeSealSource struct {
	ids      []string
	listErr  error
	buildErr map[string]error
}

func (f fakeSealSource) ListOTAPeriodSealBrandCloudIDs(context.Context, time.Time) ([]string, error) {
	return f.ids, f.listErr
}

func (f fakeSealSource) BuildOTAPeriodGrantSeal(_ context.Context, id string, start, end time.Time) (store.OTAPeriodGrantSeal, error) {
	if err := f.buildErr[id]; err != nil {
		return store.OTAPeriodGrantSeal{}, err
	}
	return store.OTAPeriodGrantSeal{OrganizationID: id, PeriodStart: start, PeriodEnd: end}, nil
}

type fakeSealSink struct {
	duplicate map[string]bool
	fail      map[string]error
}

func (f fakeSealSink) Submit(_ context.Context, seal store.OTAPeriodGrantSeal) (bool, error) {
	return f.duplicate[seal.OrganizationID], f.fail[seal.OrganizationID]
}

func TestParseOTAMonthPreviousUsesUTC(t *testing.T) {
	start, end, err := parseOTAMonth("previous", time.Date(2027, 1, 1, 7, 0, 0, 0, time.FixedZone("UTC+8", 8*3600)))
	if err != nil || !start.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) ||
		!end.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("previous UTC month = %v to %v, err %v", start, end, err)
	}
	for _, invalid := range []string{"", "2026-13", "2026-1", "2026-11-01"} {
		if _, _, err := parseOTAMonth(invalid, time.Now()); err == nil {
			t.Fatalf("accepted invalid month %q", invalid)
		}
	}
}

func TestSubmitAllBrandCloudsRetainsPartialFailureForRetry(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	ids := []string{"cloud-created", "cloud-corrupt", "cloud-duplicate", "cloud-unavailable"}
	source := fakeSealSource{ids: ids, buildErr: map[string]error{"cloud-corrupt": errors.New("corrupt grant")}}
	sink := fakeSealSink{duplicate: map[string]bool{"cloud-duplicate": true}, fail: map[string]error{"cloud-unavailable": errors.New("transport lost")}}
	var output bytes.Buffer
	err := submitAllBrandClouds(context.Background(), source, sink, start, start.AddDate(0, 1, 0), &output)
	if err == nil || !strings.Contains(err.Error(), "2 OTA Platform seals") {
		t.Fatalf("partial failure was not surfaced: %v", err)
	}
	if !strings.Contains(output.String(), "brand_clouds=4 created=1 duplicate=1 failed=2") ||
		strings.Contains(output.String(), "cloud-corrupt") || strings.Contains(output.String(), "cloud-unavailable") {
		t.Fatalf("bad batch output: %s", output.String())
	}
}

func TestSubmitAllBrandCloudsFailsClosedOnListError(t *testing.T) {
	var output bytes.Buffer
	err := submitAllBrandClouds(context.Background(), fakeSealSource{listErr: errors.New("database unavailable")}, fakeSealSink{},
		time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), &output)
	if err == nil || output.Len() != 0 {
		t.Fatalf("enumeration error was not fatal: %v, output=%s", err, output.String())
	}
}
