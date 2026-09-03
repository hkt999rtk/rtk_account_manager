package api

import (
	"context"
	"net/http"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
	"strings"
	"testing"
)

func TestIntegrationCloudViewerScopeAndCollections(t *testing.T) {
	env := newIntegrationEnv(t)
	contract := newResponseContract(t)
	ctx := context.Background()
	owner := verifiedDeveloperForTest(t, env, "web-viewer-owner@example.com")
	viewer := verifiedDeveloperForTest(t, env, "web-viewer@example.com")
	cloud := owner.BrandCloudID
	product := func(key string) model.DeviceItemProfile {
		p, err := env.store.CreateDeviceItemProfile(ctx, store.DeviceItemProfileCreateInput{ActorUserID: &owner.UserID, BrandCloudID: cloud, ProfileKey: key, DisplayName: key, Category: model.DeviceCategoryIPCamera, CAProfile: "ca", IssuerProfile: "issuer", ServiceOptions: []string{"video_streaming"}})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	p1, p2 := product("visible"), product("hidden")
	scope := map[string]any{"kind": "selected_products", "product_ids": []string{p1.ID}}
	base := "/v1/developer/brand-clouds/" + cloud
	legacyAdd := performJSON(env.router, http.MethodPost, "/v1/orgs/"+cloud+"/members", map[string]any{"email": "web-viewer@example.com", "role": "member"}, owner.AccessToken)
	if legacyAdd.Code != http.StatusConflict {
		t.Fatalf("generic member API bypassed invitation: %d %s", legacyAdd.Code, legacyAdd.Body.String())
	}
	for _, body := range []map[string]any{
		{"email": "web-viewer@example.com", "role": "viewer"},
		{"email": "web-viewer@example.com", "role": "member", "access_scope": scope},
		{"email": "web-viewer@example.com", "role": "member", "access_scope": nil},
		{"role": "viewer", "access_scope": scope},
		{"email": "invalid", "role": "viewer", "access_scope": scope},
		{"email": "web-viewer@example.com", "role": "viewer", "access_scope": map[string]any{"kind": "all_products", "product_ids": nil}},
	} {
		res := performJSON(env.router, http.MethodPost, base+"/members/invitations", body, owner.AccessToken)
		if res.Code != http.StatusBadRequest {
			t.Fatalf("invalid invite status=%d %s", res.Code, res.Body.String())
		}
	}
	res := performJSON(env.router, http.MethodPost, base+"/members/invitations", map[string]any{"email": "web-viewer@example.com", "role": "viewer", "access_scope": scope}, owner.AccessToken)
	if res.Code != http.StatusAccepted {
		t.Fatalf("invite: %d %s", res.Code, res.Body.String())
	}
	contract.validate(t, http.MethodPost, base+"/members/invitations", res)
	token := latestAuthToken(t, env.tokenObserver, "web-viewer@example.com", "brand_cloud_membership_invitation")
	res = performJSON(env.router, http.MethodPost, "/v1/developer/brand-cloud-member-invitations/accept", map[string]any{"token": token}, viewer.AccessToken)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "selected_products") {
		t.Fatalf("accept: %d %s", res.Code, res.Body.String())
	}
	contract.validate(t, http.MethodPost, "/v1/developer/brand-cloud-member-invitations/accept", res)
	ownerAssign := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+cloud+"/members/"+viewer.UserID, map[string]any{"role": "owner"}, owner.AccessToken)
	if ownerAssign.Code != http.StatusConflict {
		t.Fatalf("generic role API bypassed handoff: %d %s", ownerAssign.Code, ownerAssign.Body.String())
	}
	for _, role := range []any{nil, ""} {
		res := performJSON(env.router, http.MethodPatch, base+"/members/"+viewer.UserID, map[string]any{"role": role, "access_scope": scope}, owner.AccessToken)
		if res.Code != http.StatusBadRequest {
			t.Fatalf("invalid role accepted by scope PATCH: %d %s", res.Code, res.Body.String())
		}
	}
	device := func(p model.DeviceItemProfile) model.Device {
		d, err := env.store.CreateDevice(ctx, cloud, store.DeviceInput{Name: p.ProfileKey, Category: model.DeviceCategoryIPCamera})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.db.Exec(ctx, `UPDATE devices SET device_item_profile_id=$2 WHERE id=$1`, d.ID, p.ID); err != nil {
			t.Fatal(err)
		}
		return d
	}
	d1, d2 := device(p1), device(p2)
	group := func(name string, ids ...string) model.DeviceGroup {
		g, err := env.store.CreateDeviceGroup(ctx, cloud, store.DeviceGroupInput{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range ids {
			if err := env.store.AddDeviceToGroup(ctx, cloud, g.ID, id); err != nil {
				t.Fatal(err)
			}
		}
		return g
	}
	mixed, hidden := group("mixed", d1.ID, d2.ID), group("hidden-only", d2.ID)
	for _, pair := range []struct{ id, tag string }{{d1.ID, "shared-tag"}, {d2.ID, "shared-tag"}, {d2.ID, "secret-tag"}} {
		if _, err := env.store.AddDeviceTag(ctx, cloud, pair.id, pair.tag); err != nil {
			t.Fatal(err)
		}
	}
	for _, url := range []string{"/v1/orgs/" + cloud + "/device-item-profiles", "/v1/orgs/" + cloud + "/devices", "/v1/orgs/" + cloud + "/device-groups", "/v1/orgs/" + cloud + "/device-groups/" + mixed.ID + "/devices", "/v1/orgs/" + cloud + "/tags"} {
		res := performJSON(env.router, http.MethodGet, url, nil, viewer.AccessToken)
		if res.Code != http.StatusOK || strings.Contains(res.Body.String(), d2.ID) || strings.Contains(res.Body.String(), "hidden-only") || strings.Contains(res.Body.String(), "secret-tag") || !strings.Contains(res.Body.String(), `"total":1`) {
			t.Fatalf("scope leak %s: %d %s", url, res.Code, res.Body.String())
		}
	}
	res = performJSON(env.router, http.MethodGet, "/v1/orgs/"+cloud+"/device-groups/"+mixed.ID, nil, viewer.AccessToken)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"device_count":1`) {
		t.Fatalf("group count leaked: %d %s", res.Code, res.Body.String())
	}
	if _, err := env.store.ListDeviceGroupAggregatesForUser(ctx, cloud, viewer.UserID, 50, 0); err != nil {
		t.Fatalf("list scoped group aggregates: %v", err)
	}
	res = performJSON(env.router, http.MethodGet, "/v1/orgs/"+cloud+"/device-groups/aggregates", nil, viewer.AccessToken)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"member_count":1`) || strings.Contains(res.Body.String(), hidden.ID) {
		t.Fatalf("group aggregate leaked: %d %s", res.Code, res.Body.String())
	}
	res = performJSON(env.router, http.MethodGet, "/v1/orgs/"+cloud+"/device-groups/"+hidden.ID, nil, viewer.AccessToken)
	if res.Code != http.StatusNotFound {
		t.Fatalf("hidden group: %d %s", res.Code, res.Body.String())
	}
	// Platform capability is independent: tenant endpoints still honor viewer.
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin=true WHERE id=$1`, viewer.UserID); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/device-item-profiles", "/devices", "/fleet/devices", "/fleet/summary"} {
		res := performJSON(env.router, http.MethodGet, "/v1/orgs/"+cloud+path, nil, viewer.AccessToken)
		if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"total":1`) || strings.Contains(res.Body.String(), d2.ID) {
			t.Fatalf("platform flag bypassed read scope %s: %d %s", path, res.Code, res.Body.String())
		}
	}
	res = performJSON(env.router, http.MethodPatch, "/v1/orgs/"+cloud+"/device-item-profiles/"+p1.ID, map[string]any{"display_name": "not permitted"}, viewer.AccessToken)
	if res.Code != http.StatusForbidden {
		t.Fatalf("platform flag bypassed viewer: %d %s", res.Code, res.Body.String())
	}
	res = performJSON(env.router, http.MethodPatch, base+"/members/"+viewer.UserID, map[string]any{"access_scope": map[string]any{"kind": "all_products"}}, owner.AccessToken)
	if res.Code != http.StatusOK {
		t.Fatalf("scope PATCH: %d %s", res.Code, res.Body.String())
	}
	contract.validate(t, http.MethodPatch, base+"/members/"+viewer.UserID, res)
	res = performJSON(env.router, http.MethodGet, "/v1/orgs/"+cloud+"/device-item-profiles", nil, viewer.AccessToken)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"total":2`) {
		t.Fatalf("all scope: %d %s", res.Code, res.Body.String())
	}
}
