package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"rtk_account_manager/internal/model"
)

type IdentityProviderCreateInput struct {
	ProviderID      string
	Name            string
	Type            model.IdentityProviderType
	IssuerURL       string
	ClientID        string
	ClientSecretRef *string
	Scopes          []string
	Enabled         bool
	Metadata        map[string]any
	Now             time.Time
}

type IdentityProviderUpdateInput struct {
	ProviderID      string
	Name            *string
	IssuerURL       *string
	ClientID        *string
	ClientSecretRef *string
	Scopes          []string
	Enabled         *bool
	Metadata        map[string]any
	Now             time.Time
}

type IdentityProviderListFilter struct {
	Limit  int
	Offset int
}

type UserIdentityCreateInput struct {
	UserID        string
	ProviderID    string
	IssuerURL     string
	Subject       string
	Email         string
	EmailVerified bool
	Claims        map[string]any
	Now           time.Time
}

type OIDCLoginStateCreateInput struct {
	ProviderID           string
	StateHash            string
	NonceHash            string
	RedirectURL          string
	PostLoginRedirectURL *string
	ExpiresAt            time.Time
	Now                  time.Time
}

func (s *Store) CreateIdentityProvider(ctx context.Context, in IdentityProviderCreateInput) (model.IdentityProvider, error) {
	metadata, err := json.Marshal(defaultMetadata(in.Metadata))
	if err != nil {
		return model.IdentityProvider{}, err
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	scopes := in.Scopes
	if len(scopes) == 0 {
		scopes = []string{"openid", "email", "profile"}
	}
	providerType := in.Type
	if providerType == "" {
		providerType = model.IdentityProviderTypeOIDC
	}
	return scanIdentityProvider(s.db.QueryRow(ctx, `
		INSERT INTO identity_providers (
			provider_id,
			name,
			type,
			issuer_url,
			client_id,
			client_secret_ref,
			scopes,
			enabled,
			metadata,
			created_at,
			updated_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10)
		RETURNING id::text, provider_id, name, type, issuer_url, client_id, client_secret_ref, scopes, enabled, metadata, created_at, updated_at
	`, strings.TrimSpace(in.ProviderID), strings.TrimSpace(in.Name), providerType, strings.TrimSpace(in.IssuerURL), strings.TrimSpace(in.ClientID), in.ClientSecretRef, scopes, in.Enabled, metadata, now))
}

func (s *Store) ListIdentityProviders(ctx context.Context, in IdentityProviderListFilter) (IdentityProviderPage, error) {
	var total int
	if err := s.db.QueryRow(ctx, `SELECT count(*)::int FROM identity_providers`).Scan(&total); err != nil {
		return IdentityProviderPage{}, err
	}
	rows, err := s.db.Query(ctx, `
		SELECT id::text, provider_id, name, type, issuer_url, client_id, client_secret_ref, scopes, enabled, metadata, created_at, updated_at
		FROM identity_providers
		ORDER BY created_at DESC
		LIMIT $1 OFFSET $2
	`, in.Limit, in.Offset)
	if err != nil {
		return IdentityProviderPage{}, err
	}
	defer rows.Close()

	providers := []model.IdentityProvider{}
	for rows.Next() {
		provider, err := scanIdentityProvider(rows)
		if err != nil {
			return IdentityProviderPage{}, err
		}
		providers = append(providers, provider)
	}
	if err := rows.Err(); err != nil {
		return IdentityProviderPage{}, err
	}
	return IdentityProviderPage{Providers: providers, Page: Page{Limit: in.Limit, Offset: in.Offset, Total: total}}, nil
}

func (s *Store) GetIdentityProviderByProviderID(ctx context.Context, providerID string) (model.IdentityProvider, error) {
	provider, err := scanIdentityProvider(s.db.QueryRow(ctx, `
		SELECT id::text, provider_id, name, type, issuer_url, client_id, client_secret_ref, scopes, enabled, metadata, created_at, updated_at
		FROM identity_providers
		WHERE provider_id = $1
	`, providerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.IdentityProvider{}, ErrNotFound
	}
	return provider, err
}

func (s *Store) GetEnabledIdentityProvider(ctx context.Context) (model.IdentityProvider, error) {
	provider, err := scanIdentityProvider(s.db.QueryRow(ctx, `
		SELECT id::text, provider_id, name, type, issuer_url, client_id, client_secret_ref, scopes, enabled, metadata, created_at, updated_at
		FROM identity_providers
		WHERE enabled IS TRUE
		LIMIT 1
	`))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.IdentityProvider{}, ErrNotFound
	}
	return provider, err
}

func (s *Store) UpdateIdentityProvider(ctx context.Context, in IdentityProviderUpdateInput) (model.IdentityProvider, error) {
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var metadata []byte
	var err error
	if in.Metadata != nil {
		metadata, err = json.Marshal(defaultMetadata(in.Metadata))
		if err != nil {
			return model.IdentityProvider{}, err
		}
	}
	var scopes any
	if in.Scopes != nil {
		scopes = in.Scopes
	}
	provider, err := scanIdentityProvider(s.db.QueryRow(ctx, `
		UPDATE identity_providers
		SET
			name = COALESCE($2, name),
			issuer_url = COALESCE($3, issuer_url),
			client_id = COALESCE($4, client_id),
			client_secret_ref = COALESCE($5, client_secret_ref),
			scopes = COALESCE($6, scopes),
			enabled = COALESCE($7, enabled),
			metadata = COALESCE($8, metadata),
			updated_at = $9
		WHERE provider_id = $1
		RETURNING id::text, provider_id, name, type, issuer_url, client_id, client_secret_ref, scopes, enabled, metadata, created_at, updated_at
	`, in.ProviderID, in.Name, in.IssuerURL, in.ClientID, in.ClientSecretRef, scopes, in.Enabled, metadata, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.IdentityProvider{}, ErrNotFound
	}
	return provider, err
}

func (s *Store) DisableIdentityProvider(ctx context.Context, providerID string, now time.Time) (model.IdentityProvider, error) {
	enabled := false
	return s.UpdateIdentityProvider(ctx, IdentityProviderUpdateInput{
		ProviderID: providerID,
		Enabled:    &enabled,
		Now:        now,
	})
}

func (s *Store) CreateUserIdentity(ctx context.Context, in UserIdentityCreateInput) (model.UserIdentity, error) {
	claims, err := json.Marshal(defaultMetadata(in.Claims))
	if err != nil {
		return model.UserIdentity{}, err
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return scanUserIdentity(s.db.QueryRow(ctx, `
		WITH inserted AS (
			INSERT INTO user_identities (
			user_id,
			provider_id,
			issuer_url,
			subject,
			email,
			email_verified,
			claims,
			linked_at,
			created_at,
			updated_at
			)
			VALUES ($1, $2, $3, $4, lower(btrim($5)), $6, $7, $8, $8, $8)
			RETURNING *
		)
		SELECT inserted.id::text, inserted.user_id::text, inserted.provider_id::text, identity_providers.provider_id, inserted.issuer_url, inserted.subject, inserted.email, inserted.email_verified, inserted.claims, inserted.linked_at, inserted.last_login_at, inserted.created_at, inserted.updated_at
		FROM inserted
		JOIN identity_providers ON identity_providers.id = inserted.provider_id
	`, in.UserID, in.ProviderID, strings.TrimSpace(in.IssuerURL), strings.TrimSpace(in.Subject), in.Email, in.EmailVerified, claims, now))
}

func (s *Store) ListUserIdentities(ctx context.Context, userID string) ([]model.UserIdentity, error) {
	rows, err := s.db.Query(ctx, `
		SELECT user_identities.id::text, user_identities.user_id::text, user_identities.provider_id::text, identity_providers.provider_id, user_identities.issuer_url, user_identities.subject, user_identities.email, user_identities.email_verified, user_identities.claims, user_identities.linked_at, user_identities.last_login_at, user_identities.created_at, user_identities.updated_at
		FROM user_identities
		JOIN identity_providers ON identity_providers.id = user_identities.provider_id
		WHERE user_identities.user_id = $1
		ORDER BY user_identities.linked_at DESC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	identities := []model.UserIdentity{}
	for rows.Next() {
		identity, err := scanUserIdentity(rows)
		if err != nil {
			return nil, err
		}
		identities = append(identities, identity)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return identities, nil
}

func (s *Store) GetUserIdentityByProviderSubject(ctx context.Context, providerID, subject string) (model.UserIdentity, error) {
	identity, err := scanUserIdentity(s.db.QueryRow(ctx, `
		SELECT user_identities.id::text, user_identities.user_id::text, user_identities.provider_id::text, identity_providers.provider_id, user_identities.issuer_url, user_identities.subject, user_identities.email, user_identities.email_verified, user_identities.claims, user_identities.linked_at, user_identities.last_login_at, user_identities.created_at, user_identities.updated_at
		FROM user_identities
		JOIN identity_providers ON identity_providers.id = user_identities.provider_id
		WHERE user_identities.provider_id = $1 AND user_identities.subject = $2
	`, providerID, subject))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.UserIdentity{}, ErrNotFound
	}
	return identity, err
}

func (s *Store) UpdateUserIdentityLastLogin(ctx context.Context, identityID string, lastLoginAt time.Time) (model.UserIdentity, error) {
	identity, err := scanUserIdentity(s.db.QueryRow(ctx, `
		WITH updated AS (
			UPDATE user_identities
			SET last_login_at = $2, updated_at = $2
			WHERE id = $1
			RETURNING *
		)
		SELECT updated.id::text, updated.user_id::text, updated.provider_id::text, identity_providers.provider_id, updated.issuer_url, updated.subject, updated.email, updated.email_verified, updated.claims, updated.linked_at, updated.last_login_at, updated.created_at, updated.updated_at
		FROM updated
		JOIN identity_providers ON identity_providers.id = updated.provider_id
	`, identityID, lastLoginAt))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.UserIdentity{}, ErrNotFound
	}
	return identity, err
}

func (s *Store) DeleteUserIdentity(ctx context.Context, userID, identityID string) error {
	tag, err := s.db.Exec(ctx, `
		DELETE FROM user_identities
		WHERE user_id = $1 AND id = $2
	`, userID, identityID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) CreateOIDCLoginState(ctx context.Context, in OIDCLoginStateCreateInput) (model.OIDCLoginState, error) {
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return scanOIDCLoginState(s.db.QueryRow(ctx, `
		INSERT INTO oidc_login_states (
			provider_id,
			state_hash,
			nonce_hash,
			redirect_url,
			post_login_redirect_url,
			expires_at,
			created_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id::text, provider_id::text, state_hash, nonce_hash, redirect_url, post_login_redirect_url, expires_at, consumed_at, created_at
	`, in.ProviderID, in.StateHash, in.NonceHash, in.RedirectURL, in.PostLoginRedirectURL, in.ExpiresAt, now))
}

func (s *Store) ConsumeOIDCLoginState(ctx context.Context, stateHash string, now time.Time) (model.OIDCLoginState, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.OIDCLoginState{}, err
	}
	defer tx.Rollback(ctx)

	state, err := scanOIDCLoginState(tx.QueryRow(ctx, `
		SELECT id::text, provider_id::text, state_hash, nonce_hash, redirect_url, post_login_redirect_url, expires_at, consumed_at, created_at
		FROM oidc_login_states
		WHERE state_hash = $1
		FOR UPDATE
	`, stateHash))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.OIDCLoginState{}, ErrOIDCStateInvalid
	}
	if err != nil {
		return model.OIDCLoginState{}, err
	}
	if state.ConsumedAt != nil {
		return model.OIDCLoginState{}, ErrOIDCStateInvalid
	}
	if !state.ExpiresAt.After(now) {
		return model.OIDCLoginState{}, ErrOIDCStateExpired
	}
	consumed, err := scanOIDCLoginState(tx.QueryRow(ctx, `
		UPDATE oidc_login_states
		SET consumed_at = $2
		WHERE id = $1
		RETURNING id::text, provider_id::text, state_hash, nonce_hash, redirect_url, post_login_redirect_url, expires_at, consumed_at, created_at
	`, state.ID, now))
	if err != nil {
		return model.OIDCLoginState{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.OIDCLoginState{}, err
	}
	return consumed, nil
}

func scanIdentityProvider(row rowScanner) (model.IdentityProvider, error) {
	var provider model.IdentityProvider
	var metadata []byte
	err := row.Scan(
		&provider.ID,
		&provider.ProviderID,
		&provider.Name,
		&provider.Type,
		&provider.IssuerURL,
		&provider.ClientID,
		&provider.ClientSecretRef,
		&provider.Scopes,
		&provider.Enabled,
		&metadata,
		&provider.CreatedAt,
		&provider.UpdatedAt,
	)
	if err != nil {
		return model.IdentityProvider{}, err
	}
	provider.Metadata, err = unmarshalJSONMap(metadata)
	if err != nil {
		return model.IdentityProvider{}, err
	}
	return provider, nil
}

func scanUserIdentity(row rowScanner) (model.UserIdentity, error) {
	var identity model.UserIdentity
	var claims []byte
	err := row.Scan(
		&identity.ID,
		&identity.UserID,
		&identity.ProviderID,
		&identity.ProviderKey,
		&identity.IssuerURL,
		&identity.Subject,
		&identity.Email,
		&identity.EmailVerified,
		&claims,
		&identity.LinkedAt,
		&identity.LastLoginAt,
		&identity.CreatedAt,
		&identity.UpdatedAt,
	)
	if err != nil {
		return model.UserIdentity{}, err
	}
	identity.Claims, err = unmarshalJSONMap(claims)
	if err != nil {
		return model.UserIdentity{}, err
	}
	return identity, nil
}

func scanOIDCLoginState(row rowScanner) (model.OIDCLoginState, error) {
	var state model.OIDCLoginState
	err := row.Scan(
		&state.ID,
		&state.ProviderID,
		&state.StateHash,
		&state.NonceHash,
		&state.RedirectURL,
		&state.PostLoginRedirectURL,
		&state.ExpiresAt,
		&state.ConsumedAt,
		&state.CreatedAt,
	)
	if err != nil {
		return model.OIDCLoginState{}, err
	}
	return state, nil
}
