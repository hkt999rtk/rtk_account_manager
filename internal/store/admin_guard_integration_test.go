package store

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

func seedGuardAdmin(t *testing.T, env storeIntegrationEnv, email string) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := env.db.QueryRow(ctx, `INSERT INTO users(email,password_hash,platform_admin,email_verified) VALUES($1,'fixture-hash',true,true) RETURNING id::text`, email).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `INSERT INTO role_assignments(role_id,actor_type,actor_id,scope_type) SELECT id,'user',$1,'platform' FROM roles WHERE name='platform_admin'`, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSealedLastAdminCannotBeDisabledOrStripped(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	if _, err := env.store.BootstrapPlatformAdmin(ctx, "first@example.test", "fixture", nil); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`UPDATE users SET disabled_at=now() WHERE email='first@example.test'`,
		`UPDATE users SET platform_admin=false WHERE email='first@example.test'`,
		`UPDATE users SET signup_pending_verification=true WHERE email='first@example.test'`,
		`UPDATE role_assignments SET disabled_at=now() WHERE scope_type='platform'`,
		`DELETE FROM role_assignments WHERE scope_type='platform'`,
		`UPDATE roles SET disabled_at=now() WHERE name='platform_admin'`,
		`UPDATE roles SET name='former_admin' WHERE name='platform_admin'`,
	} {
		if _, err := env.db.Exec(ctx, query); err == nil {
			t.Fatalf("unsafe change accepted: %s", query)
		}
	}
	// Ordinary profile updates must not be blocked by the guard.
	if _, err := env.db.Exec(ctx, `UPDATE users SET display_name='Updated' WHERE email='first@example.test'`); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentAdministratorDisableLeavesOne(t *testing.T) {
	for _, isolation := range []string{"READ COMMITTED", "REPEATABLE READ"} {
		t.Run(isolation, func(t *testing.T) {
			env := newStoreIntegrationEnv(t)
			ctx := context.Background()
			first := seedGuardAdmin(t, env, "one@example.test")
			second := seedGuardAdmin(t, env, "two@example.test")
			if _, err := env.store.BootstrapPlatformAdmin(ctx, "ignored@example.test", "fixture", nil); err != nil {
				t.Fatal(err)
			}
			var success atomic.Int32
			var wg sync.WaitGroup
			ready := make(chan struct{}, 2)
			start := make(chan struct{})
			for _, id := range []string{first, second} {
				wg.Add(1)
				go func(id string) {
					defer wg.Done()
					tx, err := env.db.Begin(ctx)
					if err != nil {
						t.Error(err)
						return
					}
					defer tx.Rollback(ctx)
					if _, err = tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL "+isolation); err != nil {
						t.Error(err)
						return
					}
					// Establish snapshots before either writer starts.
					if _, err = tx.Exec(ctx, `SELECT count(*) FROM users`); err != nil {
						t.Error(err)
						return
					}
					ready <- struct{}{}
					<-start
					if _, err = tx.Exec(ctx, `UPDATE users SET disabled_at=now() WHERE id=$1`, id); err != nil {
						return
					}
					if err = tx.Commit(ctx); err == nil {
						success.Add(1)
					}
				}(id)
			}
			<-ready
			<-ready
			close(start)
			wg.Wait()
			if success.Load() != 1 {
				t.Fatalf("expected one successful removal, got %d", success.Load())
			}
			var count int
			if err := env.db.QueryRow(ctx, `SELECT count(*) FROM users WHERE platform_admin AND disabled_at IS NULL`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("remaining admins %d: %v", count, err)
			}
		})
	}
}
