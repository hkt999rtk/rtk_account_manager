package auth

import (
	"testing"
	"time"
)

func TestBrandCloudTokenClaimsCarryScopedSubject(t *testing.T) {
	service := NewService("access-secret", "refresh-secret", time.Minute, time.Hour)

	accessToken, accessExpiresAt, err := service.IssueBrandCloudAccessToken("user-1", "brand-user-1", "brand-cloud-1", "acme")
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
	if accessClaims.SubjectType != SubjectTypeBrandCloudUser ||
		accessClaims.BrandCloudUserID != "brand-user-1" ||
		accessClaims.BrandCloudID != "brand-cloud-1" ||
		accessClaims.TenantSlug != "acme" ||
		accessClaims.UserID != "user-1" ||
		accessClaims.Subject != "brand_cloud_user:brand-user-1" {
		t.Fatalf("unexpected access claims: %+v", accessClaims)
	}

	refreshToken, refreshExpiresAt, err := service.IssueBrandCloudRefreshToken("user-1", "brand-user-1", "brand-cloud-1", "acme")
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
	if refreshClaims.SubjectType != SubjectTypeBrandCloudUser ||
		refreshClaims.BrandCloudUserID != "brand-user-1" ||
		refreshClaims.BrandCloudID != "brand-cloud-1" ||
		refreshClaims.TenantSlug != "acme" ||
		refreshClaims.UserID != "user-1" ||
		refreshClaims.Subject != "brand_cloud_user:brand-user-1" {
		t.Fatalf("unexpected refresh claims: %+v", refreshClaims)
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
	if claims.SubjectType != SubjectTypePlatformUser ||
		claims.UserID != "platform-user-1" ||
		claims.BrandCloudUserID != "" ||
		claims.BrandCloudID != "" ||
		claims.TenantSlug != "" ||
		claims.Subject != "platform-user-1" {
		t.Fatalf("unexpected platform claims: %+v", claims)
	}
}
