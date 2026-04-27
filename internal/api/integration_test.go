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

	revokedRefreshRes := performJSON(env.router, http.MethodPost, "/v1/auth/refresh", map[string]any{
		"refresh_token": refreshedBody.Tokens.RefreshToken,
	}, "")
	if revokedRefreshRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected logged-out refresh token 401, got %d", revokedRefreshRes.Code)
	}

	invalidLoginRes := performJSON(env.router, http.MethodPost, "/v1/auth/login", map[string]any{
		"email":    "owner@example.com",
		"password": "wrong-password",
	}, "")
	if invalidLoginRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected invalid login 401, got %d", invalidLoginRes.Code)
	}

	invalidRefreshRes := performJSON(env.router, http.MethodPost, "/v1/auth/refresh", map[string]any{
		"refresh_token": "not-a-token",
	}, "")
	if invalidRefreshRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected invalid refresh token 401, got %d", invalidRefreshRes.Code)
	}
}

func TestIntegrationDisabledUserCannotUseExistingTokens(t *testing.T) {
	env := newIntegrationEnv(t)

	registered := registerUser(t, env.router, "disabled@example.com", "Disabled Org")
	if _, err := env.db.Exec(context.Background(), `
		UPDATE users SET disabled_at = now() WHERE id = $1
	`, registered.User.ID); err != nil {
		t.Fatal(err)
	}

	meRes := performJSON(env.router, http.MethodGet, "/v1/me", nil, registered.Tokens.AccessToken)
	if meRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected disabled user access token 401, got %d", meRes.Code)
	}

	createOrgRes := performJSON(env.router, http.MethodPost, "/v1/orgs", map[string]any{
		"name": "Should Fail",
	}, registered.Tokens.AccessToken)
	if createOrgRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected disabled user org create 401, got %d", createOrgRes.Code)
	}

	refreshRes := performJSON(env.router, http.MethodPost, "/v1/auth/refresh", map[string]any{
		"refresh_token": registered.Tokens.RefreshToken,
	}, "")
	if refreshRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected disabled user refresh 401, got %d", refreshRes.Code)
	}
}

func TestIntegrationRoleAuthorizationDeviceScopeAndSerialUniqueness(t *testing.T) {
	env := newIntegrationEnv(t)

	owner := registerUser(t, env.router, "owner@example.com", "Owner Org")
	member := registerUser(t, env.router, "member@example.com", "Member Org")
	admin := registerUser(t, env.router, "admin@example.com", "Admin Org")

	addMemberRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/members", map[string]any{
		"email": "member@example.com",
		"role":  "member",
	}, owner.Tokens.AccessToken)
	if addMemberRes.Code != http.StatusCreated {
		t.Fatalf("expected add member 201, got %d: %s", addMemberRes.Code, addMemberRes.Body.String())
	}

	addAdminRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/members", map[string]any{
		"email": "admin@example.com",
		"role":  "admin",
	}, owner.Tokens.AccessToken)
	if addAdminRes.Code != http.StatusCreated {
		t.Fatalf("expected add admin 201, got %d: %s", addAdminRes.Code, addAdminRes.Body.String())
	}

	adminAddMemberRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/members", map[string]any{
		"email": "admin@example.com",
		"role":  "member",
	}, admin.Tokens.AccessToken)
	if adminAddMemberRes.Code != http.StatusForbidden {
		t.Fatalf("expected admin add member 403, got %d", adminAddMemberRes.Code)
	}

	adminUpdateMemberRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/members/"+member.User.ID, map[string]any{
		"role": "admin",
	}, admin.Tokens.AccessToken)
	if adminUpdateMemberRes.Code != http.StatusForbidden {
		t.Fatalf("expected admin update member 403, got %d", adminUpdateMemberRes.Code)
	}

	adminRemoveMemberRes := performJSON(env.router, http.MethodDelete, "/v1/orgs/"+owner.Organization.ID+"/members/"+member.User.ID, nil, admin.Tokens.AccessToken)
	if adminRemoveMemberRes.Code != http.StatusForbidden {
		t.Fatalf("expected admin remove member 403, got %d", adminRemoveMemberRes.Code)
	}

	memberCreateRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices", devicePayload("cam-1", "SERIAL-1"), member.Tokens.AccessToken)
	if memberCreateRes.Code != http.StatusForbidden {
		t.Fatalf("expected member device create 403, got %d", memberCreateRes.Code)
	}

	deviceRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices", devicePayload("cam-1", "SERIAL-1"), owner.Tokens.AccessToken)
	if deviceRes.Code != http.StatusCreated {
		t.Fatalf("expected device create 201, got %d: %s", deviceRes.Code, deviceRes.Body.String())
	}
	createdDeviceBody := decodeBody[deviceBody](t, deviceRes)

	memberListRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+owner.Organization.ID+"/devices", nil, member.Tokens.AccessToken)
	if memberListRes.Code != http.StatusOK {
		t.Fatalf("expected member list devices 200, got %d", memberListRes.Code)
	}

	memberGetRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+owner.Organization.ID+"/devices/"+createdDeviceBody.Device.ID, nil, member.Tokens.AccessToken)
	if memberGetRes.Code != http.StatusOK {
		t.Fatalf("expected member get device 200, got %d", memberGetRes.Code)
	}

	memberUpdateRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/devices/"+createdDeviceBody.Device.ID, devicePayload("member-update", "SERIAL-2"), member.Tokens.AccessToken)
	if memberUpdateRes.Code != http.StatusForbidden {
		t.Fatalf("expected member update device 403, got %d", memberUpdateRes.Code)
	}

	memberStatusRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/devices/"+createdDeviceBody.Device.ID+"/status", map[string]any{
		"status": "offline",
	}, member.Tokens.AccessToken)
	if memberStatusRes.Code != http.StatusForbidden {
		t.Fatalf("expected member status update 403, got %d", memberStatusRes.Code)
	}

	memberDeleteRes := performJSON(env.router, http.MethodDelete, "/v1/orgs/"+owner.Organization.ID+"/devices/"+createdDeviceBody.Device.ID, nil, member.Tokens.AccessToken)
	if memberDeleteRes.Code != http.StatusForbidden {
		t.Fatalf("expected member delete device 403, got %d", memberDeleteRes.Code)
	}

	adminDeviceRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices", devicePayload("admin-cam", "ADMIN-SERIAL-1"), admin.Tokens.AccessToken)
	if adminDeviceRes.Code != http.StatusCreated {
		t.Fatalf("expected admin create device 201, got %d: %s", adminDeviceRes.Code, adminDeviceRes.Body.String())
	}
	adminDeviceBody := decodeBody[deviceBody](t, adminDeviceRes)

	adminUpdateDeviceRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/devices/"+adminDeviceBody.Device.ID, devicePayload("admin-cam-updated", "ADMIN-SERIAL-2"), admin.Tokens.AccessToken)
	if adminUpdateDeviceRes.Code != http.StatusOK {
		t.Fatalf("expected admin update device 200, got %d", adminUpdateDeviceRes.Code)
	}

	adminStatusRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/devices/"+adminDeviceBody.Device.ID+"/status", map[string]any{
		"status": "online",
	}, admin.Tokens.AccessToken)
	if adminStatusRes.Code != http.StatusOK {
		t.Fatalf("expected admin status update 200, got %d", adminStatusRes.Code)
	}

	adminDeleteRes := performJSON(env.router, http.MethodDelete, "/v1/orgs/"+owner.Organization.ID+"/devices/"+adminDeviceBody.Device.ID, nil, admin.Tokens.AccessToken)
	if adminDeleteRes.Code != http.StatusNoContent {
		t.Fatalf("expected admin delete device 204, got %d", adminDeleteRes.Code)
	}

	duplicateRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices", devicePayload("cam-dup", "SERIAL-1"), owner.Tokens.AccessToken)
	if duplicateRes.Code != http.StatusConflict {
		t.Fatalf("expected duplicate serial 409, got %d", duplicateRes.Code)
	}

	otherOrgRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+member.Organization.ID+"/devices", devicePayload("cam-other", "SERIAL-1"), member.Tokens.AccessToken)
	if otherOrgRes.Code != http.StatusCreated {
		t.Fatalf("expected same serial in different org 201, got %d: %s", otherOrgRes.Code, otherOrgRes.Body.String())
	}

	crossOrgRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+member.Organization.ID+"/devices/"+createdDeviceBody.Device.ID, nil, member.Tokens.AccessToken)
	if crossOrgRes.Code != http.StatusNotFound {
		t.Fatalf("expected cross-org device lookup 404, got %d", crossOrgRes.Code)
	}

	otherOrgResByOwner := performJSON(env.router, http.MethodGet, "/v1/orgs/"+member.Organization.ID, nil, owner.Tokens.AccessToken)
	if otherOrgResByOwner.Code != http.StatusNotFound {
		t.Fatalf("expected cross-org organization lookup 404, got %d", otherOrgResByOwner.Code)
	}

	otherOrgMembersRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+member.Organization.ID+"/members", nil, owner.Tokens.AccessToken)
	if otherOrgMembersRes.Code != http.StatusNotFound {
		t.Fatalf("expected cross-org member list 404, got %d", otherOrgMembersRes.Code)
	}

	listBody := decodeBody[devicesBody](t, memberListRes)
	if len(listBody.Devices) != 1 || listBody.Devices[0].ID != createdDeviceBody.Device.ID {
		t.Fatalf("expected member list to include created device, got %+v", listBody.Devices)
	}
	if listBody.Pagination.Limit != 50 || listBody.Pagination.Offset != 0 || listBody.Pagination.Total != 1 {
		t.Fatalf("expected default device pagination, got %+v", listBody.Pagination)
	}

	updatedDeviceRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/devices/"+createdDeviceBody.Device.ID, devicePayload("cam-updated", "SERIAL-UPDATED"), owner.Tokens.AccessToken)
	if updatedDeviceRes.Code != http.StatusOK {
		t.Fatalf("expected owner update device 200, got %d", updatedDeviceRes.Code)
	}
	updatedDeviceBody := decodeBody[deviceBody](t, updatedDeviceRes)
	if updatedDeviceBody.Device.Name != "cam-updated" || updatedDeviceBody.Device.SerialNumber == nil || *updatedDeviceBody.Device.SerialNumber != "SERIAL-UPDATED" {
		t.Fatalf("expected updated device fields, got %+v", updatedDeviceBody.Device)
	}

	invalidCategoryRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices", map[string]any{
		"name":     "bad-category",
		"category": "bad",
	}, owner.Tokens.AccessToken)
	if invalidCategoryRes.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid category 400, got %d", invalidCategoryRes.Code)
	}

	statusRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/devices/"+createdDeviceBody.Device.ID+"/status", map[string]any{
		"status": "online",
	}, owner.Tokens.AccessToken)
	if statusRes.Code != http.StatusOK {
		t.Fatalf("expected status update 200, got %d", statusRes.Code)
	}

	invalidStatusRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/devices/"+createdDeviceBody.Device.ID+"/status", map[string]any{
		"status": "bad",
	}, owner.Tokens.AccessToken)
	if invalidStatusRes.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid status 400, got %d", invalidStatusRes.Code)
	}

	deleteRes := performJSON(env.router, http.MethodDelete, "/v1/orgs/"+owner.Organization.ID+"/devices/"+createdDeviceBody.Device.ID, nil, owner.Tokens.AccessToken)
	if deleteRes.Code != http.StatusNoContent {
		t.Fatalf("expected delete 204, got %d", deleteRes.Code)
	}

	var status string
	var disabledAt *time.Time
	err := env.db.QueryRow(context.Background(), `SELECT status, disabled_at FROM devices WHERE id = $1`, createdDeviceBody.Device.ID).Scan(&status, &disabledAt)
	if err != nil {
		t.Fatal(err)
	}
	if status != "disabled" || disabledAt == nil {
		t.Fatalf("expected soft-disabled device, got status=%s disabled_at=%v", status, disabledAt)
	}

	getDisabledRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+owner.Organization.ID+"/devices/"+createdDeviceBody.Device.ID, nil, owner.Tokens.AccessToken)
	if getDisabledRes.Code != http.StatusOK {
		t.Fatalf("expected disabled device to remain readable, got %d", getDisabledRes.Code)
	}

	updateDisabledRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/devices/"+createdDeviceBody.Device.ID, devicePayload("disabled-update", "DISABLED-UPDATED"), owner.Tokens.AccessToken)
	if updateDisabledRes.Code != http.StatusConflict {
		t.Fatalf("expected disabled device update 409, got %d", updateDisabledRes.Code)
	}

	statusDisabledRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/devices/"+createdDeviceBody.Device.ID+"/status", map[string]any{
		"status": "online",
	}, owner.Tokens.AccessToken)
	if statusDisabledRes.Code != http.StatusConflict {
		t.Fatalf("expected disabled device status update 409, got %d", statusDisabledRes.Code)
	}

	deleteDisabledRes := performJSON(env.router, http.MethodDelete, "/v1/orgs/"+owner.Organization.ID+"/devices/"+createdDeviceBody.Device.ID, nil, owner.Tokens.AccessToken)
	if deleteDisabledRes.Code != http.StatusNoContent {
		t.Fatalf("expected repeated disabled device delete to remain 204, got %d", deleteDisabledRes.Code)
	}
}

func TestIntegrationListPaginationMetadata(t *testing.T) {
	env := newIntegrationEnv(t)

	owner := registerUser(t, env.router, "owner@example.com", "Owner Org")
	secondOrgRes := performJSON(env.router, http.MethodPost, "/v1/orgs", map[string]any{
		"name": "Second Org",
	}, owner.Tokens.AccessToken)
	if secondOrgRes.Code != http.StatusCreated {
		t.Fatalf("expected second org 201, got %d", secondOrgRes.Code)
	}

	orgsRes := performJSON(env.router, http.MethodGet, "/v1/orgs?limit=1&offset=1", nil, owner.Tokens.AccessToken)
	if orgsRes.Code != http.StatusOK {
		t.Fatalf("expected org list 200, got %d", orgsRes.Code)
	}
	orgsBody := decodeBody[organizationsBody](t, orgsRes)
	if len(orgsBody.Organizations) != 1 || orgsBody.Pagination.Limit != 1 || orgsBody.Pagination.Offset != 1 || orgsBody.Pagination.Total != 2 {
		t.Fatalf("unexpected org pagination response: %+v", orgsBody)
	}

	member := registerUser(t, env.router, "member@example.com", "Member Org")
	addMemberRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/members", map[string]any{
		"email": "member@example.com",
		"role":  "member",
	}, owner.Tokens.AccessToken)
	if addMemberRes.Code != http.StatusCreated {
		t.Fatalf("expected add member 201, got %d", addMemberRes.Code)
	}
	_ = member

	membersRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+owner.Organization.ID+"/members?limit=1&offset=1", nil, owner.Tokens.AccessToken)
	if membersRes.Code != http.StatusOK {
		t.Fatalf("expected member list 200, got %d", membersRes.Code)
	}
	membersBody := decodeBody[membersBody](t, membersRes)
	if len(membersBody.Members) != 1 || membersBody.Pagination.Limit != 1 || membersBody.Pagination.Offset != 1 || membersBody.Pagination.Total != 2 {
		t.Fatalf("unexpected member pagination response: %+v", membersBody)
	}

	for i, serial := range []string{"PAGE-1", "PAGE-2", "PAGE-3"} {
		res := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices", devicePayload("page-device-"+serial, serial), owner.Tokens.AccessToken)
		if res.Code != http.StatusCreated {
			t.Fatalf("expected device %d create 201, got %d", i, res.Code)
		}
	}
	devicesRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+owner.Organization.ID+"/devices?limit=2&offset=1", nil, owner.Tokens.AccessToken)
	if devicesRes.Code != http.StatusOK {
		t.Fatalf("expected device list 200, got %d", devicesRes.Code)
	}
	devicesBody := decodeBody[devicesBody](t, devicesRes)
	if len(devicesBody.Devices) != 2 || devicesBody.Pagination.Limit != 2 || devicesBody.Pagination.Offset != 1 || devicesBody.Pagination.Total != 3 {
		t.Fatalf("unexpected device pagination response: %+v", devicesBody)
	}
}

func TestIntegrationMigrationsAreIdempotent(t *testing.T) {
	env := newIntegrationEnv(t)

	if err := database.Migrate(context.Background(), env.db); err != nil {
		t.Fatal(err)
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

	_, err := env.db.Exec(context.Background(), `
		UPDATE organization_members SET role = 'admin'
		WHERE organization_id = $1 AND user_id = $2
	`, owner.Organization.ID, owner.User.ID)
	if err == nil {
		t.Fatal("expected direct SQL downgrade of last owner to fail")
	}

	_, err = env.db.Exec(context.Background(), `
		DELETE FROM organization_members
		WHERE organization_id = $1 AND user_id = $2
	`, owner.Organization.ID, owner.User.ID)
	if err == nil {
		t.Fatal("expected direct SQL deletion of last owner to fail")
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

func TestIntegrationDatabaseRejectsInvalidCoreData(t *testing.T) {
	env := newIntegrationEnv(t)

	if _, err := env.db.Exec(context.Background(), `
		INSERT INTO organizations (name) VALUES ('   ')
	`); err == nil {
		t.Fatal("expected database to reject blank organization name")
	}

	if _, err := env.db.Exec(context.Background(), `
		INSERT INTO users (email, password_hash) VALUES ('Upper@Example.com', 'hash')
	`); err == nil {
		t.Fatal("expected database to reject non-normalized email")
	}

	owner := registerUser(t, env.router, "owner@example.com", "Owner Org")
	if _, err := env.db.Exec(context.Background(), `
		INSERT INTO devices (organization_id, name, category) VALUES ($1, '   ', 'generic')
	`, owner.Organization.ID); err == nil {
		t.Fatal("expected database to reject blank device name")
	}
}

func TestIntegrationDatabaseMaintainsUpdatedAt(t *testing.T) {
	env := newIntegrationEnv(t)

	owner := registerUser(t, env.router, "owner@example.com", "Owner Org")

	var updatedAt time.Time
	if err := env.db.QueryRow(context.Background(), `
		UPDATE organizations
		SET name = 'Updated Org', updated_at = '2000-01-01T00:00:00Z'
		WHERE id = $1
		RETURNING updated_at
	`, owner.Organization.ID).Scan(&updatedAt); err != nil {
		t.Fatal(err)
	}
	if updatedAt.Year() == 2000 {
		t.Fatalf("expected organization updated_at trigger to override manual timestamp, got %s", updatedAt)
	}

	var userUpdatedAt time.Time
	if err := env.db.QueryRow(context.Background(), `
		UPDATE users
		SET display_name = 'Updated User', updated_at = '2000-01-01T00:00:00Z'
		WHERE id = $1
		RETURNING updated_at
	`, owner.User.ID).Scan(&userUpdatedAt); err != nil {
		t.Fatal(err)
	}
	if userUpdatedAt.Year() == 2000 {
		t.Fatalf("expected user updated_at trigger to override manual timestamp, got %s", userUpdatedAt)
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
		ID           string  `json:"id"`
		Name         string  `json:"name"`
		SerialNumber *string `json:"serial_number"`
	} `json:"device"`
}

type devicesBody struct {
	Devices []struct {
		ID string `json:"id"`
	} `json:"devices"`
	Pagination paginationBody `json:"pagination"`
}

type organizationsBody struct {
	Organizations []struct {
		ID string `json:"id"`
	} `json:"organizations"`
	Pagination paginationBody `json:"pagination"`
}

type membersBody struct {
	Members []struct {
		UserID string `json:"user_id"`
	} `json:"members"`
	Pagination paginationBody `json:"pagination"`
}

type paginationBody struct {
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
	Total  int `json:"total"`
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
