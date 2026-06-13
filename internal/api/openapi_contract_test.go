package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

func TestIntegrationResponsesMatchOpenAPIContract(t *testing.T) {
	env := newIntegrationEnv(t)
	contract := newResponseContract(t)

	signupRes := performJSON(env.router, http.MethodPost, "/v1/auth/signup", map[string]any{
		"email":             "contract-signup@example.com",
		"password":          "password123",
		"display_name":      "Contract Signup",
		"organization_name": "Contract Signup Org",
	}, "")
	contract.validate(t, http.MethodPost, "/v1/auth/signup", signupRes)
	signupBody := decodeBody[signupBody](t, signupRes)

	registered := registerUser(t, env.router, "contract-owner@example.com", "Contract Org")
	registerRes := performJSON(env.router, http.MethodPost, "/v1/auth/login", map[string]any{
		"email":    "contract-owner@example.com",
		"password": "password123",
	}, "")
	contract.validate(t, http.MethodPost, "/v1/auth/login", registerRes)

	changePasswordRes := performJSON(env.router, http.MethodPatch, "/v1/me/password", map[string]any{
		"current_password": "password123",
		"new_password":     "contract-password123",
	}, registered.Tokens.AccessToken)
	contract.validate(t, http.MethodPatch, "/v1/me/password", changePasswordRes)

	orgUpdateRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+registered.Organization.ID, map[string]any{
		"name": "Contract Org Updated",
	}, registered.Tokens.AccessToken)
	contract.validate(t, http.MethodPatch, "/v1/orgs/"+registered.Organization.ID, orgUpdateRes)

	deviceRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+registered.Organization.ID+"/devices", devicePayload("contract-device", "CONTRACT-001"), registered.Tokens.AccessToken)
	contract.validate(t, http.MethodPost, "/v1/orgs/"+registered.Organization.ID+"/devices", deviceRes)
	device := decodeBody[deviceBody](t, deviceRes)

	groupRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+registered.Organization.ID+"/device-groups", map[string]any{
		"name": "Contract Group",
	}, registered.Tokens.AccessToken)
	contract.validate(t, http.MethodPost, "/v1/orgs/"+registered.Organization.ID+"/device-groups", groupRes)
	group := decodeBody[deviceGroupBody](t, groupRes)

	groupsRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+registered.Organization.ID+"/device-groups", nil, registered.Tokens.AccessToken)
	contract.validate(t, http.MethodGet, "/v1/orgs/"+registered.Organization.ID+"/device-groups", groupsRes)

	getGroupRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+registered.Organization.ID+"/device-groups/"+group.Group.ID, nil, registered.Tokens.AccessToken)
	contract.validate(t, http.MethodGet, "/v1/orgs/"+registered.Organization.ID+"/device-groups/"+group.Group.ID, getGroupRes)

	groupDevicesRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+registered.Organization.ID+"/device-groups/"+group.Group.ID+"/devices", nil, registered.Tokens.AccessToken)
	contract.validate(t, http.MethodGet, "/v1/orgs/"+registered.Organization.ID+"/device-groups/"+group.Group.ID+"/devices", groupDevicesRes)

	tagRes := performJSON(env.router, http.MethodPut, "/v1/orgs/"+registered.Organization.ID+"/devices/"+device.Device.ID+"/tags/contract", nil, registered.Tokens.AccessToken)
	contract.validate(t, http.MethodPut, "/v1/orgs/"+registered.Organization.ID+"/devices/"+device.Device.ID+"/tags/contract", tagRes)

	tagsRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+registered.Organization.ID+"/devices/"+device.Device.ID+"/tags", nil, registered.Tokens.AccessToken)
	contract.validate(t, http.MethodGet, "/v1/orgs/"+registered.Organization.ID+"/devices/"+device.Device.ID+"/tags", tagsRes)

	registryOnlyStateRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+registered.Organization.ID+"/devices/"+device.Device.ID+"/provisioning", nil, registered.Tokens.AccessToken)
	contract.validate(t, http.MethodGet, "/v1/orgs/"+registered.Organization.ID+"/devices/"+device.Device.ID+"/provisioning", registryOnlyStateRes)

	registeredOrgID := registered.Organization.ID
	if _, err := store.New(env.db).CreateDeviceClaimToken(context.Background(), store.DeviceClaimTokenCreateInput{
		OrganizationID:  &registeredOrgID,
		TokenHash:       auth.HashToken("contract-claim-token"),
		Category:        model.DeviceCategoryIPCamera,
		VideoCloudDevid: "contract-claim-video-1",
		ActivityID:      "contract-claim-activity-1",
		ClipPublicKey:   "contract-claim-clip-key-1",
		ServiceOptions:  []string{"video_streaming", "video_storage"},
		ExpiresAt:       time.Now().UTC().Add(24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	claimResolveRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+registered.Organization.ID+"/devices/claim/resolve", map[string]any{
		"claim_token": "contract-claim-token",
		"device_name": "Contract Claimed Camera",
	}, registered.Tokens.AccessToken)
	contract.validate(t, http.MethodPost, "/v1/orgs/"+registered.Organization.ID+"/devices/claim/resolve", claimResolveRes)
	claimResolved := decodeBody[claimResolveBody](t, claimResolveRes)

	if _, err := store.New(env.db).CreateDeviceClaimToken(context.Background(), store.DeviceClaimTokenCreateInput{
		OrganizationID:  &registeredOrgID,
		TokenHash:       auth.HashToken("contract-unprovision-claim-token"),
		Category:        model.DeviceCategoryMQTT,
		VideoCloudDevid: "contract-unprovision-video-1",
		ActivityID:      "contract-unprovision-activity-1",
		ClipPublicKey:   "contract-unprovision-clip-key-1",
		ServiceOptions:  []string{"mqtt"},
		ExpiresAt:       time.Now().UTC().Add(24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	unprovisionClaimRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+registered.Organization.ID+"/devices/claim/resolve", map[string]any{
		"claim_token": "contract-unprovision-claim-token",
		"device_name": "Contract Unprovision Device",
	}, registered.Tokens.AccessToken)
	contract.validate(t, http.MethodPost, "/v1/orgs/"+registered.Organization.ID+"/devices/claim/resolve", unprovisionClaimRes)
	unprovisionClaim := decodeBody[claimResolveBody](t, unprovisionClaimRes)
	unprovisionRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+registered.Organization.ID+"/devices/"+unprovisionClaim.Device.ID+"/unprovision", map[string]any{
		"reason": "contract resale",
	}, registered.Tokens.AccessToken)
	contract.validate(t, http.MethodPost, "/v1/orgs/"+registered.Organization.ID+"/devices/"+unprovisionClaim.Device.ID+"/unprovision", unprovisionRes)

	verifyToken := latestAuthToken(t, env.tokenSink, "contract-signup@example.com", "email_verification")
	verifyRes := performJSON(env.router, http.MethodPost, "/v1/auth/verify-email", map[string]any{
		"token": verifyToken,
	}, "")
	contract.validate(t, http.MethodPost, "/v1/auth/verify-email", verifyRes)
	signupLoginRes := performJSON(env.router, http.MethodPost, "/v1/auth/login", map[string]any{
		"email":    "contract-signup@example.com",
		"password": "password123",
	}, "")
	contract.validate(t, http.MethodPost, "/v1/auth/login", signupLoginRes)
	signupLoginBody := decodeBody[tokenBody](t, signupLoginRes)

	raiseReqRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+signupBody.Organization.ID+"/quota-raise-requests", map[string]any{
		"requested_quota": 8,
		"use_case":        "contract test",
		"contact_info": map[string]any{
			"email": "contract@example.com",
		},
	}, signupLoginBody.Tokens.AccessToken)
	contract.validate(t, http.MethodPost, "/v1/orgs/"+signupBody.Organization.ID+"/quota-raise-requests", raiseReqRes)
	raiseReqBody := decodeBody[quotaRaiseRequestBody](t, raiseReqRes)

	admin := registerUser(t, env.router, "contract-platform-admin@example.com", "Contract Admin Org")
	if _, err := env.db.Exec(context.Background(), `UPDATE users SET platform_admin = true WHERE id = $1`, admin.User.ID); err != nil {
		t.Fatal(err)
	}
	adminClaimTokenRes := performJSON(env.router, http.MethodPost, "/v1/admin/device-claim-tokens", map[string]any{
		"organization_id":   registered.Organization.ID,
		"category":          "ip_camera",
		"video_cloud_devid": "contract-admin-claim-video-1",
		"activity_id":       "contract-admin-claim-activity-1",
		"clip_public_key":   "contract-admin-claim-clip-key-1",
		"service_options":   []string{"video_streaming", "video_storage"},
		"expires_at":        time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339Nano),
	}, admin.Tokens.AccessToken)
	contract.validate(t, http.MethodPost, "/v1/admin/device-claim-tokens", adminClaimTokenRes)
	adminClaimTokenBody := decodeBody[deviceClaimTokenAdminBody](t, adminClaimTokenRes)
	if _, err := store.New(env.db).CreateDeviceClaimToken(context.Background(), store.DeviceClaimTokenCreateInput{
		OrganizationID:  &registeredOrgID,
		TokenHash:       auth.HashToken("contract-admin-unprovision-claim-token"),
		Category:        model.DeviceCategoryIPCamera,
		VideoCloudDevid: "contract-admin-unprovision-video-1",
		ActivityID:      "contract-admin-unprovision-activity-1",
		ClipPublicKey:   "contract-admin-unprovision-clip-key-1",
		ServiceOptions:  []string{"video_streaming", "video_storage"},
		ExpiresAt:       time.Now().UTC().Add(24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	adminUnprovisionClaimRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+registered.Organization.ID+"/devices/claim/resolve", map[string]any{
		"claim_token": "contract-admin-unprovision-claim-token",
		"device_name": "Contract Admin Unprovision Device",
	}, registered.Tokens.AccessToken)
	contract.validate(t, http.MethodPost, "/v1/orgs/"+registered.Organization.ID+"/devices/claim/resolve", adminUnprovisionClaimRes)
	adminUnprovisionClaim := decodeBody[claimResolveBody](t, adminUnprovisionClaimRes)
	adminUnprovisionRes := performJSON(env.router, http.MethodPost, "/v1/admin/devices/"+adminUnprovisionClaim.Device.ID+"/unprovision", map[string]any{
		"reason":   "contract support unprovision",
		"evidence": map[string]any{"ticket": "CONTRACT-UNPROVISION"},
	}, admin.Tokens.AccessToken)
	contract.validate(t, http.MethodPost, "/v1/admin/devices/"+adminUnprovisionClaim.Device.ID+"/unprovision", adminUnprovisionRes)
	adminIDPCreateRes := performJSON(env.router, http.MethodPost, "/v1/admin/identity-providers", map[string]any{
		"provider_id":       "contract-keycloak",
		"name":              "Contract Keycloak",
		"issuer_url":        "https://contract-sso.example.test/realms/account",
		"client_id":         "contract-account-manager",
		"client_secret_ref": "env:CONTRACT_OIDC_SECRET",
		"scopes":            []string{"openid", "email", "profile"},
		"enabled":           false,
		"metadata":          map[string]any{"contract": true},
	}, admin.Tokens.AccessToken)
	contract.validate(t, http.MethodPost, "/v1/admin/identity-providers", adminIDPCreateRes)
	adminIDPListRes := performJSON(env.router, http.MethodGet, "/v1/admin/identity-providers", nil, admin.Tokens.AccessToken)
	contract.validate(t, http.MethodGet, "/v1/admin/identity-providers", adminIDPListRes)
	adminIDPGetRes := performJSON(env.router, http.MethodGet, "/v1/admin/identity-providers/contract-keycloak", nil, admin.Tokens.AccessToken)
	contract.validate(t, http.MethodGet, "/v1/admin/identity-providers/contract-keycloak", adminIDPGetRes)
	adminIDPPatchRes := performJSON(env.router, http.MethodPatch, "/v1/admin/identity-providers/contract-keycloak", map[string]any{
		"name": "Contract Keycloak Updated",
	}, admin.Tokens.AccessToken)
	contract.validate(t, http.MethodPatch, "/v1/admin/identity-providers/contract-keycloak", adminIDPPatchRes)
	adminIDPDeleteRes := performJSON(env.router, http.MethodDelete, "/v1/admin/identity-providers/contract-keycloak", nil, admin.Tokens.AccessToken)
	contract.validate(t, http.MethodDelete, "/v1/admin/identity-providers/contract-keycloak", adminIDPDeleteRes)
	brandCloudCreateRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds", map[string]any{
		"name":     "Contract Brand Cloud",
		"metadata": map[string]any{"contract": true},
	}, admin.Tokens.AccessToken)
	contract.validate(t, http.MethodPost, "/v1/admin/brand-clouds", brandCloudCreateRes)
	brandCloudResp := decodeBody[brandCloudBody](t, brandCloudCreateRes)
	brandCloudListRes := performJSON(env.router, http.MethodGet, "/v1/admin/brand-clouds", nil, admin.Tokens.AccessToken)
	contract.validate(t, http.MethodGet, "/v1/admin/brand-clouds", brandCloudListRes)
	brandCloudGetRes := performJSON(env.router, http.MethodGet, "/v1/admin/brand-clouds/"+brandCloudResp.BrandCloud.ID, nil, admin.Tokens.AccessToken)
	contract.validate(t, http.MethodGet, "/v1/admin/brand-clouds/"+brandCloudResp.BrandCloud.ID, brandCloudGetRes)
	deviceItemProfileCreateRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds/"+brandCloudResp.BrandCloud.ID+"/device-item-profiles", map[string]any{
		"profile_key":       "contract-cam-v1",
		"display_name":      "Contract Camera V1",
		"category":          "ip_camera",
		"manufacturer":      "Realtek",
		"model":             "CONTRACT-100",
		"metadata_defaults": map[string]any{"region": "tw"},
		"metadata_schema":   map[string]any{"type": "object"},
		"ca_profile":        "contract-ca",
		"issuer_profile":    "contract-issuer",
		"service_options":   []string{"video_streaming", "video_storage"},
	}, admin.Tokens.AccessToken)
	contract.validate(t, http.MethodPost, "/v1/admin/brand-clouds/"+brandCloudResp.BrandCloud.ID+"/device-item-profiles", deviceItemProfileCreateRes)
	deviceItemProfileBody := decodeBody[deviceItemProfileBody](t, deviceItemProfileCreateRes)
	deviceItemProfilesRes := performJSON(env.router, http.MethodGet, "/v1/admin/brand-clouds/"+brandCloudResp.BrandCloud.ID+"/device-item-profiles", nil, admin.Tokens.AccessToken)
	contract.validate(t, http.MethodGet, "/v1/admin/brand-clouds/"+brandCloudResp.BrandCloud.ID+"/device-item-profiles", deviceItemProfilesRes)
	deviceItemProfileGetRes := performJSON(env.router, http.MethodGet, "/v1/admin/brand-clouds/"+brandCloudResp.BrandCloud.ID+"/device-item-profiles/"+deviceItemProfileBody.DeviceItemProfile.ID, nil, admin.Tokens.AccessToken)
	contract.validate(t, http.MethodGet, "/v1/admin/brand-clouds/"+brandCloudResp.BrandCloud.ID+"/device-item-profiles/"+deviceItemProfileBody.DeviceItemProfile.ID, deviceItemProfileGetRes)
	deviceItemProfilePatchRes := performJSON(env.router, http.MethodPatch, "/v1/admin/brand-clouds/"+brandCloudResp.BrandCloud.ID+"/device-item-profiles/"+deviceItemProfileBody.DeviceItemProfile.ID, map[string]any{
		"display_name": "Contract Camera V1 Rev B",
	}, admin.Tokens.AccessToken)
	contract.validate(t, http.MethodPatch, "/v1/admin/brand-clouds/"+brandCloudResp.BrandCloud.ID+"/device-item-profiles/"+deviceItemProfileBody.DeviceItemProfile.ID, deviceItemProfilePatchRes)
	deviceItemProfileDisableRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds/"+brandCloudResp.BrandCloud.ID+"/device-item-profiles/"+deviceItemProfileBody.DeviceItemProfile.ID+"/disable", nil, admin.Tokens.AccessToken)
	contract.validate(t, http.MethodPost, "/v1/admin/brand-clouds/"+brandCloudResp.BrandCloud.ID+"/device-item-profiles/"+deviceItemProfileBody.DeviceItemProfile.ID+"/disable", deviceItemProfileDisableRes)
	brandCloudPatchRes := performJSON(env.router, http.MethodPatch, "/v1/admin/brand-clouds/"+brandCloudResp.BrandCloud.ID, map[string]any{
		"status": "disabled",
	}, admin.Tokens.AccessToken)
	contract.validate(t, http.MethodPatch, "/v1/admin/brand-clouds/"+brandCloudResp.BrandCloud.ID, brandCloudPatchRes)
	brandCloudUserRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds/"+brandCloudResp.BrandCloud.ID+"/users", map[string]any{
		"email":        "contract-brand-user@example.com",
		"password":     "password123",
		"display_name": "Contract Brand User",
		"role":         "member",
	}, admin.Tokens.AccessToken)
	contract.validate(t, http.MethodPost, "/v1/admin/brand-clouds/"+brandCloudResp.BrandCloud.ID+"/users", brandCloudUserRes)
	brandCloudUserBody := decodeBody[brandCloudUserBody](t, brandCloudUserRes)
	brandCloudMemberRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds/"+brandCloudResp.BrandCloud.ID+"/members", map[string]any{
		"brand_cloud_user_id": brandCloudUserBody.BrandCloudUser.ID,
		"role":                "owner",
	}, admin.Tokens.AccessToken)
	contract.validate(t, http.MethodPost, "/v1/admin/brand-clouds/"+brandCloudResp.BrandCloud.ID+"/members", brandCloudMemberRes)
	brandCloudUsersRes := performJSON(env.router, http.MethodGet, "/v1/admin/brand-clouds/"+brandCloudResp.BrandCloud.ID+"/users", nil, admin.Tokens.AccessToken)
	contract.validate(t, http.MethodGet, "/v1/admin/brand-clouds/"+brandCloudResp.BrandCloud.ID+"/users", brandCloudUsersRes)
	brandCloudReactivateRes := performJSON(env.router, http.MethodPatch, "/v1/admin/brand-clouds/"+brandCloudResp.BrandCloud.ID, map[string]any{
		"status": "active",
	}, admin.Tokens.AccessToken)
	if brandCloudReactivateRes.Code != http.StatusOK {
		t.Fatalf("expected brand cloud reactivate 200, got %d: %s", brandCloudReactivateRes.Code, brandCloudReactivateRes.Body.String())
	}
	brandCloudResp = decodeBody[brandCloudBody](t, brandCloudReactivateRes)
	brandLoginRes := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/"+brandCloudResp.BrandCloud.TenantSlug+"/auth/login", map[string]any{
		"email":    "contract-brand-user@example.com",
		"password": "password123",
	}, "")
	contract.validate(t, http.MethodPost, "/v1/brand-clouds/"+brandCloudResp.BrandCloud.TenantSlug+"/auth/login", brandLoginRes)
	brandLoginBody := decodeBody[brandCloudLoginBody](t, brandLoginRes)
	brandMeRes := performJSON(env.router, http.MethodGet, "/v1/brand-clouds/"+brandCloudResp.BrandCloud.TenantSlug+"/me", nil, brandLoginBody.Tokens.AccessToken)
	contract.validate(t, http.MethodGet, "/v1/brand-clouds/"+brandCloudResp.BrandCloud.TenantSlug+"/me", brandMeRes)
	brandRefreshRes := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/"+brandCloudResp.BrandCloud.TenantSlug+"/auth/refresh", map[string]any{
		"refresh_token": brandLoginBody.Tokens.RefreshToken,
	}, "")
	contract.validate(t, http.MethodPost, "/v1/brand-clouds/"+brandCloudResp.BrandCloud.TenantSlug+"/auth/refresh", brandRefreshRes)
	brandRefreshBody := decodeBody[brandCloudLoginBody](t, brandRefreshRes)
	brandLogoutRes := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/"+brandCloudResp.BrandCloud.TenantSlug+"/auth/logout", map[string]any{
		"refresh_token": brandRefreshBody.Tokens.RefreshToken,
	}, brandRefreshBody.Tokens.AccessToken)
	contract.validate(t, http.MethodPost, "/v1/brand-clouds/"+brandCloudResp.BrandCloud.TenantSlug+"/auth/logout", brandLogoutRes)
	brandCloudUserDisableRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds/"+brandCloudResp.BrandCloud.ID+"/users/"+brandCloudUserBody.BrandCloudUser.ID+"/disable", nil, admin.Tokens.AccessToken)
	contract.validate(t, http.MethodPost, "/v1/admin/brand-clouds/"+brandCloudResp.BrandCloud.ID+"/users/"+brandCloudUserBody.BrandCloudUser.ID+"/disable", brandCloudUserDisableRes)
	brandCloudUserEnableRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds/"+brandCloudResp.BrandCloud.ID+"/users/"+brandCloudUserBody.BrandCloudUser.ID+"/enable", nil, admin.Tokens.AccessToken)
	contract.validate(t, http.MethodPost, "/v1/admin/brand-clouds/"+brandCloudResp.BrandCloud.ID+"/users/"+brandCloudUserBody.BrandCloudUser.ID+"/enable", brandCloudUserEnableRes)
	brandCloudUserApproveRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds/"+brandCloudResp.BrandCloud.ID+"/users/"+brandCloudUserBody.BrandCloudUser.ID+"/approve", nil, admin.Tokens.AccessToken)
	contract.validate(t, http.MethodPost, "/v1/admin/brand-clouds/"+brandCloudResp.BrandCloud.ID+"/users/"+brandCloudUserBody.BrandCloudUser.ID+"/approve", brandCloudUserApproveRes)
	brandCloudUserDeleteRes := performJSON(env.router, http.MethodDelete, "/v1/admin/brand-clouds/"+brandCloudResp.BrandCloud.ID+"/users/"+brandCloudUserBody.BrandCloudUser.ID, nil, admin.Tokens.AccessToken)
	contract.validate(t, http.MethodDelete, "/v1/admin/brand-clouds/"+brandCloudResp.BrandCloud.ID+"/users/"+brandCloudUserBody.BrandCloudUser.ID, brandCloudUserDeleteRes)
	adminClaimTokensRes := performJSON(env.router, http.MethodGet, "/v1/admin/device-claim-tokens", nil, admin.Tokens.AccessToken)
	contract.validate(t, http.MethodGet, "/v1/admin/device-claim-tokens", adminClaimTokensRes)
	adminClaimTokenGetRes := performJSON(env.router, http.MethodGet, "/v1/admin/device-claim-tokens/"+adminClaimTokenBody.DeviceClaimToken.ID, nil, admin.Tokens.AccessToken)
	contract.validate(t, http.MethodGet, "/v1/admin/device-claim-tokens/"+adminClaimTokenBody.DeviceClaimToken.ID, adminClaimTokenGetRes)
	claimTransferRes := performJSON(env.router, http.MethodPost, "/v1/admin/device-claims/"+claimResolved.ClaimID+"/transfer", map[string]any{
		"target_organization_id": admin.Organization.ID,
		"reason":                 "contract support transfer",
		"evidence":               map[string]any{"ticket": "CONTRACT-131"},
	}, admin.Tokens.AccessToken)
	contract.validate(t, http.MethodPost, "/v1/admin/device-claims/"+claimResolved.ClaimID+"/transfer", claimTransferRes)
	if adminClaimTokenBody.ClaimToken == nil {
		t.Fatalf("expected generated claim token for reclaim contract")
	}
	claimForReclaimRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+registered.Organization.ID+"/devices/claim/resolve", map[string]any{
		"claim_token": *adminClaimTokenBody.ClaimToken,
		"device_name": "Contract Reclaim Camera",
	}, registered.Tokens.AccessToken)
	contract.validate(t, http.MethodPost, "/v1/orgs/"+registered.Organization.ID+"/devices/claim/resolve", claimForReclaimRes)
	claimReclaimRes := performJSON(env.router, http.MethodPost, "/v1/admin/device-claim-tokens/"+adminClaimTokenBody.DeviceClaimToken.ID+"/reclaim", map[string]any{
		"target_organization_id": admin.Organization.ID,
		"reason":                 "contract support reclaim",
		"evidence":               map[string]any{"factory_reset": true},
	}, admin.Tokens.AccessToken)
	contract.validate(t, http.MethodPost, "/v1/admin/device-claim-tokens/"+adminClaimTokenBody.DeviceClaimToken.ID+"/reclaim", claimReclaimRes)
	adminClaimTokenRevokeRes := performJSON(env.router, http.MethodPost, "/v1/admin/device-claim-tokens/"+adminClaimTokenBody.DeviceClaimToken.ID+"/revoke", nil, admin.Tokens.AccessToken)
	contract.validate(t, http.MethodPost, "/v1/admin/device-claim-tokens/"+adminClaimTokenBody.DeviceClaimToken.ID+"/revoke", adminClaimTokenRevokeRes)
	quotaRaiseListRes := performJSON(env.router, http.MethodGet, "/v1/admin/quota-raise-requests", nil, admin.Tokens.AccessToken)
	contract.validate(t, http.MethodGet, "/v1/admin/quota-raise-requests", quotaRaiseListRes)
	quotaRaiseShowRes := performJSON(env.router, http.MethodGet, "/v1/admin/quota-raise-requests/"+raiseReqBody.QuotaRaiseRequest.ID, nil, admin.Tokens.AccessToken)
	contract.validate(t, http.MethodGet, "/v1/admin/quota-raise-requests/"+raiseReqBody.QuotaRaiseRequest.ID, quotaRaiseShowRes)
	auditEventsRes := performJSON(env.router, http.MethodGet, "/v1/admin/audit-events", nil, admin.Tokens.AccessToken)
	contract.validate(t, http.MethodGet, "/v1/admin/audit-events", auditEventsRes)
	approveReqRes := performJSON(env.router, http.MethodPost, "/v1/admin/quota-raise-requests/"+raiseReqBody.QuotaRaiseRequest.ID+"/approve", nil, admin.Tokens.AccessToken)
	contract.validate(t, http.MethodPost, "/v1/admin/quota-raise-requests/"+raiseReqBody.QuotaRaiseRequest.ID+"/approve", approveReqRes)

	badDeviceRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+registered.Organization.ID+"/devices", map[string]any{
		"name":     "contract-device",
		"category": "invalid",
	}, registered.Tokens.AccessToken)
	contract.validate(t, http.MethodPost, "/v1/orgs/"+registered.Organization.ID+"/devices", badDeviceRes)

	provisionRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+registered.Organization.ID+"/devices/"+device.Device.ID+"/provision", map[string]any{
		"video_cloud_devid": "contract-video-1",
		"activity_id":       "contract-activity-1",
		"clip_public_key":   "contract-clip-key-1",
		"operation_id":      "contract-provision-op-1",
	}, registered.Tokens.AccessToken)
	contract.validate(t, http.MethodPost, "/v1/orgs/"+registered.Organization.ID+"/devices/"+device.Device.ID+"/provision", provisionRes)

	reusedProvisionRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+registered.Organization.ID+"/devices/"+device.Device.ID+"/provision", map[string]any{
		"video_cloud_devid": "contract-video-1",
		"activity_id":       "contract-activity-1",
		"clip_public_key":   "contract-clip-key-1",
		"operation_id":      "contract-provision-op-1",
	}, registered.Tokens.AccessToken)
	contract.validate(t, http.MethodPost, "/v1/orgs/"+registered.Organization.ID+"/devices/"+device.Device.ID+"/provision", reusedProvisionRes)

	provisioningRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+registered.Organization.ID+"/devices/"+device.Device.ID+"/provisioning", nil, registered.Tokens.AccessToken)
	contract.validate(t, http.MethodGet, "/v1/orgs/"+registered.Organization.ID+"/devices/"+device.Device.ID+"/provisioning", provisioningRes)

	deactivateRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+registered.Organization.ID+"/devices/"+device.Device.ID+"/deactivate", map[string]any{
		"operation_id": "contract-deactivate-op-1",
		"reason":       "contract-test",
	}, registered.Tokens.AccessToken)
	contract.validate(t, http.MethodPost, "/v1/orgs/"+registered.Organization.ID+"/devices/"+device.Device.ID+"/deactivate", deactivateRes)
}

type responseContract struct {
	doc    *openapi3.T
	router routers.Router
}

func newResponseContract(t *testing.T) responseContract {
	t.Helper()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile("../../openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	return responseContract{doc: doc, router: router}
}

func (c responseContract) validate(t *testing.T, method, path string, res *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest(method, "http://localhost:8080"+path, nil)
	req.Header.Set("Content-Type", "application/json")
	route, pathParams, err := c.router.FindRoute(req)
	if err != nil {
		t.Fatal(err)
	}

	input := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{
			Request:    req,
			PathParams: pathParams,
			Route:      route,
			Options: &openapi3filter.Options{
				AuthenticationFunc: openapi3filter.NoopAuthenticationFunc,
			},
		},
		Status: res.Code,
		Header: http.Header{
			"Content-Type": []string{gin.MIMEJSON},
		},
		Body: io.NopCloser(bytes.NewReader(res.Body.Bytes())),
		Options: &openapi3filter.Options{
			AuthenticationFunc: openapi3filter.NoopAuthenticationFunc,
		},
	}
	if err := openapi3filter.ValidateResponse(context.Background(), input); err != nil {
		t.Fatalf("%s %s response %d does not match OpenAPI contract: %v\nbody: %s", method, path, res.Code, err, res.Body.String())
	}
	if res.Code >= 400 && res.Body.Len() == 0 {
		t.Fatal(fmt.Errorf("error response %d must include a body", res.Code))
	}
}
