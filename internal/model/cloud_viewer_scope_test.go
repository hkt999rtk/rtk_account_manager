package model

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestCloudViewerScopeJSON(t *testing.T) {
	var wrongCase CloudViewerScope
	if err := json.Unmarshal([]byte(`{"Kind":"all_products"}`), &wrongCase); err == nil {
		t.Fatal("accepted noncanonical field case")
	}
	for _, raw := range []string{`null`, `{}`, `{"kind":"all_products","product_ids":null}`, `{"kind":"all_products","product_ids":[]}`, `{"kind":"all_products","extra":true}`, `{"kind":"selected_products","product_ids":[]}`, `{"kind":"selected_products","product_ids":["nope"]}`, `{"kind":"selected_products","product_ids":["aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","AAAAAAAA-AAAA-AAAA-AAAA-AAAAAAAAAAAA"]}`} {
		var scope CloudViewerScope
		if err := json.Unmarshal([]byte(raw), &scope); err == nil {
			t.Errorf("accepted invalid scope: %s", raw)
		}
	}
	var scope CloudViewerScope
	if err := json.Unmarshal([]byte(`{"kind":"selected_products","product_ids":["bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb","AAAAAAAA-AAAA-AAAA-AAAA-AAAAAAAAAAAA"]}`), &scope); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(scope.ProductIDs, []string{"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"}) {
		t.Fatal(scope)
	}
	if err := json.Unmarshal([]byte(`{"kind":"all_products"}`), &scope); err != nil {
		t.Fatal(err)
	}
	if scope.ProductIDs != nil {
		t.Fatal("decoding reused selected scope retained products")
	}
}
