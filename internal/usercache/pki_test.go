package usercache

import (
	"context"
	"testing"
	"time"

	"rtk_account_manager/internal/api"
	"rtk_account_manager/internal/store"
)

type pkiRoleSource struct {
	api.Store
	roles []string
	calls int
}

type recoverySource struct {
	api.Store
	result store.AdminRecovery
	calls  int
}

func (s *recoverySource) AdminRecovery(context.Context, store.RecoveryPrincipal, store.RecoveryCommand, time.Time) (store.AdminRecovery, error) {
	s.calls++
	return s.result, nil
}

func (s *pkiRoleSource) PKIRoles(context.Context, string) ([]string, error) {
	s.calls++
	return s.roles, nil
}

func TestPKIRolesAlwaysReadsBackingStore(t *testing.T) {
	source := &pkiRoleSource{roles: []string{"security_custodian"}}
	cached := &Store{Store: source}
	roles, err := cached.PKIRoles(context.Background(), "user")
	if err != nil || len(roles) != 1 {
		t.Fatalf("initial roles %v %v", roles, err)
	}
	source.roles = nil
	roles, err = cached.PKIRoles(context.Background(), "user")
	if err != nil || len(roles) != 0 || source.calls != 2 {
		t.Fatal("revoked role was cached")
	}
	if _, err = (&Store{}).PKIRoles(context.Background(), "user"); err == nil {
		t.Fatal("missing role store accepted")
	}
}

func TestAdminRecoveryAlwaysUsesAuthoritativeStore(t *testing.T) {
	source := &recoverySource{result: store.AdminRecovery{ID: "recovery-1"}}
	got, err := (&Store{Store: source}).AdminRecovery(t.Context(), store.RecoveryPrincipal{}, store.RecoveryCommand{}, time.Time{})
	if err != nil || got.ID != "recovery-1" || source.calls != 1 {
		t.Fatalf("recovery=%+v calls=%d err=%v", got, source.calls, err)
	}
	if _, err = (&Store{}).AdminRecovery(t.Context(), store.RecoveryPrincipal{}, store.RecoveryCommand{}, time.Time{}); err == nil {
		t.Fatal("missing authoritative recovery store accepted")
	}
}
