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

type ChipsetProviderWriteInput struct {
	Name        string
	ManifestURL string
	ActorUserID string
}

type ChipsetProviderRefreshInput struct {
	ProviderID      string
	ManifestVersion string
	ManifestSHA256  string
	ETag            string
	LastModified    string
	Chipsets        []model.DeveloperChipset
	AttemptedAt     time.Time
}

func (s *Store) CreateChipsetProvider(ctx context.Context, in ChipsetProviderWriteInput) (model.ChipsetProvider, error) {
	row := s.db.QueryRow(ctx, `
		INSERT INTO chipset_information_providers (name, manifest_url, created_by)
		VALUES ($1, $2, NULLIF($3, '')::uuid)
		RETURNING `+chipsetProviderColumns,
		strings.TrimSpace(in.Name), strings.TrimSpace(in.ManifestURL), strings.TrimSpace(in.ActorUserID))
	return scanChipsetProvider(row)
}

func (s *Store) UpdateChipsetProvider(ctx context.Context, providerID string, in ChipsetProviderWriteInput) (model.ChipsetProvider, error) {
	row := s.db.QueryRow(ctx, `
		UPDATE chipset_information_providers
		SET name = $2, manifest_url = $3, validation_error = '', stale = false
		WHERE id::text = $1 AND status IN ('draft', 'unpublished')
		RETURNING `+chipsetProviderColumns,
		strings.TrimSpace(providerID), strings.TrimSpace(in.Name), strings.TrimSpace(in.ManifestURL))
	provider, err := scanChipsetProvider(row)
	if errors.Is(err, ErrNotFound) {
		var exists bool
		if scanErr := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM chipset_information_providers WHERE id::text = $1)`, strings.TrimSpace(providerID)).Scan(&exists); scanErr == nil && exists {
			return model.ChipsetProvider{}, ErrConflict
		}
	}
	return provider, err
}

func (s *Store) GetChipsetProvider(ctx context.Context, providerID string) (model.ChipsetProvider, []model.DeveloperChipset, error) {
	var raw []byte
	row := s.db.QueryRow(ctx, `SELECT `+chipsetProviderColumns+`, COALESCE(snapshot, '[]'::jsonb) FROM chipset_information_providers WHERE id::text = $1`, strings.TrimSpace(providerID))
	provider, err := scanChipsetProviderWithSnapshot(row, &raw)
	if err != nil {
		return model.ChipsetProvider{}, nil, err
	}
	chipsets, err := decodeChipsetSnapshot(provider, raw)
	return provider, chipsets, err
}

func (s *Store) ListChipsetProviders(ctx context.Context) ([]model.ChipsetProvider, error) {
	rows, err := s.db.Query(ctx, `SELECT `+chipsetProviderColumns+` FROM chipset_information_providers ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	providers := []model.ChipsetProvider{}
	for rows.Next() {
		provider, err := scanChipsetProvider(rows)
		if err != nil {
			return nil, err
		}
		providers = append(providers, provider)
	}
	return providers, rows.Err()
}

func (s *Store) ListPublishedChipsets(ctx context.Context) ([]model.DeveloperChipset, error) {
	rows, err := s.db.Query(ctx, `SELECT `+chipsetProviderColumns+`, snapshot FROM chipset_information_providers WHERE status = 'published' AND snapshot IS NOT NULL ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []model.DeveloperChipset{}
	for rows.Next() {
		var raw []byte
		provider, err := scanChipsetProviderWithSnapshot(rows, &raw)
		if err != nil {
			return nil, err
		}
		chipsets, err := decodeChipsetSnapshot(provider, raw)
		if err != nil {
			return nil, err
		}
		result = append(result, chipsets...)
	}
	return result, rows.Err()
}

func (s *Store) CommitChipsetProviderRefresh(ctx context.Context, in ChipsetProviderRefreshInput) (model.ChipsetProvider, error) {
	raw, err := json.Marshal(in.Chipsets)
	if err != nil {
		return model.ChipsetProvider{}, err
	}
	sdkCount := 0
	for _, chipset := range in.Chipsets {
		sdkCount += len(chipset.SDKReleases)
	}
	row := s.db.QueryRow(ctx, `
		UPDATE chipset_information_providers
		SET manifest_version = $2, manifest_sha256 = $3, etag = $4,
			last_modified = $5, snapshot = $6, chipset_count = $7,
			sdk_release_count = $8, last_refresh_attempt_at = $9,
			last_successful_refresh_at = $9, stale = false, validation_error = ''
		WHERE id::text = $1
		RETURNING `+chipsetProviderColumns,
		in.ProviderID, in.ManifestVersion, in.ManifestSHA256, in.ETag, in.LastModified,
		raw, len(in.Chipsets), sdkCount, in.AttemptedAt.UTC())
	return scanChipsetProvider(row)
}

func (s *Store) MarkChipsetProviderNotModified(ctx context.Context, providerID string, attemptedAt time.Time) (model.ChipsetProvider, error) {
	row := s.db.QueryRow(ctx, `UPDATE chipset_information_providers SET last_refresh_attempt_at = $2, last_successful_refresh_at = $2, stale = false, validation_error = '' WHERE id::text = $1 AND snapshot IS NOT NULL RETURNING `+chipsetProviderColumns, providerID, attemptedAt.UTC())
	return scanChipsetProvider(row)
}

func (s *Store) MarkChipsetProviderRefreshFailed(ctx context.Context, providerID, message string, attemptedAt time.Time) (model.ChipsetProvider, error) {
	row := s.db.QueryRow(ctx, `UPDATE chipset_information_providers SET last_refresh_attempt_at = $2, stale = snapshot IS NOT NULL, validation_error = $3 WHERE id::text = $1 RETURNING `+chipsetProviderColumns, providerID, attemptedAt.UTC(), strings.TrimSpace(message))
	return scanChipsetProvider(row)
}

func (s *Store) SetChipsetProviderStatus(ctx context.Context, providerID string, status model.ChipsetProviderStatus) (model.ChipsetProvider, error) {
	row := s.db.QueryRow(ctx, `UPDATE chipset_information_providers SET status = $2 WHERE id::text = $1 AND ($2 <> 'published' OR snapshot IS NOT NULL) RETURNING `+chipsetProviderColumns, providerID, status)
	provider, err := scanChipsetProvider(row)
	if errors.Is(err, ErrNotFound) && status == model.ChipsetProviderStatusPublished {
		return model.ChipsetProvider{}, ErrNotProvisioned
	}
	return provider, err
}

const chipsetProviderColumns = `id::text, name, manifest_url, status, manifest_version, manifest_sha256, etag, last_modified, chipset_count, sdk_release_count, last_refresh_attempt_at, last_successful_refresh_at, stale, validation_error, created_at, updated_at`

type chipsetProviderScanner interface{ Scan(...any) error }

func scanChipsetProvider(row chipsetProviderScanner) (model.ChipsetProvider, error) {
	var provider model.ChipsetProvider
	if err := row.Scan(&provider.ID, &provider.Name, &provider.ManifestURL, &provider.Status,
		&provider.ManifestVersion, &provider.ManifestSHA256, &provider.ETag, &provider.LastModified,
		&provider.ChipsetCount, &provider.SDKReleaseCount, &provider.LastRefreshAttemptAt,
		&provider.LastSuccessfulRefreshAt, &provider.Stale, &provider.ValidationError,
		&provider.CreatedAt, &provider.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.ChipsetProvider{}, ErrNotFound
		}
		return model.ChipsetProvider{}, err
	}
	provider.Unavailable = provider.LastSuccessfulRefreshAt == nil
	return provider, nil
}

func scanChipsetProviderWithSnapshot(row chipsetProviderScanner, raw *[]byte) (model.ChipsetProvider, error) {
	var provider model.ChipsetProvider
	if err := row.Scan(&provider.ID, &provider.Name, &provider.ManifestURL, &provider.Status,
		&provider.ManifestVersion, &provider.ManifestSHA256, &provider.ETag, &provider.LastModified,
		&provider.ChipsetCount, &provider.SDKReleaseCount, &provider.LastRefreshAttemptAt,
		&provider.LastSuccessfulRefreshAt, &provider.Stale, &provider.ValidationError,
		&provider.CreatedAt, &provider.UpdatedAt, raw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.ChipsetProvider{}, ErrNotFound
		}
		return model.ChipsetProvider{}, err
	}
	provider.Unavailable = provider.LastSuccessfulRefreshAt == nil
	return provider, nil
}

func decodeChipsetSnapshot(provider model.ChipsetProvider, raw []byte) ([]model.DeveloperChipset, error) {
	chipsets := []model.DeveloperChipset{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &chipsets); err != nil {
			return nil, err
		}
	}
	for i := range chipsets {
		chipsets[i].ProviderID = provider.ID
		chipsets[i].ProviderName = provider.Name
		chipsets[i].Stale = provider.Stale
		if provider.LastSuccessfulRefreshAt != nil {
			chipsets[i].LastSuccessfulRefreshAt = *provider.LastSuccessfulRefreshAt
		}
	}
	return chipsets, nil
}
