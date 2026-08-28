package model

import (
	"encoding/json"
	"time"
)

type ChipsetProviderStatus string

const (
	ChipsetProviderStatusDraft       ChipsetProviderStatus = "draft"
	ChipsetProviderStatusPublished   ChipsetProviderStatus = "published"
	ChipsetProviderStatusUnpublished ChipsetProviderStatus = "unpublished"
)

type ChipsetEndpoint struct {
	Type       string         `json:"type"`
	Title      string         `json:"title"`
	URL        string         `json:"url"`
	Source     string         `json:"source,omitempty"`
	Languages  []string       `json:"languages,omitempty"`
	VerifiedAt string         `json:"verified_at,omitempty"`
	Summary    string         `json:"summary,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

func (e *ChipsetEndpoint) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for name, target := range map[string]*string{
		"type": &e.Type, "title": &e.Title, "url": &e.URL, "source": &e.Source,
		"verified_at": &e.VerifiedAt, "summary": &e.Summary,
	} {
		if raw, ok := fields[name]; ok {
			if err := json.Unmarshal(raw, target); err != nil {
				return err
			}
			delete(fields, name)
		}
	}
	if raw, ok := fields["languages"]; ok {
		if err := json.Unmarshal(raw, &e.Languages); err != nil {
			return err
		}
		delete(fields, "languages")
	}
	if raw, ok := fields["metadata"]; ok {
		if err := json.Unmarshal(raw, &e.Metadata); err != nil {
			return err
		}
		delete(fields, "metadata")
	}
	if e.Metadata == nil && len(fields) > 0 {
		e.Metadata = map[string]any{}
	}
	for name, raw := range fields {
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		e.Metadata[name] = value
	}
	return nil
}

type ChipsetSDKRelease struct {
	Name            string            `json:"name"`
	Version         string            `json:"version"`
	Summary         string            `json:"summary,omitempty"`
	Recommended     bool              `json:"recommended"`
	SupportedModels []string          `json:"supported_models"`
	Endpoints       []ChipsetEndpoint `json:"endpoints"`
}

type DeveloperChipset struct {
	ID                      string              `json:"id"`
	ProviderID              string              `json:"-"`
	ProviderName            string              `json:"provider_name"`
	ChipsetKey              string              `json:"chipset_key"`
	Vendor                  string              `json:"vendor"`
	Name                    string              `json:"name"`
	Family                  string              `json:"family,omitempty"`
	Description             string              `json:"description,omitempty"`
	Resources               []ChipsetEndpoint   `json:"resources,omitempty"`
	SDKReleases             []ChipsetSDKRelease `json:"sdk_releases"`
	Stale                   bool                `json:"stale"`
	LastSuccessfulRefreshAt time.Time           `json:"last_successful_refresh_at"`
}

type ChipsetProvider struct {
	ID                      string                `json:"id"`
	Name                    string                `json:"name"`
	ManifestURL             string                `json:"manifest_url"`
	Status                  ChipsetProviderStatus `json:"status"`
	ManifestVersion         string                `json:"manifest_version,omitempty"`
	ManifestSHA256          string                `json:"manifest_sha256,omitempty"`
	ETag                    string                `json:"etag,omitempty"`
	LastModified            string                `json:"last_modified,omitempty"`
	ChipsetCount            int                   `json:"chipset_count"`
	SDKReleaseCount         int                   `json:"sdk_release_count"`
	LastRefreshAttemptAt    *time.Time            `json:"last_refresh_attempt_at,omitempty"`
	LastSuccessfulRefreshAt *time.Time            `json:"last_successful_refresh_at,omitempty"`
	Stale                   bool                  `json:"stale"`
	Unavailable             bool                  `json:"unavailable"`
	ValidationError         string                `json:"validation_error,omitempty"`
	CreatedAt               time.Time             `json:"created_at"`
	UpdatedAt               time.Time             `json:"updated_at"`
}
