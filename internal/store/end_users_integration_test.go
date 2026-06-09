package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestEndUserPersistenceErrorPaths(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()

	first, err := env.store.CreateEndUser(ctx, EndUserCreateInput{
		Email:        "consumer-store@example.com",
		PasswordHash: "hash-one",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.CreateEndUser(ctx, EndUserCreateInput{
		Email:        " consumer-store@example.com ",
		PasswordHash: "hash-two",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected duplicate end user ErrConflict, got %v", err)
	}
	if _, err := env.store.GetEndUser(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected missing end user ErrNotFound, got %v", err)
	}
	if _, err := env.store.GetEndUserPassword(ctx, "missing-consumer@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected missing end user password ErrNotFound, got %v", err)
	}

	second, err := env.store.CreateEndUser(ctx, EndUserCreateInput{
		Email:        "consumer-store-two@example.com",
		PasswordHash: "hash-two",
	})
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Now().UTC().Add(time.Hour)
	if err := env.store.SaveEndUserRefreshToken(ctx, first.ID, "old-end-user-refresh", expiresAt); err != nil {
		t.Fatal(err)
	}
	if err := env.store.RotateEndUserRefreshToken(ctx, "old-end-user-refresh", "wrong-owner-refresh", second.ID, expiresAt); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected mismatched end user refresh ErrNotFound, got %v", err)
	}
	if err := env.store.RotateEndUserRefreshToken(ctx, "missing-end-user-refresh", "new-end-user-refresh", first.ID, expiresAt); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected missing end user refresh ErrNotFound, got %v", err)
	}
	if err := env.store.RotateEndUserRefreshToken(ctx, "old-end-user-refresh", "new-end-user-refresh", first.ID, expiresAt); err != nil {
		t.Fatalf("expected end user refresh rotation to succeed, got %v", err)
	}
}
