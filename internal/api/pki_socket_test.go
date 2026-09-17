package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"rtk_account_manager/internal/auth"
)

func TestPKISocketPreservesSignedRequestAndFailsWithoutOwner(t *testing.T) {
	dir, err := os.MkdirTemp("", "pkis-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "controller.sock")
	t.Setenv("PKI_CONTROLLER_URL", "https://controller.internal.example")
	t.Setenv("PKI_CONTROLLER_SOCKET", socket)
	t.Setenv("PKI_ENVIRONMENT", "dev")
	for _, name := range []string{"PKI_REQUIRE_USER_MFA", "PKI_CONTROLLER_CLIENT_CERT", "PKI_CONTROLLER_CLIENT_KEY", "PKI_CONTROLLER_CA"} {
		t.Setenv(name, "")
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer := auth.RS256TokenSigner{Signer: key, PublicKey: &key.PublicKey}
	s := New(nil, auth.NewServiceWithSigners(signer, signer, time.Minute, time.Hour))
	if err := s.ConfigurePKIFromEnv(); err != nil {
		t.Fatal(err)
	}
	defer s.pkiClient.client.CloseIdleConnections()
	input := auth.PKIAssertionInput{UserID: "operator", Roles: []string{"pki_admin"}, Environment: "dev", Method: "POST", Path: "/v1/pki/issuers", Body: []byte("{\n  \"environment\": \"dev\"\n}"), IdempotencyKey: "unchanged"}
	if _, _, err := s.pkiCall(context.Background(), input, time.Now()); err == nil {
		t.Fatal("missing owner did not fail")
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(socket, 0600); err != nil {
		t.Fatal(err)
	}
	received := make(chan string, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != string(input.Body) || r.URL.Path != input.Path || r.Method != input.Method || r.Host != "controller.internal.example" || r.Header.Get("Idempotency-Key") != input.IdempotencyKey {
			t.Error("request binding changed")
		}
		received <- strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		w.WriteHeader(201)
		io.WriteString(w, `{"id":"result"}`)
	})}
	go server.Serve(listener)
	defer server.Close()
	status, body, err := s.pkiCall(context.Background(), input, time.Now())
	if err != nil || status != 201 || string(body) != `{"id":"result"}` {
		t.Fatal("socket call", status, err)
	}
	token, err := jwt.Parse(<-received, func(*jwt.Token) (any, error) { return &key.PublicKey, nil }, jwt.WithValidMethods([]string{"RS256"}))
	if err != nil || !token.Valid {
		t.Fatal("assertion signature", err)
	}
	claims := token.Claims.(jwt.MapClaims)
	hash := sha256.Sum256(input.Body)
	if claims["mfa"] != false || claims["path"] != input.Path || claims["body_sha256"] != hex.EncodeToString(hash[:]) {
		t.Fatal("assertion binding", claims)
	}
	req, _ := http.NewRequest("GET", "https://other.internal.example/v1/pki/issuers", nil)
	if response, err := s.pkiClient.client.Do(req); err == nil {
		response.Body.Close()
		t.Fatal("changed origin accepted")
	}
	server.Close()
	if _, _, err := s.pkiCall(context.Background(), input, time.Now()); err == nil {
		t.Fatal("owner shutdown fell back to network")
	}
	t.Setenv("PKI_CONTROLLER_CLIENT_KEY", "static.key")
	if err := s.ConfigurePKIFromEnv(); err == nil {
		t.Fatal("mixed static/socket settings accepted")
	}
}

func TestPKISocketRejectsUnsafePaths(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "not-socket")
	if err := os.WriteFile(file, []byte("not a socket"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := privatePKISocket(file, true); err == nil {
		t.Fatal("regular file accepted")
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := privatePKISocket(file, false); err == nil {
		t.Fatal("shared directory accepted")
	}
	if err := privatePKISocket("relative.sock", false); err == nil {
		t.Fatal("relative socket accepted")
	}
}
