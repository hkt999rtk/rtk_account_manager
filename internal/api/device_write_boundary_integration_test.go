package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

type admittedDeviceRequestBody struct {
	ctx     context.Context
	ready   chan struct{}
	release <-chan struct{}
	once    sync.Once
	reader  io.Reader
}

func (b *admittedDeviceRequestBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.ready) })
	select {
	case <-b.release:
		return b.reader.Read(p)
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	}
}

func TestIntegrationDeviceWritesReauthorizeAfterMiddlewareAdmission(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	owner := verifiedDeveloperForTest(t, env, "device-http-owner@example.test")
	target := verifiedDeveloperForTest(t, env, "device-http-target@example.test")
	d, err := env.store.CreateDevice(ctx, owner.BrandCloudID, store.DeviceInput{Name: "Original", Category: model.DeviceCategoryIPCamera, Metadata: map[string]any{"video_cloud_devid": "http-lifecycle-device"}})
	if err != nil {
		t.Fatal(err)
	}
	base := "/v1/orgs/" + owner.BrandCloudID + "/devices"
	release := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, 5)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, base, `{"name":"Late creation","category":"ip_camera"}`},
		{http.MethodPatch, base + "/" + d.ID, `{"name":"Late update","category":"ip_camera"}`},
		{http.MethodPatch, base + "/" + d.ID + "/status", `{"status":"online"}`},
		{http.MethodPost, base + "/" + d.ID + "/provision", `{"video_cloud_devid":"http-lifecycle-device","activity_id":"activity","clip_public_key":"key"}`},
		{http.MethodPost, base + "/" + d.ID + "/deactivate", `{"reason":"user_request"}`},
	} {
		body := &admittedDeviceRequestBody{ctx: ctx, ready: make(chan struct{}), release: release, reader: strings.NewReader(tc.body)}
		request := httptest.NewRequest(tc.method, tc.path, body).WithContext(ctx)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+owner.AccessToken)
		go func() {
			response := httptest.NewRecorder()
			env.router.ServeHTTP(response, request)
			results <- response
		}()
		// Handler body binding happens only after auth/permission middleware.
		select {
		case <-body.ready:
		case response := <-results:
			t.Fatalf("request failed before admission: %d %s", response.Code, response.Body)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	tx, err := env.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	// Synthetic owner switch tests the HTTP authorization boundary; real Billing
	// prepare/commit/finalize is covered separately, not bypassed in production.
	if _, err := tx.Exec(ctx, `DELETE FROM organization_members WHERE organization_id=$1 AND user_id=$2`, owner.BrandCloudID, owner.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role) VALUES($1,$2,'owner')`, owner.BrandCloudID, target.UserID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	close(release)
	for range 5 {
		select {
		case response := <-results:
			if response.Code != http.StatusNotFound {
				t.Fatalf("stale admitted request wrote after owner switch: %d %s", response.Code, response.Body)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	got, err := env.store.GetDevice(ctx, owner.BrandCloudID, d.ID)
	if err != nil || got.Name != d.Name || got.Status != d.Status {
		t.Fatalf("stale request changed device: %+v %v", got, err)
	}
	var count int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM devices WHERE organization_id=$1`, owner.BrandCloudID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("stale request created device: %d %v", count, err)
	}
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM device_operations WHERE organization_id=$1`, owner.BrandCloudID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("stale request queued lifecycle work: %d %v", count, err)
	}
}
