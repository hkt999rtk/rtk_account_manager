package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
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
