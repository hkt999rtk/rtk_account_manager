package inbox

import (
	"testing"
	"time"

	"rtk_account_manager/internal/channel"
	"rtk_account_manager/internal/model"
)

func TestEntitlementSnapshotResultsCompleteOperationWithoutMetadataProjection(t *testing.T) {
	now := time.Date(2026, 9, 19, 1, 2, 3, 0, time.UTC)
	tests := []struct {
		name    string
		payload channel.Payload
		status  model.DeviceOperationStatus
	}{
		{name: "success", payload: &channel.DeviceEntitlementSnapshotSucceededPayload{
			OrgID: "11111111-1111-4111-8111-111111111111", AccountDeviceID: "22222222-2222-4222-8222-222222222222",
			VideoCloudDevid: "video-1", PlatformEntitlementRevision: 3, AppliedAt: now,
		}, status: model.DeviceOperationStatusSucceeded},
		{name: "failure", payload: &channel.DeviceEntitlementSnapshotFailedPayload{
			OrgID: "11111111-1111-4111-8111-111111111111", AccountDeviceID: "22222222-2222-4222-8222-222222222222",
			VideoCloudDevid: "video-1", PlatformEntitlementRevision: 3,
			ErrorCode: "entitlement_update_failed", ErrorMessage: "conflict", FailedAt: now,
		}, status: model.DeviceOperationStatusFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transition, err := buildTransitionForPayload(channel.Envelope{}, tt.payload)
			if err != nil || transition.OperationStatus == nil || *transition.OperationStatus != tt.status ||
				transition.Projection != nil || transition.OperationResult["platform_entitlement_revision"] != int64(3) {
				t.Fatalf("transition=%+v err=%v", transition, err)
			}
		})
	}
}
