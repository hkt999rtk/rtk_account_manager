package store

import (
	"errors"
	"testing"

	"rtk_account_manager/internal/model"
)

func TestMergeUserDeviceMetadataPreservesServiceFacts(t *testing.T) {
	current := map[string]any{
		"location":                                     "old",
		model.DeviceMetadataVideoCloudDevid:            "video-1",
		model.DeviceMetadataVideoCloudActivationStatus: "activated",
		model.DeviceMetadataServiceOptions:             []string{"mqtt"},
	}
	merged, err := mergeUserDeviceMetadata(map[string]any{"location": "new"}, current)
	if err != nil {
		t.Fatal(err)
	}
	if merged["location"] != "new" || merged[model.DeviceMetadataVideoCloudDevid] != "video-1" ||
		merged[model.DeviceMetadataVideoCloudActivationStatus] != "activated" || merged[model.DeviceMetadataServiceOptions] == nil {
		t.Fatalf("service facts not preserved: %+v", merged)
	}
	if current["location"] != "old" {
		t.Fatal("input metadata was mutated")
	}
}

func TestMergeUserDeviceMetadataRejectsReservedKeys(t *testing.T) {
	for _, key := range []string{model.DeviceMetadataVideoCloudDevid, model.DeviceMetadataVideoCloudActivationStatus, model.DeviceMetadataServiceOptions} {
		if _, err := mergeUserDeviceMetadata(map[string]any{key: "spoof"}, nil); !errors.Is(err, ErrReservedDeviceMetadata) {
			t.Fatalf("%s: expected reserved metadata error, got %v", key, err)
		}
	}
}
