package usercache

import (
	"context"
	"rtk_account_manager/internal/api"
	"testing"
)

type pkiRoleSource struct {
	api.Store
	roles []string
	calls int
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
