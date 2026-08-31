package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

type admittedProductStore struct {
	Store
	ready   chan struct{}
	release <-chan struct{}
}

func (s *admittedProductStore) wait(ctx context.Context) error {
	s.ready <- struct{}{}
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *admittedProductStore) CreateDeviceItemProfileAsUser(ctx context.Context, in store.DeviceItemProfileCreateInput) (model.DeviceItemProfile, error) {
	if err := s.wait(ctx); err != nil {
		return model.DeviceItemProfile{}, err
	}
	return s.Store.CreateDeviceItemProfileAsUser(ctx, in)
}
func (s *admittedProductStore) UpdateDeviceItemProfileAsUser(ctx context.Context, in store.DeviceItemProfileUpdateInput) (model.DeviceItemProfile, error) {
	if err := s.wait(ctx); err != nil {
		return model.DeviceItemProfile{}, err
	}
	return s.Store.UpdateDeviceItemProfileAsUser(ctx, in)
}
func (s *admittedProductStore) DisableDeviceItemProfileAsUser(ctx context.Context, cloud, product, actor string, platform bool) (model.DeviceItemProfile, error) {
	if err := s.wait(ctx); err != nil {
		return model.DeviceItemProfile{}, err
	}
	return s.Store.DisableDeviceItemProfileAsUser(ctx, cloud, product, actor, platform)
}

func TestIntegrationProductWritesReauthorizeAfterAdmission(t *testing.T) {
	for _, route := range []string{"cloud", "platform"} {
		t.Run(route, func(t *testing.T) {
			env := newIntegrationEnv(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			owner := verifiedDeveloperForTest(t, env, "product-http-owner@example.test")
			operator := verifiedDeveloperForTest(t, env, "product-http-operator@example.test")
			p, err := env.store.CreateDeviceItemProfileAsUser(ctx, store.DeviceItemProfileCreateInput{ActorUserID: &owner.UserID, BrandCloudID: owner.BrandCloudID, ProfileKey: "original", DisplayName: "Original", Category: model.DeviceCategoryIPCamera, CAProfile: "ca", IssuerProfile: "issuer", ServiceOptions: []string{"mqtt"}})
			if err != nil {
				t.Fatal(err)
			}
			base := "/v1/orgs/" + owner.BrandCloudID + "/device-item-profiles"
			access := owner.AccessToken
			if route == "platform" {
				if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin=true WHERE id=$1`, operator.UserID); err != nil {
					t.Fatal(err)
				}
				base = "/v1/admin/brand-clouds/" + owner.BrandCloudID + "/device-item-profiles"
				access = operator.AccessToken
			}
			release := make(chan struct{})
			ready := make(chan struct{}, 3)
			env.server.store = &admittedProductStore{Store: env.store, ready: ready, release: release}
			results := make(chan *httptest.ResponseRecorder, 3)
			for _, tc := range []struct{ method, path, body string }{
				{http.MethodPost, base, `{"profile_key":"late","display_name":"Late","category":"ip_camera","ca_profile":"ca","issuer_profile":"issuer","service_options":["mqtt"]}`},
				{http.MethodPatch, base + "/" + p.ID, `{"display_name":"Late"}`},
				{http.MethodPost, base + "/" + p.ID + "/disable", `{}`},
			} {
				req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)).WithContext(ctx)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+access)
				go func() { r := httptest.NewRecorder(); env.router.ServeHTTP(r, req); results <- r }()
				select {
				case <-ready:
				case r := <-results:
					t.Fatalf("request failed before admission: %d %s", r.Code, r.Body)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			if route == "platform" {
				if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin=false WHERE id=$1`, operator.UserID); err != nil {
					t.Fatal(err)
				}
			} else {
				// Synthetic owner switch isolates this authorization boundary; it is
				// not evidence of Billing settlement or production handoff completion.
				tx, err := env.db.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(context.Background())
				if _, err := tx.Exec(ctx, `DELETE FROM organization_members WHERE organization_id=$1 AND user_id=$2`, owner.BrandCloudID, owner.UserID); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role) VALUES($1,$2,'owner')`, owner.BrandCloudID, operator.UserID); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
			}
			close(release)
			for range 3 {
				select {
				case r := <-results:
					if r.Code != http.StatusNotFound {
						t.Fatalf("stale Product mutation: %d %s", r.Code, r.Body)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			got, err := env.store.GetDeviceItemProfile(ctx, owner.BrandCloudID, p.ID)
			if err != nil || got.DisplayName != p.DisplayName || got.Status != p.Status {
				t.Fatalf("Product changed after revocation: %+v %v", got, err)
			}
			var count, audits int
			if err := env.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM device_item_profiles),(SELECT count(*) FROM audit_events WHERE subject_type='device_item_profile')`).Scan(&count, &audits); err != nil || count != 1 || audits != 1 {
				t.Fatalf("stale create/audit: %d/%d %v", count, audits, err)
			}
		})
	}
}
