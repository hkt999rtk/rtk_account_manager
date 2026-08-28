package readiness

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type Status string

const (
	StatusPass Status = "PASS"
	StatusFail Status = "FAIL"
	StatusSkip Status = "SKIP"
)

type Options struct {
	BaseURL                 string
	ServiceVersion          string
	Email                   string
	Password                string
	OrganizationID          string
	DeviceID                string
	DatabaseURL             string
	MigrationsDir           string
	SendMailHTTPBaseURL     string
	SendMailHTTPBearerToken string
	Broker                  string
	CommandStream           string
	EventStream             string
	DryRun                  bool
	Now                     func() time.Time
	HTTPClient              *http.Client
}

type Report struct {
	GeneratedAt time.Time       `json:"generated_at"`
	Target      TargetEvidence  `json:"target"`
	Inputs      InputEvidence   `json:"inputs"`
	Checks      []CheckEvidence `json:"checks"`
	Summary     SummaryEvidence `json:"summary"`
}

type TargetEvidence struct {
	BaseURL        string `json:"base_url"`
	ServiceVersion string `json:"service_version,omitempty"`
}

type InputEvidence struct {
	Email          string `json:"email,omitempty"`
	OrganizationID string `json:"organization_id,omitempty"`
	DeviceID       string `json:"device_id,omitempty"`
	DatabaseURL    string `json:"database_url,omitempty"`
}

type SummaryEvidence struct {
	Pass int `json:"pass"`
	Fail int `json:"fail"`
	Skip int `json:"skip"`
}

type CheckEvidence struct {
	Name    string         `json:"name"`
	Status  Status         `json:"status"`
	Summary string         `json:"summary"`
	Details map[string]any `json:"details,omitempty"`
}

type loginResponse struct {
	User struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	} `json:"user"`
	Tokens struct {
		AccessToken string `json:"access_token"`
	} `json:"tokens"`
}

type orgListResponse struct {
	Organizations []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Role string `json:"role"`
	} `json:"organizations"`
}

type deviceListResponse struct {
	Devices []struct {
		ID       string         `json:"id"`
		Name     string         `json:"name"`
		Category string         `json:"category"`
		Status   string         `json:"status"`
		Metadata map[string]any `json:"metadata"`
	} `json:"devices"`
}

type provisioningResponse struct {
	Operation struct {
		OperationID   string `json:"operation_id"`
		OperationType string `json:"operation_type"`
		Status        string `json:"status"`
	} `json:"operation"`
	Readiness struct {
		State        string         `json:"state"`
		ProductState string         `json:"product_state"`
		Sources      map[string]any `json:"sources"`
	} `json:"readiness"`
	VideoMetadata map[string]any `json:"video_metadata"`
}

func OptionsFromEnv() Options {
	return Options{
		BaseURL:                 getenv("ACCOUNT_MANAGER_BASE_URL", "http://localhost:8080"),
		ServiceVersion:          os.Getenv("ACCOUNT_MANAGER_VERSION"),
		Email:                   os.Getenv("READINESS_SMOKE_EMAIL"),
		Password:                os.Getenv("READINESS_SMOKE_PASSWORD"),
		OrganizationID:          os.Getenv("READINESS_SMOKE_ORG_ID"),
		DeviceID:                os.Getenv("READINESS_SMOKE_DEVICE_ID"),
		DatabaseURL:             os.Getenv("DATABASE_URL"),
		MigrationsDir:           getenv("READINESS_MIGRATIONS_DIR", "migrations"),
		SendMailHTTPBaseURL:     os.Getenv("SENDMAIL_HTTP_BASE_URL"),
		SendMailHTTPBearerToken: os.Getenv("SENDMAIL_HTTP_BEARER_TOKEN"),
		Broker:                  os.Getenv("CROSS_SERVICE_BROKER"),
		CommandStream:           os.Getenv("ACCOUNT_VIDEO_COMMANDS_STREAM"),
		EventStream:             os.Getenv("VIDEO_ACCOUNT_EVENTS_STREAM"),
	}
}

func Run(ctx context.Context, opts Options) (Report, error) {
	opts = opts.withDefaults()
	report := Report{
		GeneratedAt: opts.now(),
		Target: TargetEvidence{
			BaseURL:        strings.TrimRight(opts.BaseURL, "/"),
			ServiceVersion: opts.ServiceVersion,
		},
		Inputs: InputEvidence{
			Email:          redactEmail(opts.Email),
			OrganizationID: opts.OrganizationID,
			DeviceID:       opts.DeviceID,
			DatabaseURL:    redactDatabaseURL(opts.DatabaseURL),
		},
	}

	if opts.DryRun {
		report.add(check("configuration", StatusPass, "dry-run configuration parsed", map[string]any{
			"base_url":        report.Target.BaseURL,
			"service_version": serviceVersionState(opts.ServiceVersion),
			"has_credentials": strings.TrimSpace(opts.Email) != "" && strings.TrimSpace(opts.Password) != "",
			"has_database":    strings.TrimSpace(opts.DatabaseURL) != "",
		}))
		report.add(check("service_version", versionStatus(opts.ServiceVersion), versionSummary(opts.ServiceVersion), nil))
		report.addAll(optionalChecks(opts)...)
		report.finish()
		return report, nil
	}

	report.add(check("service_version", versionStatus(opts.ServiceVersion), versionSummary(opts.ServiceVersion), nil))
	report.add(runHealthCheck(ctx, opts))
	report.add(runMigrationCheck(ctx, opts))
	report.addAll(optionalChecks(opts)...)

	accessToken, orgID, authChecks := runAuthAndOrgChecks(ctx, opts)
	for _, c := range authChecks {
		report.add(c)
	}
	for _, c := range runDeviceChecks(ctx, opts, accessToken, orgID) {
		report.add(c)
	}

	report.finish()
	if report.Summary.Fail > 0 {
		return report, fmt.Errorf("readiness smoke completed with %d failed check(s)", report.Summary.Fail)
	}
	return report, nil
}

func WriteReport(w io.Writer, report Report) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func runHealthCheck(ctx context.Context, opts Options) CheckEvidence {
	var body map[string]any
	status, err := doJSON(ctx, opts, http.MethodGet, "/v1/health", "", nil, &body)
	if err != nil {
		return check("health", StatusFail, err.Error(), nil)
	}
	if status != http.StatusOK {
		return check("health", StatusFail, fmt.Sprintf("unexpected HTTP status %d", status), map[string]any{"http_status": status})
	}
	return check("health", StatusPass, "service health endpoint returned ok", map[string]any{
		"http_status": status,
		"status":      body["status"],
	})
}

func runMigrationCheck(ctx context.Context, opts Options) CheckEvidence {
	if strings.TrimSpace(opts.DatabaseURL) == "" {
		return check("migration_status", StatusSkip, "DATABASE_URL is not configured", nil)
	}
	expected, err := migrationFiles(opts.MigrationsDir)
	if err != nil {
		return check("migration_status", StatusFail, err.Error(), nil)
	}
	db, err := sql.Open("pgx", opts.DatabaseURL)
	if err != nil {
		return check("migration_status", StatusFail, err.Error(), nil)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		return check("migration_status", StatusFail, err.Error(), map[string]any{
			"database_url": redactDatabaseURL(opts.DatabaseURL),
		})
	}
	defer rows.Close()

	applied := map[string]bool{}
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return check("migration_status", StatusFail, err.Error(), nil)
		}
		applied[version] = true
	}
	if err := rows.Err(); err != nil {
		return check("migration_status", StatusFail, err.Error(), nil)
	}

	missing := make([]string, 0)
	for _, version := range expected {
		if !applied[version] {
			missing = append(missing, version)
		}
	}
	details := map[string]any{
		"expected_count":  len(expected),
		"applied_count":   len(applied),
		"latest_expected": latest(expected),
	}
	if len(missing) > 0 {
		details["missing"] = missing
		return check("migration_status", StatusFail, "database is missing expected migrations", details)
	}
	return check("migration_status", StatusPass, "all local migrations are applied", details)
}

func optionalChecks(opts Options) []CheckEvidence {
	emailStatus := StatusFail
	emailSummary := "sendmail_http configuration is incomplete"
	emailDetails := map[string]any{
		"base_url":         strings.TrimSpace(opts.SendMailHTTPBaseURL),
		"token_configured": strings.TrimSpace(opts.SendMailHTTPBearerToken) != "",
	}
	if endpoint, err := url.Parse(strings.TrimSpace(opts.SendMailHTTPBaseURL)); err == nil && endpoint.Scheme == "https" && endpoint.Host != "" && strings.TrimSpace(opts.SendMailHTTPBearerToken) != "" {
		emailStatus = StatusPass
		emailSummary = "sendmail_http settings are present"
	}

	brokerStatus := StatusSkip
	brokerSummary := "cross-service broker is not configured"
	brokerDetails := map[string]any{}
	if strings.TrimSpace(opts.Broker) != "" {
		brokerStatus = StatusPass
		brokerSummary = "cross-service broker settings are present"
		brokerDetails["broker"] = opts.Broker
		brokerDetails["command_stream"] = valueOrDefault(opts.CommandStream, "account.video.commands")
		brokerDetails["event_stream"] = valueOrDefault(opts.EventStream, "video.account.events")
	}

	return []CheckEvidence{
		check("sendmail_http_configuration", emailStatus, emailSummary, emailDetails),
		check("cross_service_channel_optional", brokerStatus, brokerSummary, brokerDetails),
	}
}

func runAuthAndOrgChecks(ctx context.Context, opts Options) (string, string, []CheckEvidence) {
	if strings.TrimSpace(opts.Email) == "" || strings.TrimSpace(opts.Password) == "" {
		return "", "", []CheckEvidence{
			check("auth_login", StatusSkip, "READINESS_SMOKE_EMAIL and READINESS_SMOKE_PASSWORD are required for login smoke", nil),
			check("organization_smoke", StatusSkip, "login smoke was skipped", nil),
		}
	}

	var login loginResponse
	status, err := doJSON(ctx, opts, http.MethodPost, "/v1/auth/login", "", map[string]string{
		"email":    opts.Email,
		"password": opts.Password,
	}, &login)
	if err != nil {
		return "", "", []CheckEvidence{
			check("auth_login", StatusFail, err.Error(), nil),
			check("organization_smoke", StatusSkip, "login smoke failed", nil),
		}
	}
	if status != http.StatusOK || strings.TrimSpace(login.Tokens.AccessToken) == "" {
		return "", "", []CheckEvidence{
			check("auth_login", StatusFail, fmt.Sprintf("unexpected HTTP status %d", status), map[string]any{"http_status": status}),
			check("organization_smoke", StatusSkip, "login smoke failed", nil),
		}
	}

	authCheck := check("auth_login", StatusPass, "login returned an access token", map[string]any{
		"http_status": status,
		"user_id":     login.User.ID,
		"email":       redactEmail(login.User.Email),
	})

	orgID := strings.TrimSpace(opts.OrganizationID)
	if orgID != "" {
		status, err := doJSON(ctx, opts, http.MethodGet, "/v1/orgs/"+url.PathEscape(orgID), login.Tokens.AccessToken, nil, nil)
		if err != nil {
			return login.Tokens.AccessToken, orgID, []CheckEvidence{authCheck, check("organization_smoke", StatusFail, err.Error(), nil)}
		}
		if status != http.StatusOK {
			return login.Tokens.AccessToken, orgID, []CheckEvidence{authCheck, check("organization_smoke", StatusFail, fmt.Sprintf("unexpected HTTP status %d", status), map[string]any{"http_status": status, "organization_id": orgID})}
		}
		return login.Tokens.AccessToken, orgID, []CheckEvidence{authCheck, check("organization_smoke", StatusPass, "configured organization is readable", map[string]any{"organization_id": orgID})}
	}

	var orgs orgListResponse
	status, err = doJSON(ctx, opts, http.MethodGet, "/v1/orgs?limit=1", login.Tokens.AccessToken, nil, &orgs)
	if err != nil {
		return login.Tokens.AccessToken, "", []CheckEvidence{authCheck, check("organization_smoke", StatusFail, err.Error(), nil)}
	}
	if status != http.StatusOK {
		return login.Tokens.AccessToken, "", []CheckEvidence{authCheck, check("organization_smoke", StatusFail, fmt.Sprintf("unexpected HTTP status %d", status), map[string]any{"http_status": status})}
	}
	if len(orgs.Organizations) == 0 {
		return login.Tokens.AccessToken, "", []CheckEvidence{authCheck, check("organization_smoke", StatusSkip, "login succeeded but no organization is visible to the smoke user", nil)}
	}
	orgID = orgs.Organizations[0].ID
	return login.Tokens.AccessToken, orgID, []CheckEvidence{authCheck, check("organization_smoke", StatusPass, "at least one organization is readable", map[string]any{"organization_id": orgID})}
}

func runDeviceChecks(ctx context.Context, opts Options, accessToken, orgID string) []CheckEvidence {
	if strings.TrimSpace(accessToken) == "" || strings.TrimSpace(orgID) == "" {
		return []CheckEvidence{
			check("device_smoke", StatusSkip, "organization smoke did not produce an organization id", nil),
			check("provisioning_readiness", StatusSkip, "device smoke was skipped", nil),
		}
	}

	deviceID := strings.TrimSpace(opts.DeviceID)
	if deviceID != "" {
		status, err := doJSON(ctx, opts, http.MethodGet, "/v1/orgs/"+url.PathEscape(orgID)+"/devices/"+url.PathEscape(deviceID), accessToken, nil, nil)
		if err != nil {
			return []CheckEvidence{
				check("device_smoke", StatusFail, err.Error(), nil),
				check("provisioning_readiness", StatusSkip, "device smoke failed", nil),
			}
		}
		if status != http.StatusOK {
			return []CheckEvidence{
				check("device_smoke", StatusFail, fmt.Sprintf("unexpected HTTP status %d", status), map[string]any{"http_status": status, "device_id": deviceID}),
				check("provisioning_readiness", StatusSkip, "device smoke failed", nil),
			}
		}
		return append([]CheckEvidence{
			check("device_smoke", StatusPass, "configured device is readable", map[string]any{"organization_id": orgID, "device_id": deviceID}),
		}, runProvisioningReadiness(ctx, opts, accessToken, orgID, deviceID)...)
	}

	var devices deviceListResponse
	status, err := doJSON(ctx, opts, http.MethodGet, "/v1/orgs/"+url.PathEscape(orgID)+"/devices?limit=1", accessToken, nil, &devices)
	if err != nil {
		return []CheckEvidence{
			check("device_smoke", StatusFail, err.Error(), nil),
			check("provisioning_readiness", StatusSkip, "device smoke failed", nil),
		}
	}
	if status != http.StatusOK {
		return []CheckEvidence{
			check("device_smoke", StatusFail, fmt.Sprintf("unexpected HTTP status %d", status), map[string]any{"http_status": status}),
			check("provisioning_readiness", StatusSkip, "device smoke failed", nil),
		}
	}
	if len(devices.Devices) == 0 {
		return []CheckEvidence{
			check("device_smoke", StatusSkip, "organization has no devices visible to the smoke user", map[string]any{"organization_id": orgID}),
			check("provisioning_readiness", StatusSkip, "no device id available", nil),
		}
	}
	deviceID = devices.Devices[0].ID
	return append([]CheckEvidence{
		check("device_smoke", StatusPass, "at least one device is readable", map[string]any{"organization_id": orgID, "device_id": deviceID}),
	}, runProvisioningReadiness(ctx, opts, accessToken, orgID, deviceID)...)
}

func runProvisioningReadiness(ctx context.Context, opts Options, accessToken, orgID, deviceID string) []CheckEvidence {
	var provisioning provisioningResponse
	status, err := doJSON(ctx, opts, http.MethodGet, "/v1/orgs/"+url.PathEscape(orgID)+"/devices/"+url.PathEscape(deviceID)+"/provisioning", accessToken, nil, &provisioning)
	if err != nil {
		return []CheckEvidence{check("provisioning_readiness", StatusFail, err.Error(), nil)}
	}
	if status == http.StatusConflict || status == http.StatusNotFound {
		return []CheckEvidence{check("provisioning_readiness", StatusSkip, "device has no provisioning evidence yet", map[string]any{"http_status": status, "device_id": deviceID})}
	}
	if status != http.StatusOK {
		return []CheckEvidence{check("provisioning_readiness", StatusFail, fmt.Sprintf("unexpected HTTP status %d", status), map[string]any{"http_status": status, "device_id": deviceID})}
	}
	return []CheckEvidence{check("provisioning_readiness", StatusPass, "provisioning/readiness evidence is readable", map[string]any{
		"device_id":        deviceID,
		"operation_id":     provisioning.Operation.OperationID,
		"operation_type":   provisioning.Operation.OperationType,
		"operation_status": provisioning.Operation.Status,
		"readiness_state":  provisioning.Readiness.State,
		"product_state":    provisioning.Readiness.ProductState,
	})}
}

func doJSON(ctx context.Context, opts Options, method, path, accessToken string, requestBody any, responseBody any) (int, error) {
	var body io.Reader
	if requestBody != nil {
		payload, err := json.Marshal(requestBody)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(opts.BaseURL, "/")+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	if requestBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	res, err := opts.httpClient().Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if responseBody != nil {
		if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(responseBody); err != nil {
			return res.StatusCode, err
		}
	}
	return res.StatusCode, nil
}

func migrationFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func (r *Report) add(c CheckEvidence) {
	r.Checks = append(r.Checks, c)
}

func (r *Report) addAll(checks ...CheckEvidence) {
	r.Checks = append(r.Checks, checks...)
}

func (r *Report) finish() {
	for _, c := range r.Checks {
		switch c.Status {
		case StatusPass:
			r.Summary.Pass++
		case StatusFail:
			r.Summary.Fail++
		case StatusSkip:
			r.Summary.Skip++
		}
	}
}

func check(name string, status Status, summary string, details map[string]any) CheckEvidence {
	if len(details) == 0 {
		details = nil
	}
	return CheckEvidence{Name: name, Status: status, Summary: summary, Details: details}
}

func (o Options) withDefaults() Options {
	if strings.TrimSpace(o.BaseURL) == "" {
		o.BaseURL = "http://localhost:8080"
	}
	if strings.TrimSpace(o.MigrationsDir) == "" {
		o.MigrationsDir = "migrations"
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	return o
}

func (o Options) now() time.Time {
	return o.Now().UTC()
}

func (o Options) httpClient() *http.Client {
	if o.HTTPClient != nil {
		return o.HTTPClient
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func versionStatus(version string) Status {
	if strings.TrimSpace(version) == "" {
		return StatusSkip
	}
	return StatusPass
}

func versionSummary(version string) string {
	if strings.TrimSpace(version) == "" {
		return "service version was not provided"
	}
	return "service version was recorded"
}

func serviceVersionState(version string) string {
	if strings.TrimSpace(version) == "" {
		return "not_provided"
	}
	return "provided"
}

func latest(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[len(values)-1]
}

func valueOrDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func redactEmail(email string) string {
	email = strings.TrimSpace(email)
	if email == "" {
		return ""
	}
	local, domain, ok := strings.Cut(email, "@")
	if !ok {
		return "[redacted]"
	}
	if len(local) <= 1 {
		return "*@" + domain
	}
	return local[:1] + "***@" + domain
}

func redactDatabaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "[redacted]"
	}
	if parsed.User != nil {
		parsed.User = url.User("[redacted]")
	}
	if parsed.Host != "" {
		parsed.Host = "[redacted]"
	}
	return parsed.String()
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
