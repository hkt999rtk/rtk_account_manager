package model

import (
	"encoding/json"
	"testing"
)

func TestChipsetEndpointUnmarshalMetadataAndErrors(t *testing.T) {
	var endpoint ChipsetEndpoint
	if err := json.Unmarshal([]byte(`{"type":"github","title":"Code","url":"https://example.com","metadata":{"audience":"developer"},"future":true}`), &endpoint); err != nil {
		t.Fatal(err)
	}
	if endpoint.Metadata["audience"] != "developer" || endpoint.Metadata["future"] != true {
		t.Fatalf("metadata = %#v", endpoint.Metadata)
	}
	for _, raw := range []string{`[`, `{"type":1}`, `{"metadata":"invalid"}`} {
		if err := json.Unmarshal([]byte(raw), &endpoint); err == nil {
			t.Fatalf("expected error for %s", raw)
		}
	}
}
