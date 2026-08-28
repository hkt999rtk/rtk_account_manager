package readiness

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOptionsFromEnvReadsSmokeSettings(t *testing.T) {
	t.Setenv("ACCOUNT_MANAGER_BASE_URL", "https://account.internal")
	t.Setenv("ACCOUNT_MANAGER_VERSION", "build-1")
	t.Setenv("READINESS_SMOKE_EMAIL", "owner@example.com")
	t.Setenv("READINESS_SMOKE_PASSWORD", "password")
	t.Setenv("READINESS_SMOKE_ORG_ID", "org-1")
	t.Setenv("READINESS_SMOKE_DEVICE_ID", "device-1")
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("READINESS_MIGRATIONS_DIR", "/migrations")
	t.Setenv("CROSS_SERVICE_BROKER", "azure_eventhubs")
	t.Setenv("ACCOUNT_VIDEO_COMMANDS_STREAM", "commands")
	t.Setenv("VIDEO_ACCOUNT_EVENTS_STREAM", "events")

	opts := OptionsFromEnv()
	if opts.BaseURL != "https://account.internal" ||
		opts.ServiceVersion != "build-1" ||
		opts.Email != "owner@example.com" ||
		opts.Password != "password" ||
		opts.OrganizationID != "org-1" ||
		opts.DeviceID != "device-1" ||
		opts.DatabaseURL != "postgres://example" ||
		opts.MigrationsDir != "/migrations" ||
		opts.Broker != "azure_eventhubs" ||
		opts.CommandStream != "commands" ||
		opts.EventStream != "events" {
		t.Fatalf("unexpected options from env: %+v", opts)
	}
}

func TestRunDryRunProducesRedactedSkips(t *testing.T) {
	report, err := Run(context.Background(), Options{
		BaseURL:     "http://account-manager.internal",
		Email:       "owner@example.com",
		Password:    "secret-password",
		DatabaseURL: "postgres://rtk:secret@localhost:5432/rtk_account_manager?sslmode=disable",
		DryRun:      true,
		Now:         fixedNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.GeneratedAt != fixedNow() {
		t.Fatalf("unexpected generated_at: %s", report.GeneratedAt)
	}
	if report.Inputs.Email != "o***@example.com" {
		t.Fatalf("expected redacted email, got %q", report.Inputs.Email)
	}
	if report.Inputs.DatabaseURL != "postgres://%5Bredacted%5D@[redacted]/rtk_account_manager?sslmode=disable" {
		t.Fatalf("expected redacted database URL, got %q", report.Inputs.DatabaseURL)
	}
	assertCheck(t, report, "configuration", StatusPass)
	assertCheck(t, report, "service_version", StatusSkip)
	assertCheck(t, report, "cross_service_channel_optional", StatusSkip)
	if report.Summary.Pass != 1 || report.Summary.Skip != 2 || report.Summary.Fail != 0 {
		t.Fatalf("unexpected summary: %+v", report.Summary)
	}

	var out bytes.Buffer
	if err := WriteReport(&out, report); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(out.Bytes()) {
		t.Fatalf("expected JSON report, got %q", out.String())
	}
}

func TestRunSmokeReadsHealthAuthOrgDeviceAndProvisioning(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/health":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/v1/auth/login":
			var req map[string]string
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode login request: %v", err)
			}
			if req["password"] == "" {
				t.Fatal("expected password to be sent only to login endpoint")
			}
			_, _ = w.Write([]byte(`{"user":{"id":"user-1","email":"owner@example.com"},"tokens":{"access_token":"access-token"}}`))
		case "/v1/orgs/org-1":
			requireBearer(t, r)
			_, _ = w.Write([]byte(`{"organization":{"id":"org-1","name":"Private Cloud","role":"owner"}}`))
		case "/v1/orgs/org-1/devices/device-1":
			requireBearer(t, r)
			_, _ = w.Write([]byte(`{"device":{"id":"device-1","name":"Camera","category":"ip_camera","status":"offline","metadata":{}}}`))
		case "/v1/orgs/org-1/devices/device-1/provisioning":
			requireBearer(t, r)
			_, _ = w.Write([]byte(`{
				"operation":{"operation_id":"op-1","operation_type":"provision","status":"succeeded"},
				"readiness":{"state":"ready","product_state":"activated","sources":{"device_enabled":true}},
				"video_metadata":{"video_cloud_devid":"vc-1"}
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	report, err := Run(context.Background(), Options{
		BaseURL:        server.URL,
		ServiceVersion: "2026.05.07+abc123",
		Email:          "owner@example.com",
		Password:       "secret-password",
		OrganizationID: "org-1",
		DeviceID:       "device-1",
		Broker:         "azure_eventhubs",
		CommandStream:  "account.video.commands",
		EventStream:    "video.account.events",
		Now:            fixedNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"service_version",
		"health",
		"cross_service_channel_optional",
		"auth_login",
		"organization_smoke",
		"device_smoke",
		"provisioning_readiness",
	} {
		assertCheck(t, report, name, StatusPass)
	}
	assertCheck(t, report, "migration_status", StatusSkip)
	if report.Summary.Fail != 0 {
		t.Fatalf("unexpected failed checks: %+v", report.Summary)
	}
}

func TestRunSmokeDiscoversOrgAndDeviceFromLists(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/health":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/v1/auth/login":
			_, _ = w.Write([]byte(`{"user":{"id":"user-1","email":"owner@example.com"},"tokens":{"access_token":"access-token"}}`))
		case "/v1/orgs":
			requireBearer(t, r)
			_, _ = w.Write([]byte(`{"organizations":[{"id":"org-1","name":"Org","role":"owner"}]}`))
		case "/v1/orgs/org-1/devices":
			requireBearer(t, r)
			_, _ = w.Write([]byte(`{"devices":[{"id":"device-1","name":"Camera","category":"ip_camera","status":"offline","metadata":{}}]}`))
		case "/v1/orgs/org-1/devices/device-1/provisioning":
			requireBearer(t, r)
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"not_provisioned"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	report, err := Run(context.Background(), Options{
		BaseURL:  server.URL,
		Email:    "owner@example.com",
		Password: "secret-password",
		Now:      fixedNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertCheck(t, report, "auth_login", StatusPass)
	assertCheck(t, report, "organization_smoke", StatusPass)
	assertCheck(t, report, "device_smoke", StatusPass)
	assertCheck(t, report, "provisioning_readiness", StatusSkip)
}

func TestRunSmokeSkipsWhenNoOrganizationsAreVisible(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/health":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/v1/auth/login":
			_, _ = w.Write([]byte(`{"user":{"id":"user-1","email":"owner@example.com"},"tokens":{"access_token":"access-token"}}`))
		case "/v1/orgs":
			requireBearer(t, r)
			_, _ = w.Write([]byte(`{"organizations":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	report, err := Run(context.Background(), Options{
		BaseURL:  server.URL,
		Email:    "owner@example.com",
		Password: "secret-password",
		Now:      fixedNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertCheck(t, report, "organization_smoke", StatusSkip)
	assertCheck(t, report, "device_smoke", StatusSkip)
	assertCheck(t, report, "provisioning_readiness", StatusSkip)
}

func TestRunSmokeFailsLoginAndSkipsDependentChecks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/health":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/v1/auth/login":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_credentials"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	report, err := Run(context.Background(), Options{
		BaseURL:  server.URL,
		Email:    "owner@example.com",
		Password: "wrong-password",
		Now:      fixedNow,
	})
	if err == nil {
		t.Fatal("expected failed login to fail smoke")
	}
	assertCheck(t, report, "auth_login", StatusFail)
	assertCheck(t, report, "organization_smoke", StatusSkip)
	assertCheck(t, report, "device_smoke", StatusSkip)
}

func TestRunSmokeFailsOnMissingMigration(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "001_init.sql"), []byte("-- migration"), 0o600); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	report, err := Run(context.Background(), Options{
		BaseURL:       server.URL,
		DatabaseURL:   "postgres://invalid-host.invalid/rtk",
		MigrationsDir: dir,
		Now:           fixedNow,
	})
	if err == nil {
		t.Fatal("expected migration check failure")
	}
	assertCheck(t, report, "migration_status", StatusFail)
}

func fixedNow() time.Time {
	return time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
}

func requireBearer(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer access-token" {
		t.Fatalf("unexpected authorization header: %q", got)
	}
}

func assertCheck(t *testing.T, report Report, name string, status Status) {
	t.Helper()
	for _, check := range report.Checks {
		if check.Name == name {
			if check.Status != status {
				t.Fatalf("expected %s to be %s, got %s (%s)", name, status, check.Status, check.Summary)
			}
			return
		}
	}
	t.Fatalf("missing check %q in %+v", name, report.Checks)
}
