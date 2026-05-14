package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"rtk_account_manager/internal/model"
)

func TestIdentityProviderStoreCRUDAndEnabledInvariant(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	secretRef := "env:OIDC_CLIENT_SECRET"

	provider, err := env.store.CreateIdentityProvider(ctx, IdentityProviderCreateInput{
		ProviderID:      "keycloak",
		Name:            "Keycloak",
		Type:            model.IdentityProviderTypeOIDC,
		IssuerURL:       "https://keycloak.example.test/realms/account",
		ClientID:        "account-manager",
		ClientSecretRef: &secretRef,
		Scopes:          []string{"openid", "email", "profile"},
		Enabled:         true,
		Metadata:        map[string]any{"realm": "account"},
		Now:             now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if provider.ProviderID != "keycloak" || provider.Type != model.IdentityProviderTypeOIDC || !provider.Enabled {
		t.Fatalf("unexpected provider: %+v", provider)
	}
	if provider.ClientSecretRef == nil || *provider.ClientSecretRef != secretRef {
		t.Fatalf("expected secret ref only, got %+v", provider)
	}
	if got := provider.Metadata["realm"]; got != "account" {
		t.Fatalf("expected metadata to round trip, got %+v", provider.Metadata)
	}

	fetched, err := env.store.GetIdentityProviderByProviderID(ctx, "keycloak")
	if err != nil {
		t.Fatal(err)
	}
	if fetched.ID != provider.ID {
		t.Fatalf("expected provider %s, got %+v", provider.ID, fetched)
	}

	listed, err := env.store.ListIdentityProviders(ctx, IdentityProviderListFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if listed.Page.Total != 1 || len(listed.Providers) != 1 || listed.Providers[0].ID != provider.ID {
		t.Fatalf("expected provider in list, got %+v", listed)
	}

	updatedName := "Keycloak Updated"
	disabled, err := env.store.UpdateIdentityProvider(ctx, IdentityProviderUpdateInput{
		ProviderID: "keycloak",
		Name:       &updatedName,
		Enabled:    boolPtr(false),
		Metadata:   map[string]any{"realm": "updated"},
		Now:        now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Name != updatedName || disabled.Enabled {
		t.Fatalf("expected disabled updated provider, got %+v", disabled)
	}

	enabled, err := env.store.UpdateIdentityProvider(ctx, IdentityProviderUpdateInput{
		ProviderID: "keycloak",
		Enabled:    boolPtr(true),
		Now:        now.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !enabled.Enabled {
		t.Fatalf("expected enabled provider, got %+v", enabled)
	}

	_, err = env.store.CreateIdentityProvider(ctx, IdentityProviderCreateInput{
		ProviderID:      "second",
		Name:            "Second",
		Type:            model.IdentityProviderTypeOIDC,
		IssuerURL:       "https://second.example.test/realms/account",
		ClientID:        "account-manager",
		ClientSecretRef: &secretRef,
		Scopes:          []string{"openid"},
		Enabled:         true,
		Now:             now,
	})
	if err == nil {
		t.Fatal("expected second enabled provider to be rejected")
	}

	var rawSecretCount int
	if err := env.db.QueryRow(ctx, `
		SELECT count(*)::int
		FROM identity_providers
		WHERE client_secret_ref = 'raw-client-secret' OR metadata::text LIKE '%raw-client-secret%'
	`).Scan(&rawSecretCount); err != nil {
		t.Fatal(err)
	}
	if rawSecretCount != 0 {
		t.Fatal("raw OIDC client secret was persisted")
	}
}

func TestIdentityProviderRejectsRawClientSecretRef(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	rawSecret := "raw-client-secret"

	_, err := env.store.CreateIdentityProvider(ctx, IdentityProviderCreateInput{
		ProviderID:      "bad-secret",
		Name:            "Bad Secret",
		Type:            model.IdentityProviderTypeOIDC,
		IssuerURL:       "https://keycloak.example.test/realms/account",
		ClientID:        "account-manager",
		ClientSecretRef: &rawSecret,
		Scopes:          []string{"openid"},
	})
	if err == nil {
		t.Fatal("expected raw client secret ref to be rejected")
	}
}

func TestUserIdentityStoreEnforcesUniquenessAndListsByUser(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	registered, err := env.store.Register(ctx, RegisterInput{
		Email:            "oidc-user@example.com",
		PasswordHash:     "hash",
		OrganizationName: "OIDC User Org",
	})
	if err != nil {
		t.Fatal(err)
	}
	provider := createOIDCTestProvider(t, env, "keycloak", false)
	now := time.Now().UTC().Truncate(time.Microsecond)

	identity, err := env.store.CreateUserIdentity(ctx, UserIdentityCreateInput{
		UserID:        registered.User.ID,
		ProviderID:    provider.ID,
		IssuerURL:     provider.IssuerURL,
		Subject:       "subject-1",
		Email:         "oidc-user@example.com",
		EmailVerified: true,
		Claims:        map[string]any{"preferred_username": "oidc-user"},
		Now:           now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if identity.ProviderKey != "keycloak" || identity.Email != "oidc-user@example.com" || !identity.EmailVerified {
		t.Fatalf("unexpected identity: %+v", identity)
	}

	listed, err := env.store.ListUserIdentities(ctx, registered.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != identity.ID {
		t.Fatalf("expected identity in user list, got %+v", listed)
	}

	_, err = env.store.GetUserIdentityByProviderSubject(ctx, provider.ID, "subject-1")
	if err != nil {
		t.Fatal(err)
	}

	_, err = env.store.CreateUserIdentity(ctx, UserIdentityCreateInput{
		UserID:        registered.User.ID,
		ProviderID:    provider.ID,
		IssuerURL:     provider.IssuerURL,
		Subject:       "subject-1",
		Email:         "oidc-user@example.com",
		EmailVerified: true,
	})
	if err == nil {
		t.Fatal("expected duplicate provider subject to be rejected")
	}

	lastLoginAt := now.Add(time.Minute)
	updated, err := env.store.UpdateUserIdentityLastLogin(ctx, identity.ID, lastLoginAt)
	if err != nil {
		t.Fatal(err)
	}
	if updated.LastLoginAt == nil || !updated.LastLoginAt.Equal(lastLoginAt) {
		t.Fatalf("expected last login update, got %+v", updated)
	}

	if err := env.store.DeleteUserIdentity(ctx, registered.User.ID, identity.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.GetUserIdentityByProviderSubject(ctx, provider.ID, "subject-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected identity to be deleted, got %v", err)
	}
}

func TestOIDCLoginStateStoresHashesAndRejectsReplay(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	provider := createOIDCTestProvider(t, env, "keycloak", false)
	now := time.Now().UTC().Truncate(time.Microsecond)
	postLogin := "https://app.example.test/done"

	state, err := env.store.CreateOIDCLoginState(ctx, OIDCLoginStateCreateInput{
		ProviderID:           provider.ID,
		StateHash:            "hashed-state",
		NonceHash:            "hashed-nonce",
		RedirectURL:          "https://api.example.test/v1/auth/oidc/keycloak/callback",
		PostLoginRedirectURL: &postLogin,
		ExpiresAt:            now.Add(10 * time.Minute),
		Now:                  now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if state.StateHash != "hashed-state" || state.NonceHash != "hashed-nonce" {
		t.Fatalf("expected hashes to round trip, got %+v", state)
	}

	var rawCount int
	if err := env.db.QueryRow(ctx, `
		SELECT count(*)::int
		FROM oidc_login_states
		WHERE state_hash = 'raw-state' OR nonce_hash = 'raw-nonce'
	`).Scan(&rawCount); err != nil {
		t.Fatal(err)
	}
	if rawCount != 0 {
		t.Fatal("raw OIDC state or nonce was persisted")
	}

	consumed, err := env.store.ConsumeOIDCLoginState(ctx, "hashed-state", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if consumed.ConsumedAt == nil {
		t.Fatalf("expected consumed_at, got %+v", consumed)
	}
	if _, err := env.store.ConsumeOIDCLoginState(ctx, "hashed-state", now.Add(2*time.Minute)); !errors.Is(err, ErrOIDCStateInvalid) {
		t.Fatalf("expected replay to be rejected, got %v", err)
	}
	if _, err := env.store.ConsumeOIDCLoginState(ctx, "missing-state", now); !errors.Is(err, ErrOIDCStateInvalid) {
		t.Fatalf("expected missing state to be rejected, got %v", err)
	}
}

func TestOIDCLoginStateRejectsExpiredState(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	provider := createOIDCTestProvider(t, env, "keycloak", false)
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := env.store.CreateOIDCLoginState(ctx, OIDCLoginStateCreateInput{
		ProviderID:  provider.ID,
		StateHash:   "expired-state",
		NonceHash:   "expired-nonce",
		RedirectURL: "https://api.example.test/callback",
		ExpiresAt:   now.Add(-time.Minute),
		Now:         now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.ConsumeOIDCLoginState(ctx, "expired-state", now); !errors.Is(err, ErrOIDCStateExpired) {
		t.Fatalf("expected expired state to be rejected, got %v", err)
	}
}

func createOIDCTestProvider(t *testing.T, env storeIntegrationEnv, providerID string, enabled bool) model.IdentityProvider {
	t.Helper()
	secretRef := "env:OIDC_CLIENT_SECRET"
	provider, err := env.store.CreateIdentityProvider(context.Background(), IdentityProviderCreateInput{
		ProviderID:      providerID,
		Name:            "Keycloak",
		Type:            model.IdentityProviderTypeOIDC,
		IssuerURL:       "https://keycloak.example.test/realms/account",
		ClientID:        "account-manager",
		ClientSecretRef: &secretRef,
		Scopes:          []string{"openid", "email", "profile"},
		Enabled:         enabled,
		Metadata:        map[string]any{"realm": "account"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return provider
}
