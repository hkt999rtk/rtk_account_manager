package store

import (
	"errors"
	"strings"
	"testing"
)

func TestPlatformServiceManifestValidation(t *testing.T) {
	base := PlatformServiceRegistration{
		RequestID: "request-1", ServiceID: "shadow", InstanceID: "shadow-1",
		ManifestVersion: "1", ProtocolVersion: "1", EndpointRef: "shadow-api",
		Options: []PlatformServiceOption{{Code: "iot_shadow", DisplayName: "IoT Shadow", Requires: []string{"mqtt"}}},
	}
	if err := ValidatePlatformServiceRegistration(base); err != nil {
		t.Fatal(err)
	}
	ota := base
	ota.ServiceID = "ota"
	ota.Options = []PlatformServiceOption{{Code: "ota", DisplayName: "Firmware OTA", Requires: []string{"mqtt"}}}
	if err := ValidatePlatformServiceRegistration(ota); err != nil {
		t.Fatalf("OTA service registration rejected: %v", err)
	}
	ota.Options = []PlatformServiceOption{{Code: "ota", DisplayName: "Firmware OTA"}}
	if err := ValidatePlatformServiceRegistration(ota); !errors.Is(err, ErrServiceRegistrationInvalid) {
		t.Fatalf("OTA without MQTT dependency accepted: %v", err)
	}
	tests := []struct {
		name   string
		change func(*PlatformServiceRegistration)
		want   error
	}{
		{"foreign-mqtt", func(r *PlatformServiceRegistration) { r.Options[0].Code = "mqtt"; r.Options[0].Requires = nil }, ErrServiceRegistrationDenied},
		{"foreign-ota", func(r *PlatformServiceRegistration) {
			r.Options[0] = PlatformServiceOption{Code: "ota", DisplayName: "Firmware OTA", Requires: []string{"mqtt"}}
		}, ErrServiceRegistrationDenied},
		{"unversioned", func(r *PlatformServiceRegistration) { r.ProtocolVersion = "2" }, ErrServiceRegistrationInvalid},
		{"invalid-code", func(r *PlatformServiceRegistration) { r.Options[0].Code = "platform:admin" }, ErrServiceRegistrationInvalid},
		{"duplicate", func(r *PlatformServiceRegistration) { r.Options = append(r.Options, r.Options[0]) }, ErrServiceRegistrationInvalid},
		{"self-dependency", func(r *PlatformServiceRegistration) { r.Options[0].Requires = []string{"iot_shadow"} }, ErrServiceRegistrationInvalid},
		{"missing-mqtt-foundation", func(r *PlatformServiceRegistration) { r.Options[0].Requires = nil }, ErrServiceRegistrationInvalid},
		{"mqtt-with-dependency", func(r *PlatformServiceRegistration) {
			r.ServiceID = "mqtt"
			r.Options = []PlatformServiceOption{{Code: "mqtt", DisplayName: "MQTT", Requires: []string{"iot_shadow"}}}
		}, ErrServiceRegistrationInvalid},
		{"mqtt-cannot-advertise-plugin", func(r *PlatformServiceRegistration) {
			r.ServiceID = "mqtt"
			r.Options = []PlatformServiceOption{{Code: "mqtt", DisplayName: "MQTT"}, {Code: "iot_shadow", DisplayName: "IoT Shadow"}}
		}, ErrServiceRegistrationInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := base
			r.Options = append([]PlatformServiceOption(nil), base.Options...)
			tt.change(&r)
			if err := ValidatePlatformServiceRegistration(r); !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
		})
	}
}

func TestPlatformServiceDigestIgnoresOptionOrderButBindsContents(t *testing.T) {
	base := PlatformServiceRegistration{
		ServiceID: "shadow", ManifestVersion: "1", ProtocolVersion: "1", EndpointRef: "shadow-api",
		Options: []PlatformServiceOption{
			{Code: "iot_shadow", DisplayName: "IoT Shadow", Requires: []string{"mqtt"}},
			{Code: "shadow_history", DisplayName: "History", Requires: []string{"iot_shadow", "mqtt"}},
		},
	}
	first, _, err := manifestDigest(base)
	if err != nil {
		t.Fatal(err)
	}
	base.Options[0], base.Options[1] = base.Options[1], base.Options[0]
	base.Options[0].Requires = []string{"mqtt", "iot_shadow"}
	second, _, err := manifestDigest(base)
	if err != nil || first != second {
		t.Fatalf("equivalent manifest changed digest: %v", err)
	}
	base.Options[0].DisplayName = "Different"
	third, _, err := manifestDigest(base)
	if err != nil || third == first {
		t.Fatalf("changed manifest reused digest: %v", err)
	}
}

func TestProductServiceSelectionRequiresRegisteredFoundationAndDependencies(t *testing.T) {
	catalog := PlatformServiceCatalog{CatalogRevision: 7, Options: []PlatformServiceCatalogOption{
		{PlatformServiceOption: PlatformServiceOption{Code: "mqtt"}, ServiceID: "mqtt", Selectable: true},
		{PlatformServiceOption: PlatformServiceOption{Code: "iot_shadow", Requires: []string{"mqtt"}}, ServiceID: "shadow", Selectable: true},
		{PlatformServiceOption: PlatformServiceOption{Code: "video_storage", Requires: []string{"mqtt"}}, ServiceID: "video-storage", Selectable: false},
		{PlatformServiceOption: PlatformServiceOption{Code: "guarded_plugin", Requires: []string{"mqtt", "guard_dependency"}}, ServiceID: "guarded", Selectable: true},
	}}
	for _, tc := range []struct {
		name  string
		codes []string
		valid bool
	}{
		{"mqtt-only", []string{"mqtt"}, true},
		{"shadow", []string{"iot_shadow", "mqtt"}, true},
		{"missing-foundation", []string{"iot_shadow"}, false},
		{"offline", []string{"mqtt", "video_storage"}, false},
		{"unregistered", []string{"mqtt", "new_plugin"}, false},
		{"duplicate", []string{"mqtt", "mqtt"}, false},
		{"missing-plugin-dependency", []string{"mqtt", "guarded_plugin"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bindings, err := validateProductServiceSelection(tc.codes, catalog)
			if (err == nil) != tc.valid {
				t.Fatalf("selection = %v, %v", bindings, err)
			}
			if tc.valid && len(bindings) != len(tc.codes) {
				t.Fatalf("bindings = %v", bindings)
			}
		})
	}
}

func TestProductServiceOptionCodeValidationRejectsAmbiguousGrants(t *testing.T) {
	tooMany := make([]string, 65)
	for index := range tooMany {
		tooMany[index] = "mqtt"
	}
	for _, options := range [][]string{
		tooMany,
		{strings.Repeat("x", 65)},
		{"MQTT"},
		{"mqtt", "mqtt"},
	} {
		if err := ValidateProductServiceOptionCodes(options); !errors.Is(err, ErrClaimUnsupportedService) {
			t.Fatalf("options %v = %v", options, err)
		}
	}
}
