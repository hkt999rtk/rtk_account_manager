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

	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/channel"
	"rtk_account_manager/internal/database"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
	"rtk_account_manager/internal/testutil"
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

	testutil.LockIntegrationDatabase(t, db)

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

	meRes := performJSON(env.router, http.MethodGet, "/v1/me", nil, registered.Tokens.AccessToken)
	if meRes.Code != http.StatusOK {
		t.Fatalf("expected me 200, got %d: %s", meRes.Code, meRes.Body.String())
	}
	meBody := decodeBody[meBody](t, meRes)
	if meBody.User.ID != registered.User.ID || len(meBody.Organizations) != 1 || meBody.Organizations[0].ID != registered.Organization.ID {
		t.Fatalf("unexpected me response: %+v", meBody)
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

func TestIntegrationOwnerCanDisableAndEnableMemberUser(t *testing.T) {
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

	adminDisableRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/members/"+member.User.ID+"/disable", nil, admin.Tokens.AccessToken)
	if adminDisableRes.Code != http.StatusForbidden {
		t.Fatalf("expected admin disable member 403, got %d", adminDisableRes.Code)
	}
	memberDisableRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/members/"+member.User.ID+"/disable", nil, member.Tokens.AccessToken)
	if memberDisableRes.Code != http.StatusForbidden {
		t.Fatalf("expected member disable member 403, got %d", memberDisableRes.Code)
	}

	disableRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/members/"+member.User.ID+"/disable", nil, owner.Tokens.AccessToken)
	if disableRes.Code != http.StatusOK {
		t.Fatalf("expected owner disable member 200, got %d: %s", disableRes.Code, disableRes.Body.String())
	}
	disabledMember := decodeBody[memberBody](t, disableRes)
	if disabledMember.Member.DisabledAt == nil {
		t.Fatal("expected disabled member response to include disabled_at")
	}

	memberMeRes := performJSON(env.router, http.MethodGet, "/v1/me", nil, member.Tokens.AccessToken)
	if memberMeRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected disabled member access token 401, got %d", memberMeRes.Code)
	}
	memberRefreshRes := performJSON(env.router, http.MethodPost, "/v1/auth/refresh", map[string]any{
		"refresh_token": member.Tokens.RefreshToken,
	}, "")
	if memberRefreshRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected disabled member refresh 401, got %d", memberRefreshRes.Code)
	}
	memberLoginRes := performJSON(env.router, http.MethodPost, "/v1/auth/login", map[string]any{
		"email":    "member@example.com",
		"password": "password123",
	}, "")
	if memberLoginRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected disabled member login 401, got %d", memberLoginRes.Code)
	}

	membersRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+owner.Organization.ID+"/members", nil, owner.Tokens.AccessToken)
	if membersRes.Code != http.StatusOK {
		t.Fatalf("expected member list 200, got %d", membersRes.Code)
	}
	members := decodeBody[membersBody](t, membersRes)
	if members.Pagination.Total != 3 {
		t.Fatalf("expected disabled member to remain listed, got pagination %+v", members.Pagination)
	}

	enableRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/members/"+member.User.ID+"/enable", nil, owner.Tokens.AccessToken)
	if enableRes.Code != http.StatusOK {
		t.Fatalf("expected owner enable member 200, got %d: %s", enableRes.Code, enableRes.Body.String())
	}
	enabledMember := decodeBody[memberBody](t, enableRes)
	if enabledMember.Member.DisabledAt != nil {
		t.Fatal("expected enabled member response to clear disabled_at")
	}

	memberLoginAfterEnableRes := performJSON(env.router, http.MethodPost, "/v1/auth/login", map[string]any{
		"email":    "member@example.com",
		"password": "password123",
	}, "")
	if memberLoginAfterEnableRes.Code != http.StatusOK {
		t.Fatalf("expected enabled member login 200, got %d", memberLoginAfterEnableRes.Code)
	}
}

func TestIntegrationOwnerCanUpdateAndRemoveMember(t *testing.T) {
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

	updateRoleRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/members/"+member.User.ID, map[string]any{
		"role": "admin",
	}, owner.Tokens.AccessToken)
	if updateRoleRes.Code != http.StatusOK {
		t.Fatalf("expected member role update 200, got %d: %s", updateRoleRes.Code, updateRoleRes.Body.String())
	}
	updated := decodeBody[memberBody](t, updateRoleRes)
	if updated.Member.Role != "admin" {
		t.Fatalf("expected updated role admin, got %+v", updated.Member)
	}

	removeMemberRes := performJSON(env.router, http.MethodDelete, "/v1/orgs/"+owner.Organization.ID+"/members/"+member.User.ID, nil, owner.Tokens.AccessToken)
	if removeMemberRes.Code != http.StatusNoContent {
		t.Fatalf("expected member remove 204, got %d", removeMemberRes.Code)
	}

	memberListRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+owner.Organization.ID+"/members", nil, owner.Tokens.AccessToken)
	if memberListRes.Code != http.StatusOK {
		t.Fatalf("expected member list 200, got %d", memberListRes.Code)
	}
	members := decodeBody[membersBody](t, memberListRes)
	if members.Pagination.Total != 1 {
		t.Fatalf("expected only owner after member removal, got pagination %+v", members.Pagination)
	}
}

func TestIntegrationValidationAndNotFoundErrors(t *testing.T) {
	env := newIntegrationEnv(t)

	owner := registerUser(t, env.router, "owner@example.com", "Owner Org")

	malformedCreateOrgRes := performRaw(env.router, http.MethodPost, "/v1/orgs", []byte(`{"name":`), owner.Tokens.AccessToken)
	if malformedCreateOrgRes.Code != http.StatusBadRequest {
		t.Fatalf("expected malformed org create 400, got %d", malformedCreateOrgRes.Code)
	}

	invalidAddRoleRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/members", map[string]any{
		"email": "missing@example.com",
		"role":  "invalid",
	}, owner.Tokens.AccessToken)
	if invalidAddRoleRes.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid add member role 400, got %d", invalidAddRoleRes.Code)
	}

	invalidUpdateRoleRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/members/"+owner.User.ID, map[string]any{
		"role": "invalid",
	}, owner.Tokens.AccessToken)
	if invalidUpdateRoleRes.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid update member role 400, got %d", invalidUpdateRoleRes.Code)
	}

	missingUserRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/members", map[string]any{
		"email": "missing@example.com",
		"role":  "member",
	}, owner.Tokens.AccessToken)
	if missingUserRes.Code != http.StatusNotFound {
		t.Fatalf("expected missing member user 404, got %d", missingUserRes.Code)
	}

	removeMissingRes := performJSON(env.router, http.MethodDelete, "/v1/orgs/"+owner.Organization.ID+"/members/00000000-0000-0000-0000-000000000000", nil, owner.Tokens.AccessToken)
	if removeMissingRes.Code != http.StatusNotFound {
		t.Fatalf("expected missing member remove 404, got %d", removeMissingRes.Code)
	}

	deleteMissingDeviceRes := performJSON(env.router, http.MethodDelete, "/v1/orgs/"+owner.Organization.ID+"/devices/00000000-0000-0000-0000-000000000000", nil, owner.Tokens.AccessToken)
	if deleteMissingDeviceRes.Code != http.StatusNotFound {
		t.Fatalf("expected missing device delete 404, got %d", deleteMissingDeviceRes.Code)
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

func TestIntegrationOwnerCanUpdateOrganization(t *testing.T) {
	env := newIntegrationEnv(t)

	owner := registerUser(t, env.router, "owner@example.com", "Owner Org")
	admin := registerUser(t, env.router, "admin@example.com", "Admin Org")
	member := registerUser(t, env.router, "member@example.com", "Member Org")

	for _, user := range []struct {
		email string
		role  string
	}{
		{email: "admin@example.com", role: "admin"},
		{email: "member@example.com", role: "member"},
	} {
		res := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/members", map[string]any{
			"email": user.email,
			"role":  user.role,
		}, owner.Tokens.AccessToken)
		if res.Code != http.StatusCreated {
			t.Fatalf("expected add %s member 201, got %d", user.role, res.Code)
		}
	}

	adminUpdateRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID, map[string]any{
		"name": "Admin Rename",
	}, admin.Tokens.AccessToken)
	if adminUpdateRes.Code != http.StatusForbidden {
		t.Fatalf("expected admin organization update 403, got %d", adminUpdateRes.Code)
	}

	memberUpdateRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID, map[string]any{
		"name": "Member Rename",
	}, member.Tokens.AccessToken)
	if memberUpdateRes.Code != http.StatusForbidden {
		t.Fatalf("expected member organization update 403, got %d", memberUpdateRes.Code)
	}

	blankUpdateRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID, map[string]any{
		"name": "   ",
	}, owner.Tokens.AccessToken)
	if blankUpdateRes.Code != http.StatusBadRequest {
		t.Fatalf("expected blank organization update 400, got %d", blankUpdateRes.Code)
	}

	updateRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID, map[string]any{
		"name": "Renamed Org",
	}, owner.Tokens.AccessToken)
	if updateRes.Code != http.StatusOK {
		t.Fatalf("expected owner organization update 200, got %d: %s", updateRes.Code, updateRes.Body.String())
	}
	body := decodeBody[organizationBody](t, updateRes)
	if body.Organization.Name != "Renamed Org" || body.Organization.Role != "owner" {
		t.Fatalf("unexpected organization update response: %+v", body.Organization)
	}

	crossOrgUpdateRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+admin.Organization.ID, map[string]any{
		"name": "Cross Org Rename",
	}, owner.Tokens.AccessToken)
	if crossOrgUpdateRes.Code != http.StatusNotFound {
		t.Fatalf("expected cross-organization update 404, got %d", crossOrgUpdateRes.Code)
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

func TestIntegrationCleanupRefreshTokensRemovesExpiredAndRevokedRows(t *testing.T) {
	env := newIntegrationEnv(t)

	owner := registerUser(t, env.router, "owner@example.com", "Owner Org")
	now := time.Now().UTC()
	for _, token := range []struct {
		hash      string
		expiresAt time.Time
		revokedAt *time.Time
	}{
		{hash: "expired", expiresAt: now.Add(-time.Hour)},
		{hash: "revoked", expiresAt: now.Add(time.Hour), revokedAt: &[]time.Time{now.Add(-time.Minute)}[0]},
		{hash: "active", expiresAt: now.Add(time.Hour)},
	} {
		_, err := env.db.Exec(context.Background(), `
			INSERT INTO refresh_tokens (user_id, token_hash, expires_at, revoked_at)
			VALUES ($1, $2, $3, $4)
		`, owner.User.ID, token.hash, token.expiresAt, token.revokedAt)
		if err != nil {
			t.Fatal(err)
		}
	}

	deleted, err := store.New(env.db).CleanupRefreshTokens(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Fatalf("expected 2 cleaned tokens, got %d", deleted)
	}

	var remaining int
	if err := env.db.QueryRow(context.Background(), `SELECT count(*) FROM refresh_tokens WHERE token_hash = 'active'`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("expected active token to remain, got %d", remaining)
	}
}

func TestIntegrationStoreRefreshTokenHelpers(t *testing.T) {
	env := newIntegrationEnv(t)

	owner := registerUser(t, env.router, "owner@example.com", "Owner Org")
	tokenHash := auth.HashToken("store-refresh-token")
	if err := store.New(env.db).SaveRefreshToken(context.Background(), owner.User.ID, tokenHash, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	userID, err := store.New(env.db).RefreshTokenActive(context.Background(), tokenHash)
	if err != nil {
		t.Fatal(err)
	}
	if userID != owner.User.ID {
		t.Fatalf("expected active refresh token user %s, got %s", owner.User.ID, userID)
	}

	if err := store.New(env.db).RevokeUserRefreshTokens(context.Background(), owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.New(env.db).RefreshTokenActive(context.Background(), tokenHash); err == nil {
		t.Fatal("expected revoked refresh token to be inactive")
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

	disableRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/members/"+owner.User.ID+"/disable", nil, owner.Tokens.AccessToken)
	if disableRes.Code != http.StatusConflict {
		t.Fatalf("expected last owner disable 409, got %d", disableRes.Code)
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

	if _, err := env.db.Exec(context.Background(), `
		INSERT INTO organizations (name) VALUES ('Ownerless Org')
	`); err == nil {
		t.Fatal("expected database to reject organization without owner")
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

func TestIntegrationProvisioningEndpoints(t *testing.T) {
	env := newIntegrationEnv(t)

	owner := registerUser(t, env.router, "owner@example.com", "Owner Org")
	admin := registerUser(t, env.router, "admin@example.com", "Admin Org")
	member := registerUser(t, env.router, "member@example.com", "Member Org")
	outsider := registerUser(t, env.router, "outsider@example.com", "Outsider Org")

	for _, membership := range []struct {
		email string
		role  string
	}{
		{email: "admin@example.com", role: "admin"},
		{email: "member@example.com", role: "member"},
	} {
		res := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/members", map[string]any{
			"email": membership.email,
			"role":  membership.role,
		}, owner.Tokens.AccessToken)
		if res.Code != http.StatusCreated {
			t.Fatalf("expected add member %s 201, got %d: %s", membership.email, res.Code, res.Body.String())
		}
	}

	deviceRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices", devicePayload("provision-device", "PROVISION-001"), owner.Tokens.AccessToken)
	if deviceRes.Code != http.StatusCreated {
		t.Fatalf("expected create device 201, got %d: %s", deviceRes.Code, deviceRes.Body.String())
	}
	device := decodeBody[deviceBody](t, deviceRes)

	memberProvisionRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/provision", map[string]any{
		"video_cloud_devid": "video-device-1",
		"activity_id":       "activity-1",
		"clip_public_key":   "clip-key-1",
	}, member.Tokens.AccessToken)
	if memberProvisionRes.Code != http.StatusForbidden {
		t.Fatalf("expected member provision 403, got %d", memberProvisionRes.Code)
	}

	provisionRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/provision", map[string]any{
		"video_cloud_devid": "video-device-1",
		"activity_id":       "activity-1",
		"clip_public_key":   "clip-key-1",
		"operation_id":      "provision-op-1",
	}, owner.Tokens.AccessToken)
	if provisionRes.Code != http.StatusCreated {
		t.Fatalf("expected provision 201, got %d: %s", provisionRes.Code, provisionRes.Body.String())
	}
	provisioned := decodeBody[operationBody](t, provisionRes)
	if provisioned.Operation.OperationID != "provision-op-1" {
		t.Fatalf("unexpected operation id: %+v", provisioned.Operation)
	}
	if provisioned.Operation.Status != "pending" {
		t.Fatalf("expected pending operation status, got %+v", provisioned.Operation)
	}

	reusedRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/provision", map[string]any{
		"video_cloud_devid": "video-device-1",
		"activity_id":       "activity-1",
		"clip_public_key":   "clip-key-1",
		"operation_id":      "provision-op-1",
	}, owner.Tokens.AccessToken)
	if reusedRes.Code != http.StatusOK {
		t.Fatalf("expected idempotent provision 200, got %d: %s", reusedRes.Code, reusedRes.Body.String())
	}
	reused := decodeBody[operationBody](t, reusedRes)
	if reused.Operation.MessageID != provisioned.Operation.MessageID {
		t.Fatalf("expected reused provision to keep message id, got first=%s second=%s", provisioned.Operation.MessageID, reused.Operation.MessageID)
	}

	conflictRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/provision", map[string]any{
		"video_cloud_devid": "video-device-1",
		"activity_id":       "activity-2",
		"clip_public_key":   "clip-key-1",
		"operation_id":      "provision-op-1",
	}, owner.Tokens.AccessToken)
	if conflictRes.Code != http.StatusConflict {
		t.Fatalf("expected conflicting provision 409, got %d: %s", conflictRes.Code, conflictRes.Body.String())
	}

	var operationCount int
	if err := env.db.QueryRow(context.Background(), `
		SELECT count(*) FROM device_operations WHERE operation_id = 'provision-op-1'
	`).Scan(&operationCount); err != nil {
		t.Fatal(err)
	}
	if operationCount != 1 {
		t.Fatalf("expected one provision operation row, got %d", operationCount)
	}

	var outboxCount int
	if err := env.db.QueryRow(context.Background(), `
		SELECT count(*) FROM device_message_outbox WHERE operation_id = 'provision-op-1'
	`).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if outboxCount != 1 {
		t.Fatalf("expected one outbox row, got %d", outboxCount)
	}

	provisionMessage, err := store.New(env.db).GetLatestOutboxMessageByOperationID(context.Background(), "provision-op-1")
	if err != nil {
		t.Fatal(err)
	}
	provisionPayload := validateAccountCommandEnvelope(t, provisionMessage)
	provisionCommand, ok := provisionPayload.(*channel.DeviceProvisionRequestedPayload)
	if !ok {
		t.Fatalf("expected provision payload type, got %T", provisionPayload)
	}
	if provisionCommand.ActivityID != "activity-1" || provisionCommand.ClipPublicKey != "clip-key-1" || provisionCommand.VideoCloudDevid != "video-device-1" {
		t.Fatalf("unexpected provision command payload: %+v", provisionCommand)
	}

	memberStateRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/provisioning", nil, member.Tokens.AccessToken)
	if memberStateRes.Code != http.StatusOK {
		t.Fatalf("expected member provisioning state 200, got %d: %s", memberStateRes.Code, memberStateRes.Body.String())
	}
	memberState := decodeBody[provisioningBody](t, memberStateRes)
	if memberState.Operation.OperationID != "provision-op-1" {
		t.Fatalf("unexpected provisioning state operation: %+v", memberState.Operation)
	}
	if got := memberState.VideoMetadata[model.DeviceMetadataVideoCloudDevid]; got != "video-device-1" {
		t.Fatalf("expected pending devid in provisioning state, got %+v", got)
	}
	if got := memberState.VideoMetadata[model.DeviceMetadataVideoCloudActivityID]; got != "activity-1" {
		t.Fatalf("expected pending activity id in provisioning state, got %+v", got)
	}
	if got := memberState.VideoMetadata[model.DeviceMetadataVideoCloudActivationStatus]; got != string(model.VideoCloudActivationStatusPending) {
		t.Fatalf("expected pending activation status in provisioning state, got %+v", got)
	}

	pendingDevice, err := store.New(env.db).GetDevice(context.Background(), owner.Organization.ID, device.Device.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pendingDevice.Status != model.DeviceStatusUnknown {
		t.Fatalf("expected accepted provisioning not to set device online, got %s", pendingDevice.Status)
	}

	outsiderStateRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/provisioning", nil, outsider.Tokens.AccessToken)
	if outsiderStateRes.Code != http.StatusNotFound {
		t.Fatalf("expected cross-org provisioning state 404, got %d", outsiderStateRes.Code)
	}

	disableProvisionedRes := performJSON(env.router, http.MethodDelete, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID, nil, owner.Tokens.AccessToken)
	if disableProvisionedRes.Code != http.StatusNoContent {
		t.Fatalf("expected disable provisioned device 204, got %d: %s", disableProvisionedRes.Code, disableProvisionedRes.Body.String())
	}

	reusedDisabledRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/provision", map[string]any{
		"video_cloud_devid": "video-device-1",
		"activity_id":       "activity-1",
		"clip_public_key":   "clip-key-1",
		"operation_id":      "provision-op-1",
	}, owner.Tokens.AccessToken)
	if reusedDisabledRes.Code != http.StatusOK {
		t.Fatalf("expected disabled-device idempotent provision 200, got %d: %s", reusedDisabledRes.Code, reusedDisabledRes.Body.String())
	}
	reusedDisabled := decodeBody[operationBody](t, reusedDisabledRes)
	if reusedDisabled.Operation.MessageID != provisioned.Operation.MessageID {
		t.Fatalf("expected disabled-device retry to keep message id, got first=%s second=%s", provisioned.Operation.MessageID, reusedDisabled.Operation.MessageID)
	}

	disabledDeviceRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices", devicePayload("disabled-device", "PROVISION-002"), owner.Tokens.AccessToken)
	if disabledDeviceRes.Code != http.StatusCreated {
		t.Fatalf("expected disabled fixture device 201, got %d: %s", disabledDeviceRes.Code, disabledDeviceRes.Body.String())
	}
	disabledDevice := decodeBody[deviceBody](t, disabledDeviceRes)
	deleteDisabledRes := performJSON(env.router, http.MethodDelete, "/v1/orgs/"+owner.Organization.ID+"/devices/"+disabledDevice.Device.ID, nil, owner.Tokens.AccessToken)
	if deleteDisabledRes.Code != http.StatusNoContent {
		t.Fatalf("expected disable fixture device 204, got %d", deleteDisabledRes.Code)
	}

	provisionDisabledRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/"+disabledDevice.Device.ID+"/provision", map[string]any{
		"video_cloud_devid": "video-device-disabled",
		"activity_id":       "activity-disabled",
		"clip_public_key":   "clip-key-disabled",
	}, owner.Tokens.AccessToken)
	if provisionDisabledRes.Code != http.StatusConflict {
		t.Fatalf("expected disabled device provision 409, got %d: %s", provisionDisabledRes.Code, provisionDisabledRes.Body.String())
	}

	adminDeviceRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices", devicePayload("admin-provision-device", "PROVISION-003"), owner.Tokens.AccessToken)
	if adminDeviceRes.Code != http.StatusCreated {
		t.Fatalf("expected admin fixture device 201, got %d: %s", adminDeviceRes.Code, adminDeviceRes.Body.String())
	}
	adminDevice := decodeBody[deviceBody](t, adminDeviceRes)

	adminProvisionRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/"+adminDevice.Device.ID+"/provision", map[string]any{
		"video_cloud_devid": "video-device-admin",
		"activity_id":       "activity-admin",
		"clip_public_key":   "clip-key-admin",
	}, admin.Tokens.AccessToken)
	if adminProvisionRes.Code != http.StatusCreated {
		t.Fatalf("expected admin provision 201, got %d: %s", adminProvisionRes.Code, adminProvisionRes.Body.String())
	}
}

func TestIntegrationDeactivateEndpointUsesProjectedVideoMetadata(t *testing.T) {
	env := newIntegrationEnv(t)

	owner := registerUser(t, env.router, "owner@example.com", "Owner Org")
	admin := registerUser(t, env.router, "admin@example.com", "Admin Org")
	member := registerUser(t, env.router, "member@example.com", "Member Org")
	outsider := registerUser(t, env.router, "outsider@example.com", "Outsider Org")
	for _, membership := range []struct {
		email string
		role  string
	}{
		{email: "admin@example.com", role: "admin"},
		{email: "member@example.com", role: "member"},
	} {
		res := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/members", map[string]any{
			"email": membership.email,
			"role":  membership.role,
		}, owner.Tokens.AccessToken)
		if res.Code != http.StatusCreated {
			t.Fatalf("expected add member %s 201, got %d: %s", membership.email, res.Code, res.Body.String())
		}
	}

	deviceRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices", devicePayload("deactivate-device", "DEACTIVATE-001"), owner.Tokens.AccessToken)
	if deviceRes.Code != http.StatusCreated {
		t.Fatalf("expected create device 201, got %d: %s", deviceRes.Code, deviceRes.Body.String())
	}
	device := decodeBody[deviceBody](t, deviceRes)

	projected, err := store.New(env.db).ProjectDevice(context.Background(), owner.Organization.ID, device.Device.ID, store.ProvisionSucceededProjection(channel.DeviceProvisionSucceededPayload{
		OrgID:           owner.Organization.ID,
		AccountDeviceID: device.Device.ID,
		VideoCloudDevid: "video-device-1",
		ActivityID:      "activity-1",
		ActivatedAt:     time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if projected.Metadata[model.DeviceMetadataVideoCloudDevid] != "video-device-1" {
		t.Fatalf("expected projected video metadata, got %+v", projected.Metadata)
	}

	memberDeactivateRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/deactivate", map[string]any{
		"operation_id": "member-deactivate-op-1",
	}, member.Tokens.AccessToken)
	if memberDeactivateRes.Code != http.StatusForbidden {
		t.Fatalf("expected member deactivate 403, got %d", memberDeactivateRes.Code)
	}

	outsiderDeactivateRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/deactivate", map[string]any{
		"operation_id": "outsider-deactivate-op-1",
	}, outsider.Tokens.AccessToken)
	if outsiderDeactivateRes.Code != http.StatusNotFound {
		t.Fatalf("expected cross-org deactivate 404, got %d", outsiderDeactivateRes.Code)
	}

	disableRes := performJSON(env.router, http.MethodDelete, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID, nil, owner.Tokens.AccessToken)
	if disableRes.Code != http.StatusNoContent {
		t.Fatalf("expected disable device 204, got %d: %s", disableRes.Code, disableRes.Body.String())
	}

	deactivateRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/deactivate", map[string]any{
		"operation_id": "deactivate-op-1",
	}, admin.Tokens.AccessToken)
	if deactivateRes.Code != http.StatusCreated {
		t.Fatalf("expected deactivate 201, got %d: %s", deactivateRes.Code, deactivateRes.Body.String())
	}
	deactivated := decodeBody[operationBody](t, deactivateRes)
	if deactivated.Operation.OperationType != "deactivate" {
		t.Fatalf("expected deactivate operation type, got %+v", deactivated.Operation)
	}
	if deactivated.Operation.RequestedBy == nil || *deactivated.Operation.RequestedBy != admin.User.ID {
		t.Fatalf("expected admin requester in operation, got %+v", deactivated.Operation)
	}

	reusedDeactivateRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/deactivate", map[string]any{
		"operation_id": "deactivate-op-1",
	}, admin.Tokens.AccessToken)
	if reusedDeactivateRes.Code != http.StatusOK {
		t.Fatalf("expected idempotent deactivate 200, got %d: %s", reusedDeactivateRes.Code, reusedDeactivateRes.Body.String())
	}
	reusedDeactivate := decodeBody[operationBody](t, reusedDeactivateRes)
	if reusedDeactivate.Operation.MessageID != deactivated.Operation.MessageID {
		t.Fatalf("expected reused deactivate to keep message id, got first=%s second=%s", deactivated.Operation.MessageID, reusedDeactivate.Operation.MessageID)
	}

	conflictDeactivateRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/deactivate", map[string]any{
		"operation_id": "deactivate-op-1",
		"reason":       "user_request",
	}, admin.Tokens.AccessToken)
	if conflictDeactivateRes.Code != http.StatusConflict {
		t.Fatalf("expected conflicting deactivate 409, got %d: %s", conflictDeactivateRes.Code, conflictDeactivateRes.Body.String())
	}

	var messageType string
	var partitionKey string
	var payload []byte
	if err := env.db.QueryRow(context.Background(), `
		SELECT message_type, partition_key, payload
		FROM device_message_outbox
		WHERE operation_id = 'deactivate-op-1'
	`).Scan(&messageType, &partitionKey, &payload); err != nil {
		t.Fatal(err)
	}
	if messageType != "DeviceDeactivateRequested" {
		t.Fatalf("expected deactivate outbox message type, got %s", messageType)
	}
	if partitionKey != device.Device.ID {
		t.Fatalf("expected partition key %s, got %s", device.Device.ID, partitionKey)
	}

	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["video_cloud_devid"] != "video-device-1" {
		t.Fatalf("expected projected video devid in payload, got %+v", decoded)
	}
	if decoded["reason"] != defaultDeactivationReason {
		t.Fatalf("expected default deactivation reason in payload, got %+v", decoded)
	}

	var outboxCount int
	if err := env.db.QueryRow(context.Background(), `
		SELECT count(*) FROM device_message_outbox WHERE operation_id = 'deactivate-op-1'
	`).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if outboxCount != 1 {
		t.Fatalf("expected one deactivate outbox row, got %d", outboxCount)
	}

	deactivateMessage, err := store.New(env.db).GetLatestOutboxMessageByOperationID(context.Background(), "deactivate-op-1")
	if err != nil {
		t.Fatal(err)
	}
	deactivatePayload := validateAccountCommandEnvelope(t, deactivateMessage)
	deactivateCommand, ok := deactivatePayload.(*channel.DeviceDeactivateRequestedPayload)
	if !ok {
		t.Fatalf("expected deactivate payload type, got %T", deactivatePayload)
	}
	if deactivateCommand.VideoCloudDevid != "video-device-1" || deactivateCommand.Reason != defaultDeactivationReason {
		t.Fatalf("unexpected deactivate command payload: %+v", deactivateCommand)
	}

	if _, err := env.db.Exec(context.Background(), `
		UPDATE devices
		SET metadata = '{}'::jsonb
		WHERE id = $1
	`, device.Device.ID); err != nil {
		t.Fatal(err)
	}

	reusedMissingMetadataRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/deactivate", map[string]any{
		"operation_id": "deactivate-op-1",
	}, admin.Tokens.AccessToken)
	if reusedMissingMetadataRes.Code != http.StatusOK {
		t.Fatalf("expected missing-metadata idempotent deactivate 200, got %d: %s", reusedMissingMetadataRes.Code, reusedMissingMetadataRes.Body.String())
	}
	reusedMissingMetadata := decodeBody[operationBody](t, reusedMissingMetadataRes)
	if reusedMissingMetadata.Operation.MessageID != deactivated.Operation.MessageID {
		t.Fatalf("expected missing-metadata retry to keep message id, got first=%s second=%s", deactivated.Operation.MessageID, reusedMissingMetadata.Operation.MessageID)
	}

	missingMetadataRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices", devicePayload("plain-device", "DEACTIVATE-002"), owner.Tokens.AccessToken)
	if missingMetadataRes.Code != http.StatusCreated {
		t.Fatalf("expected plain device 201, got %d: %s", missingMetadataRes.Code, missingMetadataRes.Body.String())
	}
	plainDevice := decodeBody[deviceBody](t, missingMetadataRes)

	deactivateMissingMetadataRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/"+plainDevice.Device.ID+"/deactivate", map[string]any{}, owner.Tokens.AccessToken)
	if deactivateMissingMetadataRes.Code != http.StatusConflict {
		t.Fatalf("expected unprojected deactivate 409, got %d: %s", deactivateMissingMetadataRes.Code, deactivateMissingMetadataRes.Body.String())
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

type meBody struct {
	User struct {
		ID string `json:"id"`
	} `json:"user"`
	Organizations []struct {
		ID string `json:"id"`
	} `json:"organizations"`
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

type organizationBody struct {
	Organization struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Role string `json:"role"`
	} `json:"organization"`
}

type membersBody struct {
	Members []struct {
		UserID     string     `json:"user_id"`
		DisabledAt *time.Time `json:"disabled_at"`
	} `json:"members"`
	Pagination paginationBody `json:"pagination"`
}

type memberBody struct {
	Member struct {
		UserID     string     `json:"user_id"`
		Role       string     `json:"role"`
		DisabledAt *time.Time `json:"disabled_at"`
	} `json:"member"`
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
	return performRaw(router, method, path, payload, accessToken)
}

func performRaw(router *gin.Engine, method, path string, payload []byte, accessToken string) *httptest.ResponseRecorder {
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

func validateAccountCommandEnvelope(t *testing.T, message model.DeviceMessageOutbox) channel.Payload {
	t.Helper()

	payload, err := json.Marshal(message.Payload)
	if err != nil {
		t.Fatalf("marshal outbox payload: %v", err)
	}

	envelope := channel.Envelope{
		MessageID:     message.MessageID,
		CorrelationID: message.CorrelationID,
		OperationID:   message.OperationID,
		SourceService: channel.ServiceAccountManager,
		TargetService: channel.ServiceRealtekVideoCloud,
		MessageType:   channel.MessageType(message.MessageType),
		SchemaVersion: message.SchemaVersion,
		PartitionKey:  message.PartitionKey,
		OccurredAt:    message.CreatedAt.UTC(),
		Payload:       payload,
	}

	decoded, err := envelope.ValidateAndDecode(message.Stream)
	if err != nil {
		t.Fatalf("validate outbox envelope: %v", err)
	}
	return decoded
}
