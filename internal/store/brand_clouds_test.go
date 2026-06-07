package store

import "testing"

func TestNormalizeTenantSlug(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "lowercase", in: "acme", want: "acme"},
		{name: "trim and collapse", in: "  Acme Brand Cloud  ", want: "acme-brand-cloud"},
		{name: "punctuation", in: "Realtek+Video.Cloud", want: "realtek-video-cloud"},
		{name: "empty after normalization", in: " !!! ", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeTenantSlug(tt.in); got != tt.want {
				t.Fatalf("normalizeTenantSlug(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestGeneratedTenantSlugUsesNameAndEightCharSuffix(t *testing.T) {
	got := generatedTenantSlug("Acme Brand", "1234567890abcdef")
	if got != "acme-brand-12345678" {
		t.Fatalf("unexpected generated slug %q", got)
	}

	got = generatedTenantSlug(" !!! ", "abcdef")
	if got != "brand-abcdef" {
		t.Fatalf("unexpected fallback generated slug %q", got)
	}
}

func TestRandomTenantSlugSuffixIsHexEightChars(t *testing.T) {
	got, err := randomTenantSlugSuffix()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 8 {
		t.Fatalf("expected 8 hex chars, got %q", got)
	}
	for _, r := range got {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			t.Fatalf("expected lowercase hex suffix, got %q", got)
		}
	}
}
