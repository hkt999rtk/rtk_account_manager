package store

import "testing"

func TestLoggerServiceManifestAdvertisesRetentionChoices(t *testing.T) {
	manifest := PlatformServiceRegistration{
		RequestID: "request-logger", ServiceID: "logger", InstanceID: "logger-service-0",
		ManifestVersion: "1", ProtocolVersion: "1", EndpointRef: "logger",
		Options: []PlatformServiceOption{{Code: "device_logging", DisplayName: "Device logging", Requires: []string{"mqtt"}, LogRetentionDays: []int{7, 30, 90}}},
	}
	if err := ValidatePlatformServiceRegistration(manifest); err != nil {
		t.Fatalf("valid Logger manifest: %v", err)
	}
	manifest.Options[0].LogRetentionDays = []int{7, 30}
	if err := ValidatePlatformServiceRegistration(manifest); err == nil {
		t.Fatal("incomplete retention choices accepted")
	}
	manifest.ServiceID = "other"
	manifest.Options[0].LogRetentionDays = []int{7, 30, 90}
	if err := ValidatePlatformServiceRegistration(manifest); err == nil {
		t.Fatal("another service claimed device_logging")
	}
}
