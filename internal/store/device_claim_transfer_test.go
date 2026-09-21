package store

import (
	"encoding/hex"
	"testing"
	"time"

	"rtk_account_manager/internal/model"
)

func TestClaimTransferReservationIDIsStablePerClaimVersion(t *testing.T) {
	claim := model.DeviceClaim{ID: "claim-1", OrganizationID: "source-org", UpdatedAt: time.Date(2026, 9, 21, 0, 0, 0, 123000, time.UTC)}
	first := claimTransferReservationID(claim, "target-org")
	if len(first) != 64 || claimTransferReservationID(claim, "target-org") != first {
		t.Fatalf("reservation ID is not stable: %q", first)
	}
	if _, err := hex.DecodeString(first); err != nil {
		t.Fatalf("reservation ID is not SHA-256 hex: %v", err)
	}
	claim.UpdatedAt = claim.UpdatedAt.Add(time.Microsecond)
	if claimTransferReservationID(claim, "target-org") == first || claimTransferReservationID(claim, "other-org") == first {
		t.Fatal("new claim version or target reused an old transfer generation")
	}
}

func TestClaimLifecycleBound(t *testing.T) {
	for _, tt := range []struct {
		name     string
		metadata map[string]any
		want     bool
	}{
		{name: "claim only", metadata: map[string]any{model.DeviceMetadataVideoCloudDevid: "video-1"}},
		{name: "pending", metadata: map[string]any{model.DeviceMetadataVideoCloudActivationStatus: "pending"}, want: true},
		{name: "activated", metadata: map[string]any{model.DeviceMetadataVideoCloudActivationStatus: "activated"}, want: true},
		{name: "failed may have partial side effects", metadata: map[string]any{model.DeviceMetadataVideoCloudActivationStatus: "failed"}, want: true},
		{name: "deactivated still retains cloud identity", metadata: map[string]any{model.DeviceMetadataVideoCloudActivationStatus: "deactivated"}, want: true},
		{name: "null", metadata: map[string]any{model.DeviceMetadataVideoCloudActivationStatus: nil}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := claimLifecycleBound(tt.metadata); got != tt.want {
				t.Fatalf("claimLifecycleBound() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestHasVideoCloudIdentity(t *testing.T) {
	if hasVideoCloudIdentity(nil) || hasVideoCloudIdentity(map[string]any{model.DeviceMetadataVideoCloudDevid: "  "}) {
		t.Fatal("blank or missing video identity must not be cloud-managed presence")
	}
	if !hasVideoCloudIdentity(map[string]any{model.DeviceMetadataVideoCloudDevid: "video-1"}) {
		t.Fatal("claim identity must fence human online status")
	}
}
