package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestPasswordHashAndCheck(t *testing.T) {
	hash, err := HashPassword("secret")
	if err != nil {
		t.Fatal(err)
	}
	if !CheckPassword(hash, "secret") {
		t.Fatal("expected password to match")
	}
	if CheckPassword(hash, "wrong") {
		t.Fatal("expected wrong password to fail")
	}
}

func TestTokenKindValidation(t *testing.T) {
	svc := NewService("access-secret", "refresh-secret", time.Minute, time.Hour)
	access, _, err := svc.IssueAccessToken("user-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ParseAccessToken(access); err != nil {
		t.Fatalf("expected access token to parse: %v", err)
	}
	if _, err := svc.ParseRefreshToken(access); err == nil {
		t.Fatal("expected access token to fail refresh parsing")
	}
}

func TestTokenKindValidationWithRSASigners(t *testing.T) {
	accessKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	refreshKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewServiceWithSigners(
		RS256TokenSigner{Signer: accessKey, PublicKey: &accessKey.PublicKey},
		RS256TokenSigner{Signer: refreshKey, PublicKey: &refreshKey.PublicKey},
		time.Minute,
		time.Hour,
	)
	access, _, err := svc.IssueAccessToken("user-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ParseAccessToken(access); err != nil {
		t.Fatalf("expected access token to parse: %v", err)
	}
	if _, err := svc.ParseRefreshToken(access); err == nil {
		t.Fatal("expected access token to fail refresh parsing")
	}

	wrongKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	wrongSvc := NewServiceWithSigners(
		RS256TokenSigner{Signer: wrongKey, PublicKey: &wrongKey.PublicKey},
		RS256TokenSigner{Signer: refreshKey, PublicKey: &refreshKey.PublicKey},
		time.Minute,
		time.Hour,
	)
	if _, err := wrongSvc.ParseAccessToken(access); err == nil {
		t.Fatal("expected token signed with another key to fail parsing")
	}
}

func TestLoadPEMTokenSigner(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	privatePath := filepath.Join(dir, "token.key")
	publicPath := filepath.Join(dir, "token.pub")
	if err := os.WriteFile(privatePath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		t.Fatal(err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(publicPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}), 0o600); err != nil {
		t.Fatal(err)
	}

	signer, err := LoadPEMTokenSigner(privatePath, publicPath)
	if err != nil {
		t.Fatalf("LoadPEMTokenSigner() error = %v", err)
	}
	svc := NewServiceWithSigners(signer, signer, time.Minute, time.Hour)
	token, _, err := svc.IssueAccessToken("user-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ParseAccessToken(token); err != nil {
		t.Fatalf("expected token to parse: %v", err)
	}
}

func TestLoadPEMTokenSignerSupportsPKCS8(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	privatePath := filepath.Join(dir, "token.key")
	publicPath := filepath.Join(dir, "token.pub")
	privateDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(privatePath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(publicPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadPEMTokenSigner(privatePath, publicPath); err != nil {
		t.Fatalf("LoadPEMTokenSigner() error = %v", err)
	}
}

func TestLoadPEMTokenSignerRejectsInvalidMaterial(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	privatePath := filepath.Join(dir, "token.key")
	publicPath := filepath.Join(dir, "token.pub")
	if err := os.WriteFile(privatePath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		t.Fatal(err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&otherKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(publicPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadPEMTokenSigner(privatePath, publicPath); err == nil {
		t.Fatal("expected mismatched public key to fail")
	}

	if err := os.WriteFile(privatePath, []byte("not pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPEMTokenSigner(privatePath, publicPath); err == nil {
		t.Fatal("expected invalid private key PEM to fail")
	}

	if err := os.WriteFile(privatePath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(publicPath, []byte("not pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPEMTokenSigner(privatePath, publicPath); err == nil {
		t.Fatal("expected invalid public key PEM to fail")
	}
}

func TestRS256TokenSignerErrorPaths(t *testing.T) {
	if _, err := (RS256TokenSigner{}).SignToken("payload"); err == nil {
		t.Fatal("expected missing signer to fail")
	}
	if _, err := (RS256TokenSigner{}).Keyfunc(jwt.New(jwt.SigningMethodRS256)); err == nil {
		t.Fatal("expected missing public key to fail")
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer := RS256TokenSigner{Signer: key, PublicKey: &key.PublicKey}
	if _, err := signer.Keyfunc(jwt.New(jwt.SigningMethodHS256)); err == nil {
		t.Fatal("expected wrong signing method to fail")
	}
	svc := NewServiceWithSigners(RS256TokenSigner{}, signer, time.Minute, time.Hour)
	if _, _, err := svc.IssueAccessToken("user-1"); err == nil {
		t.Fatal("expected issuing with missing signer to fail")
	}
}

func TestExpiredAndWrongSecretTokensFailParsing(t *testing.T) {
	expiredSvc := NewService("access-secret", "refresh-secret", -time.Minute, time.Hour)
	expired, _, err := expiredSvc.IssueAccessToken("user-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := expiredSvc.ParseAccessToken(expired); err == nil {
		t.Fatal("expected expired access token to fail parsing")
	}

	validSvc := NewService("access-secret", "refresh-secret", time.Minute, time.Hour)
	token, _, err := validSvc.IssueAccessToken("user-1")
	if err != nil {
		t.Fatal(err)
	}
	wrongSecretSvc := NewService("other-access-secret", "refresh-secret", time.Minute, time.Hour)
	if _, err := wrongSecretSvc.ParseAccessToken(token); err == nil {
		t.Fatal("expected token signed with another secret to fail parsing")
	}
}

func TestRandomTokenProducesHashableDistinctValues(t *testing.T) {
	first, err := RandomToken()
	if err != nil {
		t.Fatal(err)
	}
	second, err := RandomToken()
	if err != nil {
		t.Fatal(err)
	}
	if first == "" || second == "" {
		t.Fatal("expected non-empty random tokens")
	}
	if first == second {
		t.Fatal("expected distinct random tokens")
	}
	if got := HashToken(first); len(got) != 64 {
		t.Fatalf("expected sha256 hex token hash length 64, got %d", len(got))
	}
}
