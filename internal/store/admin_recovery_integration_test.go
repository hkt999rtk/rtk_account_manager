package store

import (
	"context"
	"sync"
	"testing"
	"time"
)

func recoveryFixture(t *testing.T, env storeIntegrationEnv, email, role string) string {
	t.Helper()
	var id string
	if err := env.db.QueryRow(context.Background(), `INSERT INTO users(email,password_hash,email_verified) VALUES($1,'fixture-hash',true) RETURNING id::text`, email).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if role != "" {
		if _, err := env.store.CreateRoleAssignment(context.Background(), RoleAssignmentCreateInput{RoleName: role, ActorType: "user", ActorID: id, ScopeType: "platform"}); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func TestAdminRecoveryRequiresIndependentLiveApprovals(t *testing.T) {
	testAdminRecoveryApprovals(t, false)
}

func TestAdminRecoveryWithoutMFAStillRequiresIndependentLiveApprovals(t *testing.T) {
	testAdminRecoveryApprovals(t, true)
}

func testAdminRecoveryApprovals(t *testing.T, optionalMFA bool) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := env.store.BootstrapPlatformAdmin(ctx, "initial@example.test", "fixture", nil); err != nil {
		t.Fatal(err)
	}
	creator := recoveryFixture(t, env, "creator@example.test", "pki_admin")
	admin := recoveryFixture(t, env, "approver@example.test", "pki_admin")
	custodian := recoveryFixture(t, env, "custodian@example.test", "security_custodian")
	target := recoveryFixture(t, env, "replacement@example.test", "")
	principal := func(id string) RecoveryPrincipal {
		return RecoveryPrincipal{UserID: id, MFA: !optionalMFA, AuthTime: now, AuthenticatedWithoutMFA: optionalMFA}
	}
	command := RecoveryCommand{Action: "create", Key: "recovery-1", Target: target, Reason: "Recover administration after a fixture incident"}
	r, err := env.store.AdminRecovery(ctx, principal(creator), command, now)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := env.store.AdminRecovery(ctx, principal(creator), command, now)
	if err != nil || replay.ID != r.ID {
		t.Fatal("idempotent request failed")
	}
	command.Reason = "different reason"
	if _, err = env.store.AdminRecovery(ctx, principal(creator), command, now); err != ErrConflict {
		t.Fatal("changed request reused idempotency key")
	}
	approve := RecoveryCommand{Action: "approve", ID: r.ID, Role: "pki_admin", Digest: r.Digest}
	if _, err = env.store.AdminRecovery(ctx, principal(creator), approve, now); err == nil {
		t.Fatal("requester self-approval accepted")
	}
	if _, err = env.store.AdminRecovery(ctx, principal(target), approve, now); err == nil {
		t.Fatal("target approved own elevation")
	}
	execute := RecoveryCommand{Action: "execute", ID: r.ID}
	if _, err = env.store.AdminRecovery(ctx, principal(admin), execute, now); err == nil {
		t.Fatal("executed without approvals")
	}
	approve.Digest = "wrong"
	if _, err = env.store.AdminRecovery(ctx, principal(admin), approve, now); err == nil {
		t.Fatal("wrong digest accepted")
	}
	approve.Digest = r.Digest
	if _, err = env.store.AdminRecovery(ctx, principal(admin), approve, now); err != nil {
		t.Fatal(err)
	}
	if _, err = env.store.AdminRecovery(ctx, principal(admin), execute, now); err == nil {
		t.Fatal("single approver accepted")
	}
	approve.Role = "security_custodian"
	if _, err = env.store.AdminRecovery(ctx, principal(custodian), approve, now); err != nil {
		t.Fatal(err)
	}
	if _, err = env.db.Exec(ctx, `UPDATE role_assignments SET disabled_at=now() WHERE actor_id=$1`, custodian); err != nil {
		t.Fatal(err)
	}
	if _, err = env.store.AdminRecovery(ctx, principal(admin), execute, now); err == nil {
		t.Fatal("revoked custodian role retained authority")
	}
	if _, err = env.db.Exec(ctx, `UPDATE role_assignments SET disabled_at=NULL WHERE actor_id=$1`, custodian); err != nil {
		t.Fatal(err)
	}
	if !optionalMFA {
		stale := principal(admin)
		stale.AuthTime = now.Add(-6 * time.Minute)
		if _, err = env.store.AdminRecovery(ctx, stale, execute, now); err == nil {
			t.Fatal("stale MFA accepted")
		}
	}
	outcomes := make(chan error, 8)
	var concurrent sync.WaitGroup
	for n := 0; n < 8; n++ {
		concurrent.Add(1)
		go func() {
			defer concurrent.Done()
			result, e := env.store.AdminRecovery(ctx, principal(admin), execute, now)
			if e == nil && result.Status != "completed" {
				e = ErrConflict
			}
			outcomes <- e
		}()
	}
	concurrent.Wait()
	close(outcomes)
	for e := range outcomes {
		if e != nil {
			t.Fatal(e)
		}
	}
	final, err := env.store.AdminRecovery(ctx, principal(admin), RecoveryCommand{Action: "get", ID: r.ID}, now)
	if err != nil || final.Status != "completed" {
		t.Fatalf("execution: %+v %v", final, err)
	}
	if _, err = env.store.AdminRecovery(ctx, principal(admin), execute, now); err != nil {
		t.Fatal("execution replay failed")
	}
	isAdmin, err := env.store.IsPlatformAdmin(ctx, target)
	if err != nil || !isAdmin {
		t.Fatal("target not restored to platform administration")
	}
	roles, err := env.store.PKIRoles(ctx, target)
	if err != nil || len(roles) != 1 || roles[0] != "platform_admin" {
		t.Fatalf("unexpected recovery grants: %v %v", roles, err)
	}
	if _, err = env.db.Exec(ctx, `UPDATE users SET disabled_at=now() WHERE id=$1`, target); err != nil {
		t.Fatal(err)
	}
	if _, err = env.store.AdminRecovery(ctx, principal(admin), execute, now); err != nil {
		t.Fatal(err)
	}
	var stillDisabled bool
	if err = env.db.QueryRow(ctx, `SELECT disabled_at IS NOT NULL FROM users WHERE id=$1`, target).Scan(&stillDisabled); err != nil || !stillDisabled {
		t.Fatal("completed recovery resurrected a subsequently disabled account")
	}
	var hash string
	var sealed bool
	var events int
	if err = env.db.QueryRow(ctx, `SELECT password_hash FROM users WHERE id=$1`, target).Scan(&hash); err != nil || hash != "fixture-hash" {
		t.Fatal("recovery changed password")
	}
	if err = env.db.QueryRow(ctx, `SELECT sealed_at IS NOT NULL FROM platform_bootstrap`).Scan(&sealed); err != nil || !sealed {
		t.Fatal("recovery reopened bootstrap")
	}
	if err = env.db.QueryRow(ctx, `SELECT count(*) FROM platform_admin_recovery_audit WHERE request_id=$1 AND event='administrator_granted'`, r.ID).Scan(&events); err != nil || events != 1 {
		t.Fatal("execution was duplicated")
	}
	for _, query := range []string{`DELETE FROM platform_admin_recovery_approvals`, `DELETE FROM platform_admin_recovery_audit`, `UPDATE platform_admin_recovery SET reason='altered'`} {
		if _, err = env.db.Exec(ctx, query); err == nil {
			t.Fatal("recovery evidence is mutable")
		}
	}
}

func TestAdminRecoveryExpiresAndRejectsDisabledTarget(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := env.store.BootstrapPlatformAdmin(ctx, "initial@example.test", "fixture", nil); err != nil {
		t.Fatal(err)
	}
	creator := recoveryFixture(t, env, "creator@example.test", "pki_admin")
	target := recoveryFixture(t, env, "target@example.test", "")
	p := RecoveryPrincipal{UserID: creator, MFA: true, AuthTime: now}
	if _, err := env.db.Exec(ctx, `UPDATE users SET disabled_at=now() WHERE id=$1`, target); err != nil {
		t.Fatal(err)
	}
	cmd := RecoveryCommand{Action: "create", Key: "expired", Target: target, Reason: "fixture recovery"}
	if _, err := env.store.AdminRecovery(ctx, p, cmd, now); err == nil {
		t.Fatal("disabled target accepted")
	}
	if _, err := env.db.Exec(ctx, `UPDATE users SET disabled_at=NULL WHERE id=$1`, target); err != nil {
		t.Fatal(err)
	}
	r, err := env.store.AdminRecovery(ctx, p, cmd, now)
	if err != nil {
		t.Fatal(err)
	}
	p.AuthTime = now.Add(2 * time.Hour)
	if _, err = env.store.AdminRecovery(ctx, p, RecoveryCommand{Action: "execute", ID: r.ID}, p.AuthTime); err != ErrConflict {
		t.Fatalf("expired recovery accepted: %v", err)
	}
}
