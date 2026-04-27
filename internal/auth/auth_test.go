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
