package api

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

func TestValidProductCollaboratorRole(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, role := range []string{store.ProductEditorRole, store.ProductViewerRole} {
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		if !validProductCollaboratorRole(ctx, role) {
			t.Fatalf("valid role %q was rejected", role)
		}
	}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	if validProductCollaboratorRole(ctx, store.ProductOwnerRole) {
		t.Fatal("owner role was accepted as a collaborator role")
	}
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
}

func TestSplitCSVQuery(t *testing.T) {
	if got, want := splitCSVQuery(" alpha, beta  gamma ,, "), []string{"alpha", "beta", "gamma"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("splitCSVQuery() = %#v, want %#v", got, want)
	}
}

func TestCanonicalServiceOptions(t *testing.T) {
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	if got, ok := canonicalServiceOptions(ctx, []string{" mqtt ", "video_storage"}); !ok || !reflect.DeepEqual(got, []string{"mqtt", "video_storage"}) {
		t.Fatalf("canonicalServiceOptions() = %#v, %v", got, ok)
	}
	for name, raw := range map[string][]string{
		"empty":     nil,
		"invalid":   {"email"},
		"duplicate": {"mqtt", "mqtt"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			if _, ok := canonicalServiceOptions(ctx, raw); ok {
				t.Fatal("expected invalid service options")
			}
		})
	}
}

func TestParseDeviceClaimTokenCategoryRejectsUnknownValue(t *testing.T) {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	if category, ok := parseDeviceClaimTokenCategory(ctx, "camera"); ok || category != model.DeviceCategory("") {
		t.Fatalf("category = %q, ok=%v", category, ok)
	}
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
}
