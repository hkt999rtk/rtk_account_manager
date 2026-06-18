package store

import (
	"testing"
	"time"
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

func TestNewStoreHasDefaultAuthTokenRateLimit(t *testing.T) {
	s := New(nil)
	if s.authTokenRateLimitMax != 5 {
		t.Fatalf("expected default max 5, got %d", s.authTokenRateLimitMax)
	}
	if s.authTokenRateLimitWindow != time.Hour {
		t.Fatalf("expected default window 1h, got %v", s.authTokenRateLimitWindow)
	}
}
