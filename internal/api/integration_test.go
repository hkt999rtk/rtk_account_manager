package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kevinhuang/rtk_account_manager/internal/auth"
	"github.com/kevinhuang/rtk_account_manager/internal/database"
	"github.com/kevinhuang/rtk_account_manager/internal/store"
)

type integrationEnv struct {
	router *gin.Engine
	db     *pgxpool.Pool
}

func newIntegrationEnv(t *testing.T) integrationEnv {
	t.Helper()

	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx := context.Background()
	db, err := database.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)

	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		TRUNCATE refresh_tokens, devices, organization_members, organizations, users
		RESTART IDENTITY CASCADE
	`); err != nil {
		t.Fatal(err)
	}

	gin.SetMode(gin.TestMode)
	authService := auth.NewService("test-access-secret", "test-refresh-secret", time.Minute, time.Hour)
	return integrationEnv{router: New(store.New(db), authService).Router(), db: db}
}

func TestIntegrationRegisterLoginRefreshAndLogout(t *testing.T) {
	env := newIntegrationEnv(t)

	registered := registerUser(t, env.router, "owner@example.com", "Owner Org")
	if registered.User.ID == "" || registered.Organization.ID == "" {
		t.Fatal("expected user and organization IDs")
	}

	loginRes := performJSON(env.router, http.MethodPost, "/v1/auth/login", map[string]any{
		"email":    "owner@example.com",
		"password": "password123",
	}, "")
	if loginRes.Code != http.StatusOK {
		t.Fatalf("expected login 200, got %d: %s", loginRes.Code, loginRes.Body.String())
	}

	loginBody := decodeBody[tokenBody](t, loginRes)
	refreshRes := performJSON(env.router, http.MethodPost, "/v1/auth/refresh", map[string]any{
		"refresh_token": loginBody.Tokens.RefreshToken,
	}, "")
	if refreshRes.Code != http.StatusOK {
		t.Fatalf("expected refresh 200, got %d: %s", refreshRes.Code, refreshRes.Body.String())
	}

	oldRefreshRes := performJSON(env.router, http.MethodPost, "/v1/auth/refresh", map[string]any{
		"refresh_token": loginBody.Tokens.RefreshToken,
	}, "")
	if oldRefreshRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected old refresh token to be revoked, got %d", oldRefreshRes.Code)
	}

	refreshedBody := decodeBody[tokenBody](t, refreshRes)
	logoutRes := performJSON(env.router, http.MethodPost, "/v1/auth/logout", map[string]any{
		"refresh_token": refreshedBody.Tokens.RefreshToken,
	}, refreshedBody.Tokens.AccessToken)
	if logoutRes.Code != http.StatusNoContent {
		t.Fatalf("expected logout 204, got %d", logoutRes.Code)
	}
}

func TestIntegrationRoleAuthorizationDeviceScopeAndSerialUniqueness(t *testing.T) {
	env := newIntegrationEnv(t)

	owner := registerUser(t, env.router, "owner@example.com", "Owner Org")
	member := registerUser(t, env.router, "member@example.com", "Member Org")

	addMemberRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/members", map[string]any{
		"email": "member@example.com",
		"role":  "member",
	}, owner.Tokens.AccessToken)
	if addMemberRes.Code != http.StatusCreated {
		t.Fatalf("expected add member 201, got %d: %s", addMemberRes.Code, addMemberRes.Body.String())
	}

	memberCreateRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices", devicePayload("cam-1", "SERIAL-1"), member.Tokens.AccessToken)
	if memberCreateRes.Code != http.StatusForbidden {
		t.Fatalf("expected member device create 403, got %d", memberCreateRes.Code)
	}

	deviceRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices", devicePayload("cam-1", "SERIAL-1"), owner.Tokens.AccessToken)
	if deviceRes.Code != http.StatusCreated {
		t.Fatalf("expected device create 201, got %d: %s", deviceRes.Code, deviceRes.Body.String())
	}
	deviceBody := decodeBody[deviceBody](t, deviceRes)

	duplicateRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices", devicePayload("cam-dup", "SERIAL-1"), owner.Tokens.AccessToken)
	if duplicateRes.Code != http.StatusConflict {
		t.Fatalf("expected duplicate serial 409, got %d", duplicateRes.Code)
	}

	otherOrgRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+member.Organization.ID+"/devices", devicePayload("cam-other", "SERIAL-1"), member.Tokens.AccessToken)
	if otherOrgRes.Code != http.StatusCreated {
		t.Fatalf("expected same serial in different org 201, got %d: %s", otherOrgRes.Code, otherOrgRes.Body.String())
	}

	crossOrgRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+member.Organization.ID+"/devices/"+deviceBody.Device.ID, nil, member.Tokens.AccessToken)
	if crossOrgRes.Code != http.StatusNotFound {
		t.Fatalf("expected cross-org device lookup 404, got %d", crossOrgRes.Code)
	}

	statusRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/devices/"+deviceBody.Device.ID+"/status", map[string]any{
		"status": "online",
	}, owner.Tokens.AccessToken)
	if statusRes.Code != http.StatusOK {
		t.Fatalf("expected status update 200, got %d", statusRes.Code)
	}

	deleteRes := performJSON(env.router, http.MethodDelete, "/v1/orgs/"+owner.Organization.ID+"/devices/"+deviceBody.Device.ID, nil, owner.Tokens.AccessToken)
	if deleteRes.Code != http.StatusNoContent {
		t.Fatalf("expected delete 204, got %d", deleteRes.Code)
	}

	var status string
	var disabledAt *time.Time
	err := env.db.QueryRow(context.Background(), `SELECT status, disabled_at FROM devices WHERE id = $1`, deviceBody.Device.ID).Scan(&status, &disabledAt)
	if err != nil {
		t.Fatal(err)
	}
	if status != "disabled" || disabledAt == nil {
		t.Fatalf("expected soft-disabled device, got status=%s disabled_at=%v", status, disabledAt)
	}
}

func TestIntegrationLastOwnerCannotBeRemovedOrDowngraded(t *testing.T) {
	env := newIntegrationEnv(t)

	owner := registerUser(t, env.router, "owner@example.com", "Owner Org")

	downgradeRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/members/"+owner.User.ID, map[string]any{
		"role": "admin",
	}, owner.Tokens.AccessToken)
	if downgradeRes.Code != http.StatusConflict {
		t.Fatalf("expected last owner downgrade 409, got %d", downgradeRes.Code)
	}

	removeRes := performJSON(env.router, http.MethodDelete, "/v1/orgs/"+owner.Organization.ID+"/members/"+owner.User.ID, nil, owner.Tokens.AccessToken)
	if removeRes.Code != http.StatusConflict {
		t.Fatalf("expected last owner remove 409, got %d", removeRes.Code)
	}
}

func TestIntegrationRejectsBlankNames(t *testing.T) {
	env := newIntegrationEnv(t)

	blankOrgRes := performJSON(env.router, http.MethodPost, "/v1/auth/register", map[string]any{
		"email":             "blank@example.com",
		"password":          "password123",
		"organization_name": "   ",
	}, "")
	if blankOrgRes.Code != http.StatusBadRequest {
		t.Fatalf("expected blank organization name 400, got %d", blankOrgRes.Code)
	}

	owner := registerUser(t, env.router, "owner@example.com", "Owner Org")
	blankDeviceRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices", map[string]any{
		"name":     "   ",
		"category": "ip_camera",
	}, owner.Tokens.AccessToken)
	if blankDeviceRes.Code != http.StatusBadRequest {
		t.Fatalf("expected blank device name 400, got %d", blankDeviceRes.Code)
	}
}

type registerBody struct {
	User struct {
		ID string `json:"id"`
	} `json:"user"`
	Organization struct {
		ID string `json:"id"`
	} `json:"organization"`
	Tokens struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	} `json:"tokens"`
}

type tokenBody struct {
	Tokens struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	} `json:"tokens"`
}

type deviceBody struct {
	Device struct {
		ID string `json:"id"`
	} `json:"device"`
}

func registerUser(t *testing.T, router *gin.Engine, email, orgName string) registerBody {
	t.Helper()
	res := performJSON(router, http.MethodPost, "/v1/auth/register", map[string]any{
		"email":             email,
		"password":          "password123",
		"display_name":      email,
		"organization_name": orgName,
	}, "")
	if res.Code != http.StatusCreated {
		t.Fatalf("expected register 201, got %d: %s", res.Code, res.Body.String())
	}
	return decodeBody[registerBody](t, res)
}

func devicePayload(name, serial string) map[string]any {
	return map[string]any{
		"name":          name,
		"category":      "ip_camera",
		"serial_number": serial,
		"metadata": map[string]any{
			"location": "lab",
		},
	}
}

func performJSON(router *gin.Engine, method, path string, body any, accessToken string) *httptest.ResponseRecorder {
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)
	return res
}

func decodeBody[T any](t *testing.T, res *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v: %s", err, res.Body.String())
	}
	return out
}
