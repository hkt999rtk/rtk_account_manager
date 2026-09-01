package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

type admittedProductionRunStore struct {
	Store
	ready       chan struct{}
	release     <-chan struct{}
	failSigning bool
	signerCalls int
}

func (s *admittedProductionRunStore) IssueProductionRunAsUser(ctx context.Context, in store.ProductionRunCreateInput, issue store.ProductionRunIssuer) (model.ProductionRun, string, error) {
	close(s.ready)
	select {
	case <-s.release:
	case <-ctx.Done():
		return model.ProductionRun{}, "", ctx.Err()
	}
	return s.Store.IssueProductionRunAsUser(ctx, in, func(run model.ProductionRun, p model.DeviceItemProfile) (string, error) {
		s.signerCalls++
		if s.failSigning {
			return "partial-secret-token", errors.New("sensitive-signing-diagnostic")
		}
		return issue(run, p)
	})
}

func TestIntegrationProductionRunRechecksAdmissionBeforeSigning(t *testing.T) {
	for _, state := range []string{"owner_revoked", "platform_revoked", "product_disabled", "signing_failed"} {
		t.Run(state, func(t *testing.T) {
			env := newIntegrationEnv(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			owner := verifiedDeveloperForTest(t, env, "production-http-owner@example.test")
			operator := verifiedDeveloperForTest(t, env, "production-http-operator@example.test")
			p, err := env.store.CreateDeviceItemProfileAsUser(ctx, store.DeviceItemProfileCreateInput{ActorUserID: &owner.UserID, BrandCloudID: owner.BrandCloudID, ProfileKey: "http-production", DisplayName: "Product", Category: model.DeviceCategoryIPCamera, CAProfile: "ca", IssuerProfile: "issuer", ServiceOptions: []string{"mqtt"}})
			if err != nil {
				t.Fatal(err)
			}
			env.server.ConfigureProductionJWT("isolated-signing-secret", "factory-enroll")
			release := make(chan struct{})
			paused := &admittedProductionRunStore{Store: env.store, ready: make(chan struct{}), release: release, failSigning: state == "signing_failed"}
			env.server.store = paused
			path := "/v1/orgs/" + owner.BrandCloudID + "/device-item-profiles/" + p.ID + "/production-runs"
			access := owner.AccessToken
			if state == "platform_revoked" {
				if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin=true WHERE id=$1`, operator.UserID); err != nil {
					t.Fatal(err)
				}
				path = "/v1/admin/brand-clouds/" + owner.BrandCloudID + "/device-item-profiles/" + p.ID + "/production-runs"
				access = operator.AccessToken
			}
			body := `{"allowed_quantity":10,"valid_from":"` + time.Now().Add(-time.Minute).UTC().Format(time.RFC3339) + `","valid_until":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+access)
			result := make(chan *httptest.ResponseRecorder, 1)
			go func() { r := httptest.NewRecorder(); env.router.ServeHTTP(r, req); result <- r }()
			select {
			case <-paused.ready:
			case r := <-result:
				t.Fatalf("rejected before admission: %d %s", r.Code, r.Body)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			expected := http.StatusNotFound
			switch state {
			case "owner_revoked":
				// Synthetic owner change isolates HTTP admission from Billing protocol.
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
			case "platform_revoked":
				if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin=false WHERE id=$1`, operator.UserID); err != nil {
					t.Fatal(err)
				}
			case "product_disabled":
				if _, err := env.store.DisableDeviceItemProfileAsUser(ctx, owner.BrandCloudID, p.ID, owner.UserID, false); err != nil {
					t.Fatal(err)
				}
				expected = http.StatusConflict
			case "signing_failed":
				expected = http.StatusInternalServerError
			}
			close(release)
			select {
			case r := <-result:
				if r.Code != expected {
					t.Fatalf("status=%d want=%d: %s", r.Code, expected, r.Body)
				}
				if strings.Contains(r.Body.String(), "partial-secret-token") || strings.Contains(r.Body.String(), "sensitive-signing") || strings.Contains(r.Body.String(), "factory_jwt") {
					t.Fatal("failed response leaked signing result")
				}
				if state == "signing_failed" && !strings.Contains(r.Body.String(), "production_jwt_sign_failed") {
					t.Fatal("missing stable signing failure code")
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			wantCalls := 0
			if state == "signing_failed" {
				wantCalls = 1
			}
			if paused.signerCalls != wantCalls {
				t.Fatalf("unexpected signer calls: %d", paused.signerCalls)
			}
			var runs, audits int
			if err := env.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM factory_production_runs),(SELECT count(*) FROM audit_events WHERE subject_type='factory_production_run')`).Scan(&runs, &audits); err != nil || runs != 0 || audits != 0 {
				t.Fatalf("partial issuance: %d/%d %v", runs, audits, err)
			}
		})
	}
}
