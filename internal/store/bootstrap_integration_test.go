package store

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

func TestBootstrapSealsOnceWithoutCredentialReset(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	var created atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := env.store.BootstrapPlatformAdmin(ctx, "root@example.test", "original-hash", nil)
			if err != nil {
				t.Error(err)
			}
			if ok {
				created.Add(1)
			}
		}()
	}
	wg.Wait()
	if created.Load() != 1 {
		t.Fatalf("created %d administrators", created.Load())
	}
	seedGuardAdmin(t, env, "backup@example.test")
	if _, err := env.db.Exec(ctx, `UPDATE users SET disabled_at=now() WHERE email='root@example.test'`); err != nil {
		t.Fatal(err)
	}
	ok, err := env.store.BootstrapPlatformAdmin(ctx, "root@example.test", "reset-hash", nil)
	if err != nil || ok {
		t.Fatalf("bootstrap replay: %v %v", ok, err)
	}
	ok, err = env.store.BootstrapPlatformAdmin(ctx, "second@example.test", "reset-hash", nil)
	if err != nil || ok {
		t.Fatalf("second bootstrap: %v %v", ok, err)
	}
	var hash string
	var disabled bool
	var count int
	if err = env.db.QueryRow(ctx, `SELECT password_hash,disabled_at IS NOT NULL FROM users WHERE email='root@example.test'`).Scan(&hash, &disabled); err != nil {
		t.Fatal(err)
	}
	if hash != "original-hash" || !disabled {
		t.Fatal("startup reset or resurrected administrator")
	}
	if err = env.db.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("extra user: %d %v", count, err)
	}
	if _, err = env.store.EnsurePlatformAdmin(ctx, "root@example.test", "reset-hash", nil); err != ErrConflict {
		t.Fatalf("legacy helper bypassed seal: %v", err)
	}
	if _, err = env.db.Exec(ctx, `UPDATE platform_bootstrap SET sealed_at=NULL,reason=NULL`); err == nil {
		t.Fatal("seal can be cleared")
	}
	if _, err = env.db.Exec(ctx, `DELETE FROM platform_bootstrap`); err == nil {
		t.Fatal("seal can be deleted")
	}
}

func TestBootstrapSealsExistingDisabledAdministrator(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	admin, err := env.store.EnsurePlatformAdmin(ctx, "existing@example.test", "old-hash", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = env.db.Exec(ctx, `UPDATE users SET disabled_at=now() WHERE id=$1`, admin.ID); err != nil {
		t.Fatal(err)
	}
	ok, err := env.store.BootstrapPlatformAdmin(ctx, "new@example.test", "new-hash", nil)
	if err != nil || ok {
		t.Fatalf("existing installation bootstrap: %v %v", ok, err)
	}
	var sealed bool
	if err = env.db.QueryRow(ctx, `SELECT sealed_at IS NOT NULL FROM platform_bootstrap`).Scan(&sealed); err != nil || !sealed {
		t.Fatalf("existing installation not sealed: %v", err)
	}
}
