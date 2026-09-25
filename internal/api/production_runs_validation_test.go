package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

type failingProductionRunStore struct{ Store }

func (failingProductionRunStore) ListProductionRuns(context.Context, string, string, int, int) (store.ProductionRunPage, error) {
	return store.ProductionRunPage{}, errors.New("storage unavailable")
}

func (failingProductionRunStore) StopProductionRunAsUser(context.Context, string, string, string, string) (model.ProductionRun, error) {
	return model.ProductionRun{}, errors.New("storage unavailable")
}

func TestCreateProductionRunRejectsInvalidAuthorizationBeforeStoreWrite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	from := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	validBody := func(quantity int, until time.Time) string {
		return fmt.Sprintf(`{"factory_id":"line-a","batch_id":"batch-a","allowed_quantity":%d,"valid_from":%q,"valid_until":%q}`, quantity, from.Format(time.RFC3339), until.Format(time.RFC3339))
	}
	for _, tc := range []struct {
		name, signer, body, key, errorCode string
		status                             int
	}{
		{"missing signer", "", validBody(10, from.Add(time.Hour)), "intent-1", "production_jwt_signer_unavailable", http.StatusServiceUnavailable},
		{"malformed JSON", "test-secret", `{`, "intent-1", "invalid_request", http.StatusBadRequest},
		{"negative quantity", "test-secret", validBody(-1, from.Add(time.Hour)), "intent-1", "invalid_allowed_quantity", http.StatusBadRequest},
		{"reversed period", "test-secret", validBody(10, from.Add(-time.Hour)), "intent-1", "invalid_production_period", http.StatusBadRequest},
		{"period too long", "test-secret", validBody(10, from.Add(8*24*time.Hour)), "intent-1", "invalid_production_period", http.StatusBadRequest},
		{"unsafe intent", "test-secret", validBody(10, from.Add(time.Hour)), "bad\nintent", "invalid_idempotency_key", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/orgs/cloud-1/device-item-profiles/product-1/production-runs", strings.NewReader(tc.body))
			ctx.Request.Header.Set("Content-Type", "application/json")
			ctx.Request.Header.Set("Idempotency-Key", tc.key)
			(&Server{productionJWTSecret: tc.signer}).createProductionRun(ctx)
			if recorder.Code != tc.status || !strings.Contains(recorder.Body.String(), tc.errorCode) {
				t.Fatalf("status=%d body=%s, want %d %s", recorder.Code, recorder.Body.String(), tc.status, tc.errorCode)
			}
		})
	}
}

func TestProductionRunReadAndStopReportStorageFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := &Server{store: failingProductionRunStore{}}
	for _, tc := range []struct {
		name, method string
		call         func(*gin.Context)
	}{
		{"list", http.MethodGet, server.listOrganizationProductionRuns},
		{"stop", http.MethodPost, server.stopOrganizationProductionRun},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(tc.method, "/v1/orgs/cloud-1/device-item-profiles/product-1/production-runs", nil)
			ctx.Params = gin.Params{{Key: "orgId", Value: "cloud-1"}, {Key: "profileId", Value: "product-1"}, {Key: "runId", Value: "run-1"}}
			ctx.Set("userID", "operator-1")
			tc.call(ctx)
			if recorder.Code != http.StatusInternalServerError || !strings.Contains(recorder.Body.String(), "internal_error") {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}
