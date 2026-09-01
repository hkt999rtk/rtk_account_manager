package api

import (
	"context"
	"net/http"
	"testing"

	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

func TestIntegrationProductDeviceDisplay(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()
	owner := verifiedDeveloperForTest(t, env, "display-api@example.test")
	p, err := env.store.CreateDeviceItemProfile(ctx, store.DeviceItemProfileCreateInput{ActorUserID: &owner.UserID, BrandCloudID: owner.BrandCloudID, ProfileKey: "display-api", DisplayName: "Display", Category: model.DeviceCategoryIPCamera, CAProfile: "ca", IssuerProfile: "issuer", ServiceOptions: []string{"mqtt"}})
	if err != nil {
		t.Fatal(err)
	}
	d, err := env.store.CreateDevice(ctx, owner.BrandCloudID, store.DeviceInput{Name: "Original", Category: model.DeviceCategoryIPCamera, Metadata: map[string]any{"clip_public_key": "preserve"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = env.db.Exec(ctx, `UPDATE devices SET device_item_profile_id=$2 WHERE id=$1`, d.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	path := "/v1/orgs/" + owner.BrandCloudID + "/device-item-profiles/" + p.ID + "/devices/" + d.ID + "/display"
	for _, body := range []string{`{}`, `{"name":null}`, `{"name":"good","model":null}`, `{"name":"a","name":"b"}`, `{"metadata":{}}`, `{"product_id":"forged"}`, `{"name":""}`} {
		r := performRaw(env.router, http.MethodPatch, path, []byte(body), owner.AccessToken)
		if r.Code != 400 {
			t.Fatalf("invalid input %d: %s", r.Code, r.Body)
		}
	}
	r := performJSON(env.router, http.MethodPatch, path, map[string]any{"name": "Updated", "model": ""}, owner.AccessToken)
	if r.Code != 200 {
		t.Fatalf("display update %d: %s", r.Code, r.Body)
	}
	newResponseContract(t).validate(t, http.MethodPatch, path, r)
	got, err := env.store.GetDevice(ctx, owner.BrandCloudID, d.ID)
	if err != nil || got.Name != "Updated" || got.Metadata["clip_public_key"] != "preserve" {
		t.Fatalf("readback: %+v %v", got, err)
	}
	wrong := "/v1/orgs/" + owner.BrandCloudID + "/device-item-profiles/" + owner.BrandCloudID + "/devices/" + d.ID + "/display"
	if r = performJSON(env.router, http.MethodPatch, wrong, map[string]any{"name": "wrong Product"}, owner.AccessToken); r.Code != 404 {
		t.Fatalf("wrong Product status %d", r.Code)
	}
}
