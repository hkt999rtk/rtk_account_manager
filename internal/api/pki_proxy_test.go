package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"rtk_account_manager/internal/auth"
)

type pkiProxyTestStore struct {
	Store
	roles []string
}

func (s pkiProxyTestStore) PKIRoles(context.Context, string) ([]string, error) {
	return s.roles, nil
}

func TestPKIProxyForwardsAuthorizedHumanRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer := auth.RS256TokenSigner{Signer: key, PublicKey: &key.PublicKey}
	authService := auth.NewServiceWithSigners(signer, signer, time.Minute, time.Hour)
	access, _, err := authService.IssueAccessToken("operator")
	if err != nil {
		t.Fatal(err)
	}
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/pki/operations" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Fatalf("unexpected controller request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"items":[]}`)
	}))
	defer controller.Close()
	base, err := url.Parse(controller.URL)
	if err != nil {
		t.Fatal(err)
	}
	server := New(pkiProxyTestStore{roles: []string{"pki_viewer"}}, authService)
	server.pkiClient = &pkiProxy{base: base, client: controller.Client(), environment: "dev"}
	router := gin.New()
	router.Any("/v1/pki/*path", server.proxyPKI)
	req := httptest.NewRequest(http.MethodGet, "/v1/pki/operations", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK || recorder.Body.String() != `{"items":[]}` || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("proxy response: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	for _, test := range []struct {
		method string
		path   string
		body   string
		status int
	}{
		{http.MethodGet, "/v1/pki/other", "", http.StatusNotFound},
		{http.MethodPost, "/v1/pki/issuers", "{", http.StatusBadRequest},
	} {
		req = httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
		req.Header.Set("Authorization", "Bearer "+access)
		recorder = httptest.NewRecorder()
		router.ServeHTTP(recorder, req)
		if recorder.Code != test.status {
			t.Fatalf("%s %s: status=%d body=%s", test.method, test.path, recorder.Code, recorder.Body.String())
		}
	}
}

func TestPKIProxyRejectsUserWithoutPKIRole(t *testing.T) {
	authService := auth.NewService("access", "refresh", time.Minute, time.Hour)
	access, _, err := authService.IssueAccessToken("operator")
	if err != nil {
		t.Fatal(err)
	}
	server := New(pkiProxyTestStore{}, authService)
	base, _ := url.Parse("https://controller.invalid")
	server.pkiClient = &pkiProxy{base: base, client: http.DefaultClient, environment: "dev"}
	router := gin.New()
	router.GET("/v1/pki/*path", server.proxyPKI)
	req := httptest.NewRequest(http.MethodGet, "/v1/pki/operations", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestPKIProxyRejectsDifferentEnvironmentOperatorBeforeForwarding(t *testing.T) {
	authService := auth.NewService("access", "refresh", time.Minute, time.Hour)
	access, _, err := authService.IssueAccessToken("other-admin")
	if err != nil {
		t.Fatal(err)
	}
	server := New(pkiProxyTestStore{roles: []string{"platform_admin"}}, authService)
	base, _ := url.Parse("https://controller.invalid")
	server.pkiClient = &pkiProxy{base: base, client: http.DefaultClient, environment: "dev", operatorID: "configured-operator"}
	router := gin.New()
	router.POST("/v1/pki/*path", server.proxyPKI)
	req := httptest.NewRequest(http.MethodPost, "/v1/pki/operations/11111111-1111-4111-8111-111111111111/authorize", strings.NewReader(`{"request_sha256":"abc"}`))
	req.Header.Set("Authorization", "Bearer "+access)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestPKIProxyFinalizesOnlyExactAuthenticatedOperatorSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer := auth.RS256TokenSigner{Signer: key, PublicKey: &key.PublicKey}
	authService := auth.NewServiceWithSigners(signer, signer, time.Minute, time.Hour)
	path := "/v1/pki/deployment-bootstrap/11111111-1111-4111-8111-111111111111/finalize"
	for _, tc := range []struct {
		name, role, user, method, path, body string
		want                                 int
	}{
		{"operator", "platform_admin", "operator", http.MethodPost, path, "", http.StatusOK},
		{"pki-admin", "pki_admin", "operator", http.MethodPost, path, "", http.StatusOK},
		{"viewer", "pki_viewer", "operator", http.MethodPost, path, "", http.StatusForbidden},
		{"foreign-operator", "platform_admin", "other", http.MethodPost, path, "", http.StatusForbidden},
		{"caller-proof", "platform_admin", "operator", http.MethodPost, path, `{"cleanup_sha256":"caller-proof"}`, http.StatusForbidden},
		{"read-session", "platform_admin", "operator", http.MethodGet, path, "", http.StatusNotFound},
		{"invalid-session", "platform_admin", "operator", http.MethodPost, "/v1/pki/deployment-bootstrap/not-a-session/finalize", "", http.StatusNotFound},
		{"nil-session", "platform_admin", "operator", http.MethodPost, "/v1/pki/deployment-bootstrap/00000000-0000-0000-0000-000000000000/finalize", "", http.StatusNotFound},
		{"other-bootstrap-route", "platform_admin", "operator", http.MethodPost, strings.TrimSuffix(path, "finalize") + "issue", "", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
				if len(parts) != 3 {
					t.Fatal("finalization did not use an authentic signed human assertion")
				}
				raw, err := base64.RawURLEncoding.DecodeString(parts[1])
				var assertion map[string]any
				if err != nil || json.Unmarshal(raw, &assertion) != nil || assertion["active_context"] != true || assertion["sub"] != "operator" || assertion["environment"] != "dev" || assertion["method"] != http.MethodPost || assertion["path"] != path || assertion["mfa"] != false {
					t.Fatal("finalization assertion changed operator/environment, scope or manufactured assurance", assertion, err)
				}
				if r.URL.Path != path || r.Method != http.MethodPost {
					t.Fatal("finalization selected an unowned controller route")
				}
				_, _ = io.WriteString(w, `{"session_id":"11111111-1111-4111-8111-111111111111"}`)
			}))
			defer controller.Close()
			base, _ := url.Parse(controller.URL)
			server := New(pkiProxyTestStore{roles: []string{tc.role}}, authService)
			server.pkiClient = &pkiProxy{base: base, client: controller.Client(), environment: "dev", operatorID: "operator"}
			router := gin.New()
			router.Any("/v1/pki/*path", server.proxyPKI)
			access, _, err := authService.IssueAccessToken(tc.user)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			request.Header.Set("Authorization", "Bearer "+access)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != tc.want || calls != map[bool]int{true: 1, false: 0}[tc.want == http.StatusOK] {
				t.Fatal("invalid finalization admission or unauthorized controller access", response.Code, response.Body.String(), calls)
			}
		})
	}
}
