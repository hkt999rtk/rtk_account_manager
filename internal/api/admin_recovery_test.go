package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/store"
)

type recoveryAPIStore struct {
	Store
	seen    store.RecoveryPrincipal
	command store.RecoveryCommand
	calls   int
}

func (s *recoveryAPIStore) AdminRecovery(_ context.Context, p store.RecoveryPrincipal, cmd store.RecoveryCommand, _ time.Time) (store.AdminRecovery, error) {
	s.calls++
	s.seen = p
	s.command = cmd
	if !p.MFA && !p.AuthenticatedWithoutMFA {
		return store.AdminRecovery{}, store.ErrRecoveryDenied
	}
	return store.AdminRecovery{ID: "request", Status: "requested"}, nil
}

func TestAdminRecoveryOptionalMFAPolicyBindsOrdinaryUser(t *testing.T) {
	tokens := auth.NewService("access", "refresh", time.Hour, time.Hour)
	token, _, err := tokens.IssueAccessToken("ordinary-user")
	if err != nil {
		t.Fatal(err)
	}
	for _, policy := range []string{"", "false", "true", "invalid"} {
		t.Setenv("PKI_REQUIRE_USER_MFA", policy)
		backend := &recoveryAPIStore{}
		server := New(backend, tokens)
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest("GET", "/v1/platform/admin-recovery", nil)
		c.Request.Header.Set("Authorization", "Bearer "+token)
		server.adminRecovery(c)
		want := 200
		if policy == "true" {
			want = 403
		}
		if policy == "invalid" {
			want = 503
		}
		if rec.Code != want {
			t.Fatal(policy, rec.Code, rec.Body.String())
		}
		if backend.seen.MFA {
			t.Fatal("ordinary login relabeled MFA")
		}
	}
}
func TestAdminRecoveryAPIBindsVerifiedIdentity(t *testing.T) {
	backend := &recoveryAPIStore{}
	tokens := auth.NewService("fixture-access", "fixture-refresh", time.Hour, time.Hour)
	server := New(backend, tokens)
	token, _, err := tokens.IssueAssuredAccessToken("verified-user", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		body string
		want int
	}{{`{"target_user_id":"target","reason":"incident"}`, 200}, {`{"target_user_id":"target","reason":"incident","UserID":"forged"}`, 400}, {`{"target_user_id":"target","reason":"incident"} {}`, 400}} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest("POST", "/v1/platform/admin-recovery", strings.NewReader(tc.body))
		c.Request.Header.Set("Authorization", "Bearer "+token)
		c.Request.Header.Set("Idempotency-Key", "fixture-key")
		server.adminRecovery(c)
		if rec.Code != tc.want {
			t.Fatalf("response %d want %d: %s", rec.Code, tc.want, rec.Body.String())
		}
	}
	if backend.calls != 1 || backend.seen.UserID != "verified-user" || !backend.seen.MFA || backend.command.Action != "create" || backend.command.Key != "fixture-key" {
		t.Fatal("recovery identity or command not bound")
	}
}
