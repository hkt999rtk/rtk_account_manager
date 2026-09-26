package store

import (
	"errors"
	"testing"
)

func TestProductServiceGrantValidationPreservesHistoricalSubset(t *testing.T) {
	bindings := []PlatformServiceCatalogOption{{PlatformServiceOption: PlatformServiceOption{Code: "iot_shadow", Requires: []string{"mqtt"}}}}
	for _, test := range []struct {
		name       string
		effective  []string
		product    []string
		bindings   []PlatformServiceCatalogOption
		historical bool
		current    bool
	}{
		{name: "current MQTT subset", effective: []string{"mqtt"}, product: []string{"mqtt", "iot_shadow"}, bindings: bindings, historical: true, current: true},
		{name: "current dependent option", effective: []string{"mqtt", "iot_shadow"}, product: []string{"mqtt", "iot_shadow"}, bindings: bindings, historical: true, current: true},
		{name: "legacy option without MQTT", effective: []string{"video_streaming"}, product: []string{"video_streaming"}, historical: true},
		{name: "empty entitlement", product: []string{"mqtt"}},
		{name: "duplicate entitlement", effective: []string{"mqtt", "mqtt"}, product: []string{"mqtt"}},
		{name: "option absent from Product grant", effective: []string{"mqtt", "ota"}, product: []string{"mqtt"}},
		{name: "dependent option missing MQTT", effective: []string{"iot_shadow"}, product: []string{"mqtt", "iot_shadow"}, bindings: bindings},
	} {
		t.Run(test.name, func(t *testing.T) {
			historical := validateHistoricalDeviceServices(test.effective, test.product, test.bindings)
			if (historical == nil) != test.historical || historical != nil && !errors.Is(historical, ErrClaimUnsupportedService) {
				t.Fatalf("historical validation = %v, want allowed=%v", historical, test.historical)
			}
			current := validateEffectiveDeviceServices(test.effective, test.product, test.bindings)
			if (current == nil) != test.current || current != nil && !errors.Is(current, ErrClaimUnsupportedService) {
				t.Fatalf("current validation = %v, want allowed=%v", current, test.current)
			}
		})
	}
}
