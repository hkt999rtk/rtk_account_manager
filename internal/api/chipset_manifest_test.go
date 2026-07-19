package api

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"rtk_account_manager/internal/model"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestParseChipsetManifestV1(t *testing.T) {
	t.Parallel()
	raw := []byte(`{
  "manifest_version":"1",
  "provider":{"name":"Example Semiconductor","updated_at":"2026-07-19T00:00:00Z"},
  "chipsets":[{
    "chipset_key":"example-xyz123","vendor":"Example","name":"XYZ123","family":"XYZ",
    "sdk_releases":[{
      "name":"Example SDK","version":"2.4.0","recommended":true,
      "supported_models":["Model-A"],
      "endpoints":[
        {"type":"github","title":"GitHub","url":"https://github.com/example/sdk"},
        {"type":"future-resource","title":"Future","url":"https://example.com/future","audience":"developer"}
      ]
    }]
  }]
}`)
	chipsets, version, err := parseChipsetManifest("11111111-1111-1111-1111-111111111111", raw)
	if err != nil {
		t.Fatalf("parseChipsetManifest returned error: %v", err)
	}
	if version != "1" || len(chipsets) != 1 || len(chipsets[0].SDKReleases) != 1 {
		t.Fatalf("unexpected normalized manifest: version=%q chipsets=%#v", version, chipsets)
	}
	if chipsets[0].ID != stableChipsetID("11111111-1111-1111-1111-111111111111", "example-xyz123") {
		t.Fatalf("chipset id is not stable: %q", chipsets[0].ID)
	}
	if got := chipsets[0].SDKReleases[0].Endpoints[1].Type; got != "future-resource" {
		t.Fatalf("unknown endpoint type was not retained: %q", got)
	}
	if got := chipsets[0].SDKReleases[0].Endpoints[1].Metadata["audience"]; got != "developer" {
		t.Fatalf("unknown endpoint metadata was not retained: %#v", got)
	}
}

func TestParseChipsetManifestRejectsInvalidSnapshots(t *testing.T) {
	t.Parallel()
	base := `{"manifest_version":"1","provider":{"name":"P","updated_at":"2026-07-19T00:00:00Z"},"chipsets":[{"chipset_key":"c","vendor":"V","name":"C","sdk_releases":[%s]}]}`
	tests := map[string]string{
		"two recommended releases": `{"name":"SDK","version":"1","recommended":true,"supported_models":[],"endpoints":[]},{"name":"SDK","version":"2","recommended":true,"supported_models":[],"endpoints":[]}`,
		"non HTTPS endpoint":       `{"name":"SDK","version":"1","recommended":true,"supported_models":[],"endpoints":[{"type":"sdk","title":"SDK","url":"http://example.com/sdk"}]}`,
		"duplicate release":        `{"name":"SDK","version":"1","recommended":false,"supported_models":[],"endpoints":[]},{"name":"SDK","version":"1","recommended":false,"supported_models":[],"endpoints":[]}`,
	}
	for name, releases := range tests {
		t.Run(name, func(t *testing.T) {
			_, _, err := parseChipsetManifest("provider", []byte(strings.Replace(base, "%s", releases, 1)))
			if !errors.Is(err, errChipsetManifestInvalid) {
				t.Fatalf("error = %v, want manifest invalid", err)
			}
		})
	}
}

func TestParseChipsetManifestRejectsUnsupportedVersionAndExcessiveDepth(t *testing.T) {
	t.Parallel()
	unsupported := []byte(`{"manifest_version":"2","provider":{"name":"P","updated_at":"2026-07-19T00:00:00Z"},"chipsets":[]}`)
	if _, _, err := parseChipsetManifest("provider", unsupported); !errors.Is(err, errChipsetManifestVersionUnsupported) {
		t.Fatalf("unsupported version error = %v", err)
	}
	deep := []byte(strings.Repeat("[", chipsetManifestMaxDepth+1) + strings.Repeat("]", chipsetManifestMaxDepth+1))
	if _, _, err := parseChipsetManifest("provider", deep); !errors.Is(err, errChipsetManifestInvalid) {
		t.Fatalf("excessive depth error = %v", err)
	}
}

func TestChipsetProviderURLPolicy(t *testing.T) {
	t.Parallel()
	resolver := func(_ context.Context, host string) ([]net.IP, error) {
		switch host {
		case "provider.example.com":
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		case "internal.provider.example.com":
			return []net.IP{net.ParseIP("10.0.0.8")}, nil
		case "localhost.provider.example.com":
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		default:
			return nil, errors.New("unknown host")
		}
	}
	fetcher := NewChipsetManifestFetcher(ChipsetManifestFetcherConfig{AllowedHosts: []string{"provider.example.com", "*.provider.example.com"}, Resolver: resolver})
	exactOnly := NewChipsetManifestFetcher(ChipsetManifestFetcherConfig{AllowedHosts: []string{"provider.example.com"}, Resolver: resolver})
	if err := exactOnly.ValidateURL("https://internal.provider.example.com/manifest.json"); !errors.Is(err, errChipsetProviderHostNotAllowed) {
		t.Fatalf("exact allowlist unexpectedly accepted subdomain: %v", err)
	}
	if err := fetcher.ValidateURL("https://provider.example.com/manifest.json"); err != nil {
		t.Fatalf("allowed URL rejected: %v", err)
	}
	if err := fetcher.ValidateURL("http://provider.example.com/manifest.json"); !errors.Is(err, errChipsetProviderURLInvalid) {
		t.Fatalf("HTTP URL error = %v", err)
	}
	if err := fetcher.ValidateURL("https://provider.example.com:8443/manifest.json"); !errors.Is(err, errChipsetProviderURLInvalid) {
		t.Fatalf("non-default port error = %v", err)
	}
	if err := fetcher.ValidateURL("https://evil.example.net/manifest.json"); !errors.Is(err, errChipsetProviderHostNotAllowed) {
		t.Fatalf("non-allowlisted URL error = %v", err)
	}
	if err := fetcher.ValidateURL("https://internal.provider.example.com/manifest.json"); !errors.Is(err, errChipsetProviderAddressNotPublic) {
		t.Fatalf("private address error = %v", err)
	}
	if err := fetcher.ValidateURL("https://localhost.provider.example.com/manifest.json"); !errors.Is(err, errChipsetProviderAddressNotPublic) {
		t.Fatalf("loopback address error = %v", err)
	}
}

func TestChipsetProviderFetchRejectsDNSRebinding(t *testing.T) {
	var resolutions int
	resolver := func(_ context.Context, host string) ([]net.IP, error) {
		resolutions++
		if host != "provider.example.com" {
			return nil, errors.New("unknown host")
		}
		if resolutions == 1 {
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		}
		return []net.IP{net.ParseIP("169.254.169.254")}, nil
	}
	fetcher := NewChipsetManifestFetcher(ChipsetManifestFetcherConfig{AllowedHosts: []string{"provider.example.com"}, Resolver: resolver})
	_, err := fetcher.Fetch(t.Context(), model.ChipsetProvider{ManifestURL: "https://provider.example.com/manifest.json"})
	if !errors.Is(err, errChipsetProviderAddressNotPublic) {
		t.Fatalf("DNS rebinding error = %v", err)
	}
	if resolutions < 2 {
		t.Fatalf("resolver calls = %d, want validation and connect-time resolution", resolutions)
	}
}

func TestChipsetProviderRedirectPolicy(t *testing.T) {
	resolver := func(_ context.Context, host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	fetcher := NewChipsetManifestFetcher(ChipsetManifestFetcherConfig{AllowedHosts: []string{"provider.example.com"}, Resolver: resolver}).(*httpChipsetManifestFetcher)
	request, _ := http.NewRequest(http.MethodGet, "https://evil.example.net/manifest.json", nil)
	if err := fetcher.client.CheckRedirect(request, nil); !errors.Is(err, errChipsetProviderHostNotAllowed) {
		t.Fatalf("cross-domain redirect error = %v", err)
	}
	request, _ = http.NewRequest(http.MethodGet, "https://provider.example.com/next.json", nil)
	via := make([]*http.Request, 5)
	if err := fetcher.client.CheckRedirect(request, via); err == nil || !strings.Contains(err.Error(), "too many") {
		t.Fatalf("redirect limit error = %v", err)
	}
}

func TestChipsetProviderFetchTimeoutOversizeAndConditional304(t *testing.T) {
	resolver := func(_ context.Context, _ string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	provider := model.ChipsetProvider{ManifestURL: "https://provider.example.com/manifest.json", ETag: `"v1"`, LastModified: "Sun, 19 Jul 2026 00:00:00 GMT"}

	t.Run("timeout", func(t *testing.T) {
		client := &http.Client{Timeout: 5 * time.Millisecond, Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			<-req.Context().Done()
			return nil, req.Context().Err()
		})}
		fetcher := NewChipsetManifestFetcher(ChipsetManifestFetcherConfig{AllowedHosts: []string{"provider.example.com"}, Resolver: resolver, HTTPClient: client})
		if _, err := fetcher.Fetch(t.Context(), provider); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("timeout error = %v", err)
		}
	})

	t.Run("oversized response", func(t *testing.T) {
		client := &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", chipsetManifestMaxBytes+1))), Header: make(http.Header)}, nil
		})}
		fetcher := NewChipsetManifestFetcher(ChipsetManifestFetcherConfig{AllowedHosts: []string{"provider.example.com"}, Resolver: resolver, HTTPClient: client})
		if _, err := fetcher.Fetch(t.Context(), provider); !errors.Is(err, errChipsetManifestInvalid) {
			t.Fatalf("oversized response error = %v", err)
		}
	})

	t.Run("conditional 304", func(t *testing.T) {
		client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Header.Get("If-None-Match") != provider.ETag || req.Header.Get("If-Modified-Since") != provider.LastModified {
				t.Fatalf("conditional headers = %q, %q", req.Header.Get("If-None-Match"), req.Header.Get("If-Modified-Since"))
			}
			return &http.Response{StatusCode: http.StatusNotModified, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
		})}
		fetcher := NewChipsetManifestFetcher(ChipsetManifestFetcherConfig{AllowedHosts: []string{"provider.example.com"}, Resolver: resolver, HTTPClient: client})
		result, err := fetcher.Fetch(t.Context(), provider)
		if err != nil || !result.NotModified || result.ETag != provider.ETag || result.LastModified != provider.LastModified {
			t.Fatalf("304 result = %+v, %v", result, err)
		}
	})

	t.Run("upstream status", func(t *testing.T) {
		client := &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusBadGateway, Body: io.NopCloser(strings.NewReader("upstream secret")), Header: make(http.Header)}, nil
		})}
		fetcher := NewChipsetManifestFetcher(ChipsetManifestFetcherConfig{AllowedHosts: []string{"provider.example.com"}, Resolver: resolver, HTTPClient: client})
		if _, err := fetcher.Fetch(t.Context(), provider); err == nil || !strings.Contains(err.Error(), "HTTP 502") || strings.Contains(err.Error(), "secret") {
			t.Fatalf("upstream status error = %v", err)
		}
	})

	t.Run("response read failure", func(t *testing.T) {
		client := &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(errorReader{}), Header: make(http.Header)}, nil
		})}
		fetcher := NewChipsetManifestFetcher(ChipsetManifestFetcherConfig{AllowedHosts: []string{"provider.example.com"}, Resolver: resolver, HTTPClient: client})
		if _, err := fetcher.Fetch(t.Context(), provider); err == nil || err.Error() != "read failed" {
			t.Fatalf("response read error = %v", err)
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		client := &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"manifest_version":"1"`)), Header: make(http.Header)}, nil
		})}
		fetcher := NewChipsetManifestFetcher(ChipsetManifestFetcherConfig{AllowedHosts: []string{"provider.example.com"}, Resolver: resolver, HTTPClient: client})
		if _, err := fetcher.Fetch(t.Context(), provider); !errors.Is(err, errChipsetManifestInvalid) {
			t.Fatalf("malformed JSON error = %v", err)
		}
	})
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestChipsetProviderErrorsAreStableAndSanitized(t *testing.T) {
	tests := []struct {
		err     error
		message string
		code    string
	}{
		{errChipsetProviderURLInvalid, "provider URL is invalid", "PROVIDER_URL_INVALID"},
		{errChipsetProviderHostNotAllowed, "provider host is not allowed", "PROVIDER_HOST_NOT_ALLOWED"},
		{errChipsetProviderAddressNotPublic, "provider address is not public", "PROVIDER_HOST_NOT_ALLOWED"},
		{errChipsetManifestVersionUnsupported, "manifest version is unsupported", "PROVIDER_MANIFEST_VERSION_UNSUPPORTED"},
		{errChipsetManifestInvalid, "manifest validation failed", "PROVIDER_MANIFEST_INVALID"},
		{errChipsetProviderSnapshotRequired, "provider has no valid snapshot", "PROVIDER_SNAPSHOT_REQUIRED"},
		{errors.New("dial tcp 10.0.0.1: secret upstream failure"), "provider fetch failed", "PROVIDER_FETCH_FAILED"},
	}
	for _, tt := range tests {
		if got := sanitizedProviderError(tt.err); got != tt.message {
			t.Errorf("sanitizedProviderError(%v) = %q, want %q", tt.err, got, tt.message)
		}
		if got := providerErrorCode(tt.err); got != tt.code {
			t.Errorf("providerErrorCode(%v) = %q, want %q", tt.err, got, tt.code)
		}
	}
}

func TestChipsetProviderURLPolicyEdgeCasesAndDefaults(t *testing.T) {
	resolver := func(_ context.Context, host string) ([]net.IP, error) {
		switch host {
		case "empty.example.com":
			return nil, nil
		case "error.example.com":
			return nil, errors.New("dns unavailable")
		default:
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		}
	}
	fetcher := NewChipsetManifestFetcher(ChipsetManifestFetcherConfig{AllowedHosts: []string{"  EXAMPLE.COM. ", "*.example.com"}, Resolver: resolver})
	for name, raw := range map[string]string{
		"userinfo":     "https://user@example.com/manifest.json",
		"fragment":     "https://example.com/manifest.json#fragment",
		"missing host": "https:///manifest.json",
	} {
		t.Run(name, func(t *testing.T) {
			if err := fetcher.ValidateURL(raw); !errors.Is(err, errChipsetProviderURLInvalid) {
				t.Fatalf("ValidateURL(%q) = %v", raw, err)
			}
		})
	}
	if err := fetcher.ValidateURL("https://empty.example.com/manifest.json"); !errors.Is(err, errChipsetProviderURLInvalid) {
		t.Fatalf("empty DNS result = %v", err)
	}
	if err := fetcher.ValidateURL("https://error.example.com/manifest.json"); err == nil || !strings.Contains(err.Error(), "resolve provider host") {
		t.Fatalf("DNS error = %v", err)
	}
	defaultFetcher := NewChipsetManifestFetcher(ChipsetManifestFetcherConfig{AllowedHosts: []string{"localhost"}})
	if err := defaultFetcher.ValidateURL("https://localhost/manifest.json"); !errors.Is(err, errChipsetProviderAddressNotPublic) {
		t.Fatalf("default resolver localhost = %v", err)
	}
}

func TestParseChipsetManifestRejectsStructuralLimits(t *testing.T) {
	tests := map[string]string{
		"missing provider":  `{"manifest_version":"1","provider":{"name":"","updated_at":""},"chipsets":[]}`,
		"bad timestamp":     `{"manifest_version":"1","provider":{"name":"P","updated_at":"today"},"chipsets":[{"chipset_key":"c","vendor":"V","name":"C","sdk_releases":[]}]}`,
		"duplicate chipset": `{"manifest_version":"1","provider":{"name":"P","updated_at":"2026-07-19T00:00:00Z"},"chipsets":[{"chipset_key":"c","vendor":"V","name":"C","sdk_releases":[]},{"chipset_key":"c","vendor":"V","name":"C2","sdk_releases":[]}]}`,
		"blank model":       `{"manifest_version":"1","provider":{"name":"P","updated_at":"2026-07-19T00:00:00Z"},"chipsets":[{"chipset_key":"c","vendor":"V","name":"C","sdk_releases":[{"name":"SDK","version":"1","recommended":false,"supported_models":[" "],"endpoints":[]}]}]}`,
		"endpoint userinfo": `{"manifest_version":"1","provider":{"name":"P","updated_at":"2026-07-19T00:00:00Z"},"chipsets":[{"chipset_key":"c","vendor":"V","name":"C","sdk_releases":[{"name":"SDK","version":"1","recommended":false,"supported_models":[],"endpoints":[{"type":"support","title":"Support","url":"https://user@example.com"}]}]}]}`,
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, _, err := parseChipsetManifest("provider", []byte(raw)); !errors.Is(err, errChipsetManifestInvalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestDefaultChipsetFetcherDialGuards(t *testing.T) {
	resolver := func(_ context.Context, host string) ([]net.IP, error) {
		switch host {
		case "error.example.com":
			return nil, errors.New("dns failure")
		case "empty.example.com":
			return nil, nil
		case "private.example.com":
			return []net.IP{net.ParseIP("10.0.0.1")}, nil
		default:
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		}
	}
	fetcher := NewChipsetManifestFetcher(ChipsetManifestFetcherConfig{AllowedHosts: []string{"*.example.com", ""}, Resolver: resolver}).(*httpChipsetManifestFetcher)
	dial := fetcher.client.Transport.(*http.Transport).DialContext
	for name, address := range map[string]string{
		"invalid address": "missing-port",
		"disallowed host": "evil.test:443",
		"resolver error":  "error.example.com:443",
		"empty resolver":  "empty.example.com:443",
		"private address": "private.example.com:443",
	} {
		t.Run(name, func(t *testing.T) {
			conn, err := dial(t.Context(), "tcp", address)
			if conn != nil {
				_ = conn.Close()
			}
			if err == nil {
				t.Fatalf("DialContext(%q) unexpectedly succeeded", address)
			}
		})
	}
}

func TestChipsetProviderFetchSuccessfulManifest(t *testing.T) {
	manifest := `{"manifest_version":"1","provider":{"name":"Ameba","updated_at":"2026-07-19T00:00:00Z"},"chipsets":[{"chipset_key":"amebapro2","vendor":"Realtek","name":"AmebaPro2","sdk_releases":[]}]}`
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(manifest)), Header: http.Header{"Etag": {`"v1"`}, "Last-Modified": {"Sun, 19 Jul 2026 00:00:00 GMT"}}}, nil
	})}
	fetcher := NewChipsetManifestFetcher(ChipsetManifestFetcherConfig{
		AllowedHosts: []string{"provider.example.com"},
		Resolver:     func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("93.184.216.34")}, nil },
		HTTPClient:   client,
	})
	result, err := fetcher.Fetch(t.Context(), model.ChipsetProvider{ID: "provider-1", ManifestURL: "https://provider.example.com/manifest.json"})
	if err != nil {
		t.Fatal(err)
	}
	if result.ManifestVersion != "1" || len(result.Chipsets) != 1 || result.ETag != `"v1"` || result.ManifestSHA256 == "" {
		t.Fatalf("result = %+v", result)
	}
}
