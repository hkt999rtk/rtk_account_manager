package api

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

const testLabUUID = "11111111-1111-4111-8111-111111111111"

func TestTestLabEnabledOnlyInNonProductionEnvironments(t *testing.T) {
	for _, tc := range []struct {
		name, enabled, environment string
		want                       bool
	}{
		{name: "dev", enabled: "true", environment: "dev", want: true},
		{name: "development", enabled: "TRUE", environment: " Development ", want: true},
		{name: "local", enabled: "true", environment: "local", want: true},
		{name: "staging", enabled: "true", environment: "staging", want: true},
		{name: "production", enabled: "true", environment: "production", want: false},
		{name: "feature disabled", enabled: "false", environment: "dev", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TEST_LAB_ENABLED", tc.enabled)
			t.Setenv("ACCOUNT_MANAGER_ENV", tc.environment)
			if got := testLabEnabled(); got != tc.want {
				t.Fatalf("testLabEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestConfigureTestLabValidatesRuntimeEndpoint(t *testing.T) {
	server := &Server{}
	server.ConfigureTestLab(nil, "https://runtime.example.test/base/", "fixture-token")
	if server.testLab == nil {
		t.Fatal("valid runtime was rejected")
	}
	if server.testLab.origin != "https://runtime.example.test/base" || server.testLab.token != "fixture-token" {
		t.Fatalf("unexpected runtime: %+v", server.testLab)
	}
	if server.testLab.client == nil || server.testLab.client.Timeout == 0 {
		t.Fatal("runtime client was not configured")
	}

	for _, tc := range []struct{ origin, token string }{
		{origin: "://bad", token: "token"},
		{origin: "ftp://runtime.example.test", token: "token"},
		{origin: "https://user@runtime.example.test", token: "token"},
		{origin: "https://runtime.example.test?debug=1", token: "token"},
		{origin: "https://runtime.example.test#fragment", token: "token"},
		{origin: "https://runtime.example.test", token: ""},
	} {
		candidate := &Server{}
		candidate.ConfigureTestLab(nil, tc.origin, tc.token)
		if candidate.testLab != nil {
			t.Fatalf("invalid runtime %q was accepted", tc.origin)
		}
	}
}

func TestTestLabHandlersRejectDisabledOrUnavailableRuntime(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handlers := []struct {
		name string
		call func(*Server, *gin.Context)
	}{
		{name: "create session", call: (*Server).createTestLabSession},
		{name: "close session", call: (*Server).closeTestLabSession},
		{name: "credentials", call: (*Server).testLabCredentials},
		{name: "account", call: (*Server).testLabAccount},
		{name: "devices", call: (*Server).testLabDevices},
	}
	for _, tc := range handlers {
		t.Run(tc.name+" disabled", func(t *testing.T) {
			t.Setenv("TEST_LAB_ENABLED", "false")
			t.Setenv("ACCOUNT_MANAGER_ENV", "dev")
			ctx, recorder := newTestLabUnitContext(http.MethodPost, "/", `{}`, "user-1")
			tc.call(&Server{}, ctx)
			if recorder.Code != http.StatusNotFound || recorder.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status=%d cache-control=%q", recorder.Code, recorder.Header().Get("Cache-Control"))
			}
		})
		t.Run(tc.name+" unavailable", func(t *testing.T) {
			t.Setenv("TEST_LAB_ENABLED", "true")
			t.Setenv("ACCOUNT_MANAGER_ENV", "dev")
			ctx, recorder := newTestLabUnitContext(http.MethodPost, "/", `{}`, "user-1")
			tc.call(&Server{}, ctx)
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("status=%d, want %d", recorder.Code, http.StatusServiceUnavailable)
			}
		})
	}
}

func TestTestLabRequestValidationStopsBeforeStorage(t *testing.T) {
	t.Setenv("TEST_LAB_ENABLED", "true")
	t.Setenv("ACCOUNT_MANAGER_ENV", "dev")
	server := &Server{}
	server.ConfigureTestLab(nil, "https://runtime.example.test", "fixture-token")

	t.Run("session scope", func(t *testing.T) {
		ctx, recorder := newTestLabUnitContext(http.MethodPost, "/", `{}`, "user-1")
		server.createTestLabSession(ctx)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status=%d", recorder.Code)
		}
	})
	t.Run("session one object", func(t *testing.T) {
		body := `{"product_id":"` + testLabUUID + `","device_id":"` + testLabUUID + `","account_id":"` + testLabUUID + `"} {}`
		ctx, recorder := newTestLabUnitContext(http.MethodPost, "/", body, "user-1")
		server.createTestLabSession(ctx)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status=%d", recorder.Code)
		}
	})
	t.Run("account fields", func(t *testing.T) {
		ctx, recorder := newTestLabUnitContext(http.MethodPost, "/", `{"email":"not-accepted@example.test"}`, "user-1")
		server.testLabAccount(ctx)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status=%d", recorder.Code)
		}
	})
	t.Run("device query scope", func(t *testing.T) {
		ctx, recorder := newTestLabUnitContext(http.MethodGet, "/", "", "user-1")
		server.testLabDevices(ctx)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status=%d", recorder.Code)
		}
	})
	t.Run("device body", func(t *testing.T) {
		ctx, recorder := newTestLabUnitContext(http.MethodPost, "/", `{`, "user-1")
		server.testLabDevices(ctx)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status=%d", recorder.Code)
		}
	})
	t.Run("unknown action", func(t *testing.T) {
		ctx, recorder := validTestLabDeviceContext(t, "unknown", "")
		server.testLabDevices(ctx)
		if got := ctx.Writer.Status(); got != http.StatusNotFound {
			t.Fatalf("status=%d recorder=%d", got, recorder.Code)
		}
	})
	t.Run("binding claim", func(t *testing.T) {
		ctx, recorder := validTestLabDeviceContext(t, "bind", "")
		server.testLabDevices(ctx)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status=%d", recorder.Code)
		}
	})
	t.Run("provision public key", func(t *testing.T) {
		ctx, recorder := validTestLabDeviceContext(t, "provision", `,"clip_public_key":"not pem"`)
		server.testLabDevices(ctx)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status=%d", recorder.Code)
		}
	})
	t.Run("provision metadata", func(t *testing.T) {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		publicKey := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
		extra := `,"clip_public_key":` + quoteJSON(publicKey)
		ctx, recorder := validTestLabDeviceContext(t, "provision", extra)
		server.testLabDevices(ctx)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status=%d", recorder.Code)
		}
	})
}

func newTestLabUnitContext(method, target, body, userID string) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(method, target, strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Set("userID", userID)
	return ctx, recorder
}

func validTestLabDeviceContext(t *testing.T, action, extra string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	body := `{"product_id":"` + testLabUUID + `","account_id":"` + testLabUUID + `"` + extra + `}`
	ctx, recorder := newTestLabUnitContext(http.MethodPost, "/", body, "user-1")
	ctx.Params = gin.Params{
		{Key: "brandCloudId", Value: testLabUUID},
		{Key: "deviceId", Value: testLabUUID},
		{Key: "action", Value: action},
	}
	return ctx, recorder
}

func quoteJSON(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, "\n", `\n`)
	return `"` + value + `"`
}
