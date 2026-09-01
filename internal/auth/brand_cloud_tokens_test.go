package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestRetiredTenantTokensRejectedAtSharedParser(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer := RS256TokenSigner{Signer: key}
	for _, mode := range []struct {
		name                  string
		service               *Service
		method                jwt.SigningMethod
		accessKey, refreshKey any
	}{
		{"HMAC", NewService("access-secret", "refresh-secret", time.Minute, time.Hour), jwt.SigningMethodHS256, []byte("access-secret"), []byte("refresh-secret")},
		{"RSA", NewServiceWithSigners(signer, signer, time.Minute, time.Hour), jwt.SigningMethodRS256, key, key},
	} {
		for _, kind := range []TokenKind{TokenKindAccess, TokenKindRefresh} {
			for _, variant := range []struct {
				name    string
				mutate  func(*Claims)
				allowed bool
			}{
				{"global user", func(c *Claims) {}, true},
				{"legacy global user", func(c *Claims) { c.SubjectType = "" }, true},
				{"end user", func(c *Claims) {
					c.SubjectType = SubjectTypeEndUser
					c.UserID = ""
					c.EndUserID = "consumer"
					c.Subject = "end_user:consumer"
				}, true},
				{"tenant subject", func(c *Claims) { c.SubjectType = SubjectTypeBrandCloudUser }, false},
				{"tenant user claim", func(c *Claims) { c.BrandCloudUserID = "tenant-user" }, false},
				{"tenant cloud claim", func(c *Claims) { c.BrandCloudID = "cloud" }, false},
				{"tenant slug claim", func(c *Claims) { c.TenantSlug = "acme" }, false},
				{"tenant sub without type", func(c *Claims) { c.SubjectType = ""; c.Subject = "brand_cloud_user:tenant-user" }, false},
				{"mixed end user", func(c *Claims) {
					c.SubjectType = SubjectTypeEndUser
					c.EndUserID = "consumer"
					c.BrandCloudUserID = "tenant-user"
				}, false},
				{"unknown subject", func(c *Claims) { c.SubjectType = "device" }, false},
			} {
				t.Run(mode.name+"/"+string(kind)+"/"+variant.name, func(t *testing.T) {
					claims := Claims{UserID: "user-1", SubjectType: SubjectTypeUser, Kind: kind, RegisteredClaims: jwt.RegisteredClaims{Subject: "user-1", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}}
					variant.mutate(&claims)
					secret, parse := mode.accessKey, mode.service.ParseAccessToken
					if kind == TokenKindRefresh {
						secret, parse = mode.refreshKey, mode.service.ParseRefreshToken
					}
					token, err := jwt.NewWithClaims(mode.method, claims).SignedString(secret)
					if err != nil {
						t.Fatal(err)
					}
					got, err := parse(token)
					if variant.allowed {
						if err != nil || got == nil {
							t.Fatalf("valid independent identity rejected: %v", err)
						}
					} else if err == nil || got != nil {
						t.Fatal("retired or unsupported credential accepted")
					}
				})
			}
		}
	}
}

func TestPlatformTokenClaimsDefaultToPlatformSubject(t *testing.T) {
	service := NewService("access-secret", "refresh-secret", time.Minute, time.Hour)

	token, _, err := service.IssueAccessToken("platform-user-1")
	if err != nil {
		t.Fatal(err)
	}
	claims, err := service.ParseAccessToken(token)
	if err != nil {
		t.Fatal(err)
	}
	if claims.SubjectType != SubjectTypeUser ||
		claims.UserID != "platform-user-1" ||
		claims.BrandCloudUserID != "" ||
		claims.BrandCloudID != "" ||
		claims.TenantSlug != "" ||
		claims.Subject != "platform-user-1" {
		t.Fatalf("unexpected platform claims: %+v", claims)
	}
}

func TestEndUserTokenClaimsCarryGlobalSubject(t *testing.T) {
	service := NewService("access-secret", "refresh-secret", time.Minute, time.Hour)

	accessToken, accessExpiresAt, err := service.IssueEndUserAccessToken("end-user-1")
	if err != nil {
		t.Fatal(err)
	}
	if accessExpiresAt.IsZero() {
		t.Fatal("expected access expiry")
	}
	accessClaims, err := service.ParseAccessToken(accessToken)
	if err != nil {
		t.Fatal(err)
	}
	if accessClaims.SubjectType != SubjectTypeEndUser ||
		accessClaims.EndUserID != "end-user-1" ||
		accessClaims.UserID != "" ||
		accessClaims.BrandCloudUserID != "" ||
		accessClaims.BrandCloudID != "" ||
		accessClaims.Subject != "end_user:end-user-1" {
		t.Fatalf("unexpected access claims: %+v", accessClaims)
	}

	refreshToken, refreshExpiresAt, err := service.IssueEndUserRefreshToken("end-user-1")
	if err != nil {
		t.Fatal(err)
	}
	if refreshExpiresAt.IsZero() {
		t.Fatal("expected refresh expiry")
	}
	refreshClaims, err := service.ParseRefreshToken(refreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if refreshClaims.SubjectType != SubjectTypeEndUser ||
		refreshClaims.EndUserID != "end-user-1" ||
		refreshClaims.Subject != "end_user:end-user-1" {
		t.Fatalf("unexpected refresh claims: %+v", refreshClaims)
	}
}
