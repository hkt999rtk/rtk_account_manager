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

	"rtk_account_manager/internal/database"
	"rtk_account_manager/internal/testutil"
)

func TestUtilityHelpersAndEnvFallbacks(t *testing.T) {
	t.Setenv("ACCOUNT_MANAGER_BASE_URL", "http://env.example")
	t.Setenv("ACCOUNT_MANAGER_VERSION", "2026.05.07+abc123")
	t.Setenv("READINESS_SMOKE_EMAIL", "owner@example.com")
	t.Setenv("READINESS_SMOKE_PASSWORD", "secret")
	t.Setenv("READINESS_SMOKE_ORG_ID", "org-1")
	t.Setenv("READINESS_SMOKE_DEVICE_ID", "device-1")
	t.Setenv("DATABASE_URL", "postgres://rtk:secret@localhost:5432/rtk_account_manager?sslmode=disable")
	t.Setenv("READINESS_MIGRATIONS_DIR", "schema")
	t.Setenv("SMTP_HOST", "smtp.internal")
	t.Setenv("SMTP_FROM", "noreply@example.com")
	t.Setenv("CROSS_SERVICE_BROKER", "azure_eventhubs")
	t.Setenv("ACCOUNT_VIDEO_COMMANDS_STREAM", "account.video.commands")
	t.Setenv("VIDEO_ACCOUNT_EVENTS_STREAM", "video.account.events")

	opts := OptionsFromEnv()
	if opts.BaseURL != "http://env.example" {
		t.Fatalf("unexpected base url: %q", opts.BaseURL)
	}
	if opts.ServiceVersion != "2026.05.07+abc123" || opts.Email != "owner@example.com" || opts.Password != "secret" {
		t.Fatalf("unexpected env-derived options: %+v", opts)
	}
	if opts.MigrationsDir != "schema" || opts.SMTPHost != "smtp.internal" || opts.Broker != "azure_eventhubs" {
		t.Fatalf("unexpected optional env-derived options: %+v", opts)
	}

	defaulted := Options{BaseURL: " ", MigrationsDir: " "}.withDefaults()
	if defaulted.BaseURL != "http://localhost:8080" {
		t.Fatalf("expected default base url, got %q", defaulted.BaseURL)
	}
	if defaulted.MigrationsDir != "migrations" {
		t.Fatalf("expected default migrations dir, got %q", defaulted.MigrationsDir)
	}
	if defaulted.Now == nil {
		t.Fatal("expected withDefaults to install a clock")
	}

	client := &http.Client{Timeout: time.Second}
	if got := (Options{HTTPClient: client}).httpClient(); got != client {
		t.Fatalf("expected custom client passthrough, got %p", got)
	}
	if got := (Options{}).httpClient(); got == nil {
		t.Fatal("expected default http client")
	}

	if got := latest(nil); got != "" {
		t.Fatalf("expected empty latest, got %q", got)
	}
	if got := latest([]string{"001_init.sql", "002_next.sql"}); got != "002_next.sql" {
		t.Fatalf("unexpected latest value: %q", got)
	}
	if got := valueOrDefault("  ", "fallback"); got != "fallback" {
		t.Fatalf("expected fallback value, got %q", got)
	}
	if got := valueOrDefault("configured", "fallback"); got != "configured" {
		t.Fatalf("unexpected configured value: %q", got)
	}

	if got := getenv("READINESS_TEST_MISSING", "fallback"); got != "fallback" {
		t.Fatalf("expected fallback getenv value, got %q", got)
	}

	if got := versionStatus(" "); got != StatusSkip {
		t.Fatalf("expected blank version to skip, got %s", got)
	}
	if got := versionStatus("2026.05.07+abc123"); got != StatusPass {
		t.Fatalf("expected populated version to pass, got %s", got)
	}
	if got := versionSummary(" "); got != "service version was not provided" {
		t.Fatalf("unexpected blank version summary: %q", got)
	}
	if got := versionSummary("2026.05.07+abc123"); got != "service version was recorded" {
		t.Fatalf("unexpected version summary: %q", got)
	}
	if got := serviceVersionState(" "); got != "not_provided" {
		t.Fatalf("unexpected blank service version state: %q", got)
	}
	if got := serviceVersionState("2026.05.07+abc123"); got != "provided" {
		t.Fatalf("unexpected populated service version state: %q", got)
	}

	if got := redactEmail(""); got != "" {
		t.Fatalf("expected empty email to remain empty, got %q", got)
	}
	if got := redactEmail("bad-address"); got != "[redacted]" {
		t.Fatalf("expected invalid address to redact, got %q", got)
	}
	if got := redactEmail("a@example.com"); got != "*@example.com" {
		t.Fatalf("unexpected one-letter redaction: %q", got)
	}
	if got := redactEmail("owner@example.com"); got != "o***@example.com" {
		t.Fatalf("unexpected redaction: %q", got)
	}

	if got := redactDatabaseURL(""); got != "" {
		t.Fatalf("expected empty db url to remain empty, got %q", got)
	}
	if got := redactDatabaseURL("://bad"); got != "[redacted]" {
		t.Fatalf("expected parse failure to redact, got %q", got)
	}
	if got := redactDatabaseURL("postgres://rtk:secret@localhost:5432/rtk_account_manager?sslmode=disable"); got != "postgres://%5Bredacted%5D@[redacted]/rtk_account_manager?sslmode=disable" {
		t.Fatalf("unexpected redacted database url: %q", got)
	}

	report := Report{
		GeneratedAt: fixedNow(),
		Target: TargetEvidence{
			BaseURL:        "http://env.example",
			ServiceVersion: "2026.05.07+abc123",
		},
	}
	report.add(check("alpha", StatusPass, "ok", nil))
	report.addAll(check("beta", StatusSkip, "skip", nil), check("gamma", StatusFail, "fail", nil))
	report.finish()
	if report.Summary.Pass != 1 || report.Summary.Skip != 1 || report.Summary.Fail != 1 {
		t.Fatalf("unexpected summary after finish: %+v", report.Summary)
	}

	var buf bytes.Buffer
	if err := WriteReport(&buf, report); err != nil {
		t.Fatal(err)
	}
	var decoded Report
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("expected write report output to decode: %v", err)
	}
	if decoded.Target.BaseURL != "http://env.example" || decoded.Target.ServiceVersion != "2026.05.07+abc123" {
		t.Fatalf("unexpected decoded report: %+v", decoded.Target)
	}
}

func TestReadinessSkipAndFailurePaths(t *testing.T) {
	healthServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/health":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"degraded"}`))
		case "/v1/auth/login":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		case "/v1/orgs/org-1":
			http.NotFound(w, r)
		case "/v1/orgs/org-1/devices/device-1":
			http.NotFound(w, r)
		case "/v1/orgs/org-1/devices/device-1/provisioning":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"operation":{},"readiness":{"state":"","product_state":"","sources":{}},"video_metadata":{}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer healthServer.Close()

	opts := Options{BaseURL: healthServer.URL, Now: fixedNow}
	if got := runHealthCheck(context.Background(), opts); got.Status != StatusFail {
		t.Fatalf("expected failing health check, got %+v", got)
	}

	_, _, got := runAuthAndOrgChecks(context.Background(), opts)
	if len(got) != 2 || got[0].Status != StatusSkip || got[1].Status != StatusSkip {
		t.Fatalf("expected auth/org skip without credentials, got %+v", got)
	}

	authToken, orgID, got := runAuthAndOrgChecks(context.Background(), Options{
		BaseURL:  healthServer.URL,
		Email:    "owner@example.com",
		Password: "secret",
		Now:      fixedNow,
	})
	if authToken != "" || orgID != "" || len(got) != 2 || got[0].Status != StatusFail || got[1].Status != StatusSkip {
		t.Fatalf("expected login failure path, got token=%q org=%q checks=%+v", authToken, orgID, got)
	}

	if got := runDeviceChecks(context.Background(), opts, "", ""); len(got) != 2 || got[0].Status != StatusSkip || got[1].Status != StatusSkip {
		t.Fatalf("expected device skip without auth or org, got %+v", got)
	}

	if got := runProvisioningReadiness(context.Background(), Options{BaseURL: healthServer.URL, Now: fixedNow}, "access-token", "org-1", "device-1"); len(got) != 1 || got[0].Status != StatusSkip {
		t.Fatalf("expected missing provisioning evidence to skip, got %+v", got)
	}
}

func TestReadinessFallbackLookupsAndSuccessPaths(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/health":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/v1/auth/login":
			_, _ = w.Write([]byte(`{"user":{"id":"user-1","email":"owner@example.com"},"tokens":{"access_token":"access-token"}}`))
		case "/v1/orgs":
			requireBearerRequest(t, r)
			_, _ = w.Write([]byte(`{"organizations":[{"id":"org-1","name":"Private Cloud","role":"owner"}]}`))
		case "/v1/orgs/org-1":
			requireBearerRequest(t, r)
			_, _ = w.Write([]byte(`{"organization":{"id":"org-1","name":"Private Cloud","role":"owner"}}`))
		case "/v1/orgs/org-1/devices":
			requireBearerRequest(t, r)
			_, _ = w.Write([]byte(`{"devices":[{"id":"device-1","name":"Camera","category":"ip_camera","status":"offline","metadata":{}}]}`))
		case "/v1/orgs/org-1/devices/device-1":
			requireBearerRequest(t, r)
			_, _ = w.Write([]byte(`{"device":{"id":"device-1","name":"Camera","category":"ip_camera","status":"offline","metadata":{}}}`))
		case "/v1/orgs/org-1/devices/device-1/provisioning":
			requireBearerRequest(t, r)
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
		Password:       "secret",
		SMTPHost:       "smtp.internal",
		SMTPFrom:       "noreply@example.com",
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
		"smtp_optional",
		"cross_service_channel_optional",
		"auth_login",
		"organization_smoke",
		"device_smoke",
		"provisioning_readiness",
	} {
		assertCheck(t, report, name, StatusPass)
	}

	authToken, orgID, authChecks := runAuthAndOrgChecks(context.Background(), Options{
		BaseURL:  server.URL,
		Email:    "owner@example.com",
		Password: "secret",
		Now:      fixedNow,
	})
	if authToken != "access-token" || orgID != "org-1" || len(authChecks) != 2 || authChecks[1].Status != StatusPass {
		t.Fatalf("expected organization list fallback to pass, got token=%q org=%q checks=%+v", authToken, orgID, authChecks)
	}

	deviceChecks := runDeviceChecks(context.Background(), Options{
		BaseURL: server.URL,
		Now:     fixedNow,
	}, authToken, orgID)
	if len(deviceChecks) != 2 || deviceChecks[0].Status != StatusPass || deviceChecks[1].Status != StatusPass {
		t.Fatalf("expected device list fallback to pass, got %+v", deviceChecks)
	}

	if got := runProvisioningReadiness(context.Background(), Options{BaseURL: server.URL, Now: fixedNow}, authToken, orgID, "device-1"); len(got) != 1 || got[0].Status != StatusPass {
		t.Fatalf("expected provisioning success, got %+v", got)
	}
}

func TestRunMigrationCheckWithAppliedMigrations(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL is required for migration coverage")
	}

	ctx := context.Background()
	db, err := database.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)

	testutil.LockIntegrationDatabase(t, db)

	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}

	check := runMigrationCheck(context.Background(), Options{
		DatabaseURL:   dbURL,
		MigrationsDir: filepath.Join("..", "..", "migrations"),
		Now:           fixedNow,
	})
	if check.Status != StatusPass {
		t.Fatalf("expected migration check to pass, got %+v", check)
	}
}

func requireBearerRequest(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer access-token" {
		t.Fatalf("unexpected authorization header: %q", got)
	}
}
