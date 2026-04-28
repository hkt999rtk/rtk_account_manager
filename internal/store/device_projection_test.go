package store

import (
	"reflect"
	"testing"
	"time"

	"rtk_account_manager/internal/channel"
	"rtk_account_manager/internal/model"
)

func TestMetadataChangedProjectionFiltersNonVideoCloudKeys(t *testing.T) {
	projection := MetadataChangedProjection(channel.DeviceMetadataChangedPayload{
		Metadata: map[string]any{
			"location":                               "lab",
			model.DeviceMetadataVideoCloudDevid:      "devid-1",
			model.DeviceMetadataVideoCloudActivityID: "activity-1",
		},
	})

	want := map[string]any{
		model.DeviceMetadataVideoCloudDevid:      "devid-1",
		model.DeviceMetadataVideoCloudActivityID: "activity-1",
	}
	if !reflect.DeepEqual(projection.Metadata, want) {
		t.Fatalf("unexpected filtered metadata: %+v", projection.Metadata)
	}
}

func TestApplyProjectionMetadataPreservesExistingFieldsAndClearsNil(t *testing.T) {
	merged := applyProjectionMetadata(
		map[string]any{
			"location":                              "lab",
			model.DeviceMetadataVideoCloudDevid:     "old-device",
			model.DeviceMetadataVideoCloudLastError: "stale",
		},
		map[string]any{
			model.DeviceMetadataVideoCloudDevid:            "new-device",
			model.DeviceMetadataVideoCloudActivationStatus: model.VideoCloudActivationStatusActivated,
			model.DeviceMetadataVideoCloudLastError:        nil,
			"ignored":                                      "value",
		},
	)

	want := map[string]any{
		"location":                                     "lab",
		model.DeviceMetadataVideoCloudDevid:            "new-device",
		model.DeviceMetadataVideoCloudActivationStatus: model.VideoCloudActivationStatusActivated,
	}
	if !reflect.DeepEqual(merged, want) {
		t.Fatalf("unexpected merged metadata: %+v", merged)
	}
}

func TestOnlineChangedProjectionSetsStatusAndLastSeenAt(t *testing.T) {
	lastSeenAt := time.Date(2026, 4, 28, 12, 0, 0, 0, time.FixedZone("CST", 8*60*60))

	projection := OnlineChangedProjection(channel.DeviceOnlineChangedPayload{
		VideoCloudDevid: "video-device",
		Status:          channel.OnlineStatusOnline,
		LastSeenAt:      lastSeenAt,
	})

	if projection.Status == nil || *projection.Status != model.DeviceStatusOnline {
		t.Fatalf("expected online status, got %+v", projection.Status)
	}
	if projection.LastSeenAt == nil || !projection.LastSeenAt.Equal(lastSeenAt.UTC()) {
		t.Fatalf("expected UTC last_seen_at, got %+v", projection.LastSeenAt)
	}
	if got := projection.Metadata[model.DeviceMetadataVideoCloudDevid]; got != "video-device" {
		t.Fatalf("unexpected devid metadata: %+v", got)
	}
}
