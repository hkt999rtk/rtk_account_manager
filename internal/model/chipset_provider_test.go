package model

import (
	"encoding/json"
	"testing"
)

func TestChipsetEndpointUnmarshalMetadataAndErrors(t *testing.T) {
	var endpoint ChipsetEndpoint
	if err := json.Unmarshal([]byte(`{"type":"github","title":"Code","url":"https://example.com","source":"official","languages":["en"],"verified_at":"2026-08-28T00:00:00Z","summary":"Source code","metadata":{"audience":"developer"},"future":true}`), &endpoint); err != nil {
		t.Fatal(err)
	}
	if endpoint.Source != "official" || len(endpoint.Languages) != 1 || endpoint.Languages[0] != "en" || endpoint.VerifiedAt != "2026-08-28T00:00:00Z" || endpoint.Summary != "Source code" {
		t.Fatalf("governance fields = %#v", endpoint)
	}
	if endpoint.Metadata["audience"] != "developer" || endpoint.Metadata["future"] != true {
		t.Fatalf("metadata = %#v", endpoint.Metadata)
	}
	for _, raw := range []string{`[`, `{"type":1}`, `{"languages":"en"}`, `{"metadata":"invalid"}`} {
		if err := json.Unmarshal([]byte(raw), &endpoint); err == nil {
			t.Fatalf("expected error for %s", raw)
		}
	}
}
