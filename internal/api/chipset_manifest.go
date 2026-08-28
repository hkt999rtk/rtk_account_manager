package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"rtk_account_manager/internal/model"
)

var (
	errChipsetProviderURLInvalid         = errors.New("provider URL is invalid")
	errChipsetProviderHostNotAllowed     = errors.New("provider host is not allowed")
	errChipsetProviderAddressNotPublic   = errors.New("provider address is not public")
	errChipsetManifestInvalid            = errors.New("provider manifest is invalid")
	errChipsetManifestVersionUnsupported = errors.New("provider manifest version is unsupported")
	errChipsetProviderSnapshotRequired   = errors.New("provider snapshot is required")
)

const (
	chipsetManifestMaxBytes     = 5 << 20
	chipsetManifestMaxDepth     = 32
	chipsetManifestMaxChipsets  = 64
	chipsetManifestMaxReleases  = 64
	chipsetManifestMaxEndpoints = 64
	chipsetManifestMaxModels    = 256
)

var chipsetPackageKeyPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
var chipsetLanguagePattern = regexp.MustCompile(`^[A-Za-z]{2,3}(?:-[A-Za-z0-9]{2,8})*$`)

type ChipsetManifestFetcher interface {
	ValidateURL(string) error
	Fetch(context.Context, model.ChipsetProvider) (ChipsetManifestFetchResult, error)
}

type ChipsetManifestFetcherConfig struct {
	AllowedHosts []string
	HTTPClient   *http.Client
	Resolver     func(context.Context, string) ([]net.IP, error)
}

type ChipsetManifestFetchResult struct {
	NotModified     bool
	ManifestVersion string
	ManifestSHA256  string
	ETag            string
	LastModified    string
	Chipsets        []model.DeveloperChipset
}

type httpChipsetManifestFetcher struct {
	allowedHosts []string
	client       *http.Client
	resolve      func(context.Context, string) ([]net.IP, error)
}

func NewChipsetManifestFetcher(cfg ChipsetManifestFetcherConfig) ChipsetManifestFetcher {
	resolve := cfg.Resolver
	if resolve == nil {
		resolve = func(ctx context.Context, host string) ([]net.IP, error) {
			addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			ips := make([]net.IP, 0, len(addrs))
			for _, addr := range addrs {
				ips = append(ips, addr.IP)
			}
			return ips, nil
		}
	}
	fetcher := &httpChipsetManifestFetcher{allowedHosts: normalizeAllowedHosts(cfg.AllowedHosts), resolve: resolve}
	if cfg.HTTPClient != nil {
		fetcher.client = cfg.HTTPClient
		return fetcher
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		if !fetcher.hostAllowed(host) {
			return nil, errChipsetProviderHostNotAllowed
		}
		ips, err := fetcher.resolve(ctx, host)
		if err != nil || len(ips) == 0 {
			return nil, fmt.Errorf("resolve provider host: %w", err)
		}
		for _, ip := range ips {
			if !publicIP(ip) {
				return nil, errChipsetProviderAddressNotPublic
			}
		}
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
	}
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	fetcher.client = &http.Client{
		Timeout:   10 * time.Second,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many provider redirects")
			}
			return fetcher.validateURL(req.Context(), req.URL)
		},
	}
	return fetcher
}

func (f *httpChipsetManifestFetcher) ValidateURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return errChipsetProviderURLInvalid
	}
	return f.validateURL(context.Background(), u)
}

func (f *httpChipsetManifestFetcher) validateURL(ctx context.Context, u *url.URL) error {
	if u == nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") {
		return errChipsetProviderURLInvalid
	}
	if !f.hostAllowed(u.Hostname()) {
		return errChipsetProviderHostNotAllowed
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		if !publicIP(ip) {
			return errChipsetProviderAddressNotPublic
		}
		return nil
	}
	ips, err := f.resolve(ctx, u.Hostname())
	if err != nil {
		return fmt.Errorf("resolve provider host: %w", err)
	}
	if len(ips) == 0 {
		return errChipsetProviderURLInvalid
	}
	for _, ip := range ips {
		if !publicIP(ip) {
			return errChipsetProviderAddressNotPublic
		}
	}
	return nil
}

func (f *httpChipsetManifestFetcher) hostAllowed(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	for _, allowed := range f.allowedHosts {
		if strings.HasPrefix(allowed, "*.") {
			base := strings.TrimPrefix(allowed, "*.")
			if host != base && strings.HasSuffix(host, "."+base) {
				return true
			}
			continue
		}
		if host == allowed {
			return true
		}
	}
	return false
}

func (f *httpChipsetManifestFetcher) Fetch(ctx context.Context, provider model.ChipsetProvider) (ChipsetManifestFetchResult, error) {
	if err := f.ValidateURL(provider.ManifestURL); err != nil {
		return ChipsetManifestFetchResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, provider.ManifestURL, nil)
	if err != nil {
		return ChipsetManifestFetchResult{}, err
	}
	req.Header.Set("Accept", "application/json")
	if provider.ETag != "" {
		req.Header.Set("If-None-Match", provider.ETag)
	}
	if provider.LastModified != "" {
		req.Header.Set("If-Modified-Since", provider.LastModified)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return ChipsetManifestFetchResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return ChipsetManifestFetchResult{NotModified: true, ETag: provider.ETag, LastModified: provider.LastModified}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return ChipsetManifestFetchResult{}, fmt.Errorf("provider returned HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, chipsetManifestMaxBytes+1))
	if err != nil {
		return ChipsetManifestFetchResult{}, err
	}
	if len(raw) > chipsetManifestMaxBytes {
		return ChipsetManifestFetchResult{}, fmt.Errorf("%w: response exceeds %d bytes", errChipsetManifestInvalid, chipsetManifestMaxBytes)
	}
	chipsets, version, err := parseChipsetManifest(provider.ID, raw)
	if err != nil {
		return ChipsetManifestFetchResult{}, err
	}
	hash := sha256.Sum256(raw)
	return ChipsetManifestFetchResult{
		ManifestVersion: version,
		ManifestSHA256:  hex.EncodeToString(hash[:]),
		ETag:            resp.Header.Get("ETag"),
		LastModified:    resp.Header.Get("Last-Modified"),
		Chipsets:        chipsets,
	}, nil
}

type chipsetManifestV1 struct {
	Schema          string `json:"$schema"`
	ManifestVersion string `json:"manifest_version"`
	Provider        struct {
		Name      string `json:"name"`
		UpdatedAt string `json:"updated_at"`
	} `json:"provider"`
	Chipsets []struct {
		ChipsetKey  string                    `json:"chipset_key"`
		Vendor      string                    `json:"vendor"`
		Name        string                    `json:"name"`
		Family      string                    `json:"family"`
		Description string                    `json:"description"`
		Resources   []model.ChipsetEndpoint   `json:"resources"`
		SDKReleases []model.ChipsetSDKRelease `json:"sdk_releases"`
	} `json:"chipsets"`
}

// ValidateChipsetResourcePackage validates the stricter one-chipset authoring
// profile. Runtime parsing remains compatible with older v1 provider manifests.
func ValidateChipsetResourcePackage(raw []byte) error {
	if err := validateChipsetPackageRequiredFields(raw); err != nil {
		return err
	}
	var manifest chipsetManifestV1
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return fmt.Errorf("%w: %v", errChipsetManifestInvalid, err)
	}
	if strings.TrimSpace(manifest.Schema) == "" || len(manifest.Chipsets) != 1 || !chipsetPackageKeyPattern.MatchString(strings.TrimSpace(manifest.Chipsets[0].ChipsetKey)) {
		return fmt.Errorf("%w: package requires $schema, one chipset, and a stable lowercase chipset_key", errChipsetManifestInvalid)
	}
	for _, resource := range manifest.Chipsets[0].Resources {
		if err := validateChipsetLinkGovernance(resource); err != nil {
			return err
		}
	}
	for _, release := range manifest.Chipsets[0].SDKReleases {
		for _, endpoint := range release.Endpoints {
			if err := validateChipsetLinkGovernance(endpoint); err != nil {
				return err
			}
		}
	}
	_, _, err := parseChipsetManifest("00000000-0000-0000-0000-000000000000", raw)
	return err
}

func validateChipsetPackageRequiredFields(raw []byte) error {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		return fmt.Errorf("%w: %v", errChipsetManifestInvalid, err)
	}
	for _, field := range []string{"$schema", "manifest_version", "provider", "chipsets"} {
		if _, ok := document[field]; !ok {
			return fmt.Errorf("%w: package is missing %s", errChipsetManifestInvalid, field)
		}
	}
	var chipsets []map[string]json.RawMessage
	if err := json.Unmarshal(document["chipsets"], &chipsets); err != nil || len(chipsets) != 1 {
		return fmt.Errorf("%w: package must contain exactly one chipset", errChipsetManifestInvalid)
	}
	for _, field := range []string{"chipset_key", "vendor", "name", "resources", "sdk_releases"} {
		if _, ok := chipsets[0][field]; !ok {
			return fmt.Errorf("%w: chipset is missing %s", errChipsetManifestInvalid, field)
		}
	}
	var resources []map[string]json.RawMessage
	if err := json.Unmarshal(chipsets[0]["resources"], &resources); err != nil {
		return fmt.Errorf("%w: resources must be an array", errChipsetManifestInvalid)
	}
	if err := validateChipsetPackageLinkFields(resources); err != nil {
		return err
	}
	var releases []map[string]json.RawMessage
	if err := json.Unmarshal(chipsets[0]["sdk_releases"], &releases); err != nil {
		return fmt.Errorf("%w: sdk_releases must be an array", errChipsetManifestInvalid)
	}
	for _, release := range releases {
		for _, field := range []string{"name", "version", "recommended", "supported_models", "endpoints"} {
			if _, ok := release[field]; !ok {
				return fmt.Errorf("%w: SDK release is missing %s", errChipsetManifestInvalid, field)
			}
		}
		var endpoints []map[string]json.RawMessage
		if err := json.Unmarshal(release["endpoints"], &endpoints); err != nil {
			return fmt.Errorf("%w: endpoints must be an array", errChipsetManifestInvalid)
		}
		if err := validateChipsetPackageLinkFields(endpoints); err != nil {
			return err
		}
	}
	return nil
}

func validateChipsetPackageLinkFields(links []map[string]json.RawMessage) error {
	for _, link := range links {
		for _, field := range []string{"type", "title", "url", "source", "languages", "verified_at"} {
			if _, ok := link[field]; !ok {
				return fmt.Errorf("%w: resource is missing %s", errChipsetManifestInvalid, field)
			}
		}
	}
	return nil
}

func validateChipsetLinkGovernance(link model.ChipsetEndpoint) error {
	if link.Source != "official" && link.Source != "community" {
		return fmt.Errorf("%w: resource source must be official or community", errChipsetManifestInvalid)
	}
	if len(link.Languages) == 0 {
		return fmt.Errorf("%w: resource languages are required", errChipsetManifestInvalid)
	}
	seenLanguages := map[string]struct{}{}
	for _, language := range link.Languages {
		if !chipsetLanguagePattern.MatchString(language) {
			return fmt.Errorf("%w: invalid resource language", errChipsetManifestInvalid)
		}
		if _, exists := seenLanguages[language]; exists {
			return fmt.Errorf("%w: duplicate resource language", errChipsetManifestInvalid)
		}
		seenLanguages[language] = struct{}{}
	}
	if _, err := time.Parse(time.RFC3339, link.VerifiedAt); err != nil {
		return fmt.Errorf("%w: invalid resource verified_at", errChipsetManifestInvalid)
	}
	return nil
}

func parseChipsetManifest(providerID string, raw []byte) ([]model.DeveloperChipset, string, error) {
	if err := validateJSONDepth(raw, chipsetManifestMaxDepth); err != nil {
		return nil, "", err
	}
	var manifest chipsetManifestV1
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, "", fmt.Errorf("%w: %v", errChipsetManifestInvalid, err)
	}
	if manifest.ManifestVersion != "1" {
		return nil, "", errChipsetManifestVersionUnsupported
	}
	if blank(manifest.Provider.Name) || blank(manifest.Provider.UpdatedAt) || len(manifest.Chipsets) == 0 || len(manifest.Chipsets) > chipsetManifestMaxChipsets {
		return nil, "", errChipsetManifestInvalid
	}
	if _, err := time.Parse(time.RFC3339, manifest.Provider.UpdatedAt); err != nil {
		return nil, "", fmt.Errorf("%w: provider.updated_at", errChipsetManifestInvalid)
	}
	seenChipsets := map[string]struct{}{}
	result := make([]model.DeveloperChipset, 0, len(manifest.Chipsets))
	for _, source := range manifest.Chipsets {
		key := strings.TrimSpace(source.ChipsetKey)
		if key == "" || blank(source.Vendor) || blank(source.Name) || len(source.SDKReleases) > chipsetManifestMaxReleases {
			return nil, "", errChipsetManifestInvalid
		}
		if _, exists := seenChipsets[key]; exists {
			return nil, "", fmt.Errorf("%w: duplicate chipset_key", errChipsetManifestInvalid)
		}
		seenChipsets[key] = struct{}{}
		seenReleases := map[string]struct{}{}
		if len(source.Resources) > chipsetManifestMaxEndpoints {
			return nil, "", errChipsetManifestInvalid
		}
		if err := normalizeChipsetLinks(source.Resources); err != nil {
			return nil, "", err
		}
		recommended := 0
		for releaseIndex := range source.SDKReleases {
			release := &source.SDKReleases[releaseIndex]
			release.Name = strings.TrimSpace(release.Name)
			release.Version = strings.TrimSpace(release.Version)
			if release.Name == "" || release.Version == "" || len(release.Endpoints) > chipsetManifestMaxEndpoints || len(release.SupportedModels) > chipsetManifestMaxModels {
				return nil, "", errChipsetManifestInvalid
			}
			for modelIndex := range release.SupportedModels {
				release.SupportedModels[modelIndex] = strings.TrimSpace(release.SupportedModels[modelIndex])
				if release.SupportedModels[modelIndex] == "" {
					return nil, "", errChipsetManifestInvalid
				}
			}
			releaseKey := release.Name + "\x00" + release.Version
			if _, exists := seenReleases[releaseKey]; exists {
				return nil, "", fmt.Errorf("%w: duplicate SDK release", errChipsetManifestInvalid)
			}
			seenReleases[releaseKey] = struct{}{}
			if release.Recommended {
				recommended++
			}
			if err := normalizeChipsetLinks(release.Endpoints); err != nil {
				return nil, "", err
			}
		}
		if recommended > 1 {
			return nil, "", fmt.Errorf("%w: multiple recommended SDK releases", errChipsetManifestInvalid)
		}
		result = append(result, model.DeveloperChipset{
			ID: stableChipsetID(providerID, key), ChipsetKey: key,
			Vendor: strings.TrimSpace(source.Vendor), Name: strings.TrimSpace(source.Name),
			Family: strings.TrimSpace(source.Family), Description: strings.TrimSpace(source.Description),
			Resources:   source.Resources,
			SDKReleases: source.SDKReleases,
		})
	}
	return result, manifest.ManifestVersion, nil
}

func normalizeChipsetLinks(links []model.ChipsetEndpoint) error {
	seen := map[string]struct{}{}
	for index := range links {
		link := &links[index]
		link.Type = strings.TrimSpace(link.Type)
		link.Title = strings.TrimSpace(link.Title)
		link.URL = strings.TrimSpace(link.URL)
		link.Source = strings.TrimSpace(link.Source)
		link.VerifiedAt = strings.TrimSpace(link.VerifiedAt)
		link.Summary = strings.TrimSpace(link.Summary)
		u, err := url.Parse(link.URL)
		if link.Type == "" || link.Title == "" || err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
			return fmt.Errorf("%w: invalid endpoint", errChipsetManifestInvalid)
		}
		if link.Source != "" && link.Source != "official" && link.Source != "community" {
			return fmt.Errorf("%w: invalid resource source", errChipsetManifestInvalid)
		}
		for languageIndex := range link.Languages {
			link.Languages[languageIndex] = strings.TrimSpace(link.Languages[languageIndex])
			if !chipsetLanguagePattern.MatchString(link.Languages[languageIndex]) {
				return fmt.Errorf("%w: invalid resource language", errChipsetManifestInvalid)
			}
		}
		if link.VerifiedAt != "" {
			if _, err := time.Parse(time.RFC3339, link.VerifiedAt); err != nil {
				return fmt.Errorf("%w: invalid resource verified_at", errChipsetManifestInvalid)
			}
		}
		key := link.Type + "\x00" + link.URL
		if _, exists := seen[key]; exists {
			return fmt.Errorf("%w: duplicate resource link", errChipsetManifestInvalid)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validateJSONDepth(raw []byte, max int) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	depth := 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%w: %v", errChipsetManifestInvalid, err)
		}
		if delim, ok := token.(json.Delim); ok {
			switch delim {
			case '{', '[':
				depth++
				if depth > max {
					return fmt.Errorf("%w: JSON nesting exceeds %d", errChipsetManifestInvalid, max)
				}
			case '}', ']':
				depth--
			}
		}
	}
}

func stableChipsetID(providerID, key string) string {
	sum := sha256.Sum256([]byte(providerID + "\x00" + key))
	sum[6] = (sum[6] & 0x0f) | 0x50
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

func publicIP(ip net.IP) bool {
	return ip != nil && !ip.IsUnspecified() && !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsMulticast()
}

func normalizeAllowedHosts(values []string) []string {
	seen := map[string]struct{}{}
	result := []string{}
	for _, value := range values {
		value = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
		base := strings.TrimPrefix(value, "*.")
		if value == "" || base == "" || net.ParseIP(base) != nil || strings.Contains(base, "*") {
			continue
		}
		if _, exists := seen[value]; !exists {
			seen[value] = struct{}{}
			result = append(result, value)
		}
	}
	return result
}

func blank(value string) bool { return strings.TrimSpace(value) == "" }

func sanitizedProviderError(err error) string {
	switch {
	case errors.Is(err, errChipsetProviderURLInvalid):
		return "provider URL is invalid"
	case errors.Is(err, errChipsetProviderHostNotAllowed):
		return "provider host is not allowed"
	case errors.Is(err, errChipsetProviderAddressNotPublic):
		return "provider address is not public"
	case errors.Is(err, errChipsetManifestVersionUnsupported):
		return "manifest version is unsupported"
	case errors.Is(err, errChipsetManifestInvalid):
		return "manifest validation failed"
	case errors.Is(err, errChipsetProviderSnapshotRequired):
		return "provider has no valid snapshot"
	default:
		return "provider fetch failed"
	}
}

func providerErrorCode(err error) string {
	switch {
	case errors.Is(err, errChipsetProviderURLInvalid):
		return "PROVIDER_URL_INVALID"
	case errors.Is(err, errChipsetProviderHostNotAllowed), errors.Is(err, errChipsetProviderAddressNotPublic):
		return "PROVIDER_HOST_NOT_ALLOWED"
	case errors.Is(err, errChipsetManifestVersionUnsupported):
		return "PROVIDER_MANIFEST_VERSION_UNSUPPORTED"
	case errors.Is(err, errChipsetManifestInvalid):
		return "PROVIDER_MANIFEST_INVALID"
	case errors.Is(err, errChipsetProviderSnapshotRequired):
		return "PROVIDER_SNAPSHOT_REQUIRED"
	default:
		return "PROVIDER_FETCH_FAILED"
	}
}
