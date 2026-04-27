package auth

import (
	"testing"
	"time"
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
