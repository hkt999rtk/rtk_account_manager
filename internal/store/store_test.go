package store

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestConfigureAuthTokenRateLimit(t *testing.T) {
	s := &Store{}

	s.ConfigureAuthTokenRateLimit(10, 2*time.Hour)
	if s.authTokenRateLimitMax != 10 {
		t.Fatalf("expected max 10, got %d", s.authTokenRateLimitMax)
	}
	if s.authTokenRateLimitWindow != 2*time.Hour {
		t.Fatalf("expected window 2h, got %v", s.authTokenRateLimitWindow)
	}

	s.ConfigureAuthTokenRateLimit(0, 0)
	if s.authTokenRateLimitMax != 10 {
		t.Fatalf("expected max to remain 10 on invalid input, got %d", s.authTokenRateLimitMax)
	}
	if s.authTokenRateLimitWindow != 2*time.Hour {
		t.Fatalf("expected window to remain 2h on invalid input, got %v", s.authTokenRateLimitWindow)
	}
}

func TestACLValueHelpers(t *testing.T) {
	scopeID, orgID := "scope-1", "org-1"
	if gotScope, gotOrg := normalizeScope(ScopeTypePlatform, &scopeID, &orgID); gotScope != nil || gotOrg != nil {
		t.Fatalf("platform scope = %v, %v", gotScope, gotOrg)
	}
	if gotScope, gotOrg := normalizeScope(ScopeTypeOrganization, &scopeID, nil); gotScope == nil || gotOrg == nil || *gotOrg != scopeID {
		t.Fatalf("organization scope from scope ID = %v, %v", gotScope, gotOrg)
	}
	if gotScope, gotOrg := normalizeScope(ScopeTypeOrganization, nil, &orgID); gotScope == nil || gotOrg == nil || *gotScope != orgID {
		t.Fatalf("organization scope from org ID = %v, %v", gotScope, gotOrg)
	}
	if gotScope, gotOrg := normalizeScope("custom", &scopeID, &orgID); gotScope == nil || gotOrg == nil {
		t.Fatalf("custom scope = %v, %v", gotScope, gotOrg)
	}
	if got := defaultString("  value  ", "fallback"); got != "value" {
		t.Fatalf("defaultString value = %q", got)
	}
	if got := defaultString("  ", "fallback"); got != "fallback" {
		t.Fatalf("defaultString fallback = %q", got)
	}
	now := time.Now().UTC()
	if got := defaultTime(now); !got.Equal(now) {
		t.Fatalf("defaultTime(non-zero) = %v", got)
	}
	if got := defaultTime(time.Time{}); got.IsZero() {
		t.Fatal("defaultTime(zero) remained zero")
	}
	if !isUniqueViolation(&pgconn.PgError{Code: "23505"}) {
		t.Fatal("expected unique violation")
	}
	if isUniqueViolation(errors.New("not postgres")) {
		t.Fatal("unexpected unique violation")
	}
}

func TestNewStoreHasDefaultAuthTokenRateLimit(t *testing.T) {
	s := New(nil)
	if s.authTokenRateLimitMax != 5 {
		t.Fatalf("expected default max 5, got %d", s.authTokenRateLimitMax)
	}
	if s.authTokenRateLimitWindow != time.Hour {
		t.Fatalf("expected default window 1h, got %v", s.authTokenRateLimitWindow)
	}
}
