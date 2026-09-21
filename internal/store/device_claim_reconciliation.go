package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"rtk_account_manager/internal/model"
)

// DeviceTransferFence is the authenticated Video Cloud reservation snapshot.
type DeviceTransferFence struct {
	DeviceID        string `json:"device_id"`
	ReservationID   string `json:"reservation_id"`
	OrganizationID  string `json:"org_id"`
	AccountDeviceID string `json:"account_device_id"`
}

type DeviceTransferFenceReconcileInput struct {
	ClaimID               string
	ExpectedReservationID string
	Cancel                bool
	ActorUserID           string
	Reason                string
	Evidence              map[string]any
	ReadFence             func(context.Context, string) (DeviceTransferFence, error)
	ReleaseFence          func(context.Context, string, string) error
}

type DeviceTransferFenceReconcileResult struct {
	Status                string `json:"status"`
	ClaimID               string `json:"claim_id"`
	DeviceID              string `json:"device_id"`
	VideoCloudDeviceID    string `json:"video_cloud_devid"`
	AccountOrganizationID string `json:"account_organization_id"`
	TargetOrganizationID  string `json:"target_organization_id,omitempty"`
	ReservationID         string `json:"reservation_id,omitempty"`
}

// ReconcileDeviceTransferFence holds the same account device/claim/token row
// locks as override while checking the remote generation. It never cancels a
// committed transfer or a generation that cannot be derived from the unchanged
// source claim. The remote cancellation itself rechecks lifecycle state under
// Video Cloud's per-device lock.
func (s *Store) ReconcileDeviceTransferFence(ctx context.Context, in DeviceTransferFenceReconcileInput) (DeviceTransferFenceReconcileResult, error) {
	if strings.TrimSpace(in.ClaimID) == "" || in.ReadFence == nil || (in.Cancel && in.ReleaseFence == nil) {
		return DeviceTransferFenceReconcileResult{}, ErrClaimLifecycleCheckUnavailable
	}
	if in.Cancel && (strings.TrimSpace(in.ExpectedReservationID) == "" || strings.TrimSpace(in.ActorUserID) == "" || strings.TrimSpace(in.Reason) == "" || len(in.Evidence) == 0) {
		return DeviceTransferFenceReconcileResult{}, ErrClaimEvidenceRequired
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return DeviceTransferFenceReconcileResult{}, err
	}
	defer tx.Rollback(ctx)
	if in.Cancel {
		if err := lockPlatformActorTx(ctx, tx, in.ActorUserID); err != nil {
			return DeviceTransferFenceReconcileResult{}, err
		}
	}
	observed, err := getClaimForOverrideTx(ctx, tx, in.ClaimID, "", false)
	if err != nil {
		return DeviceTransferFenceReconcileResult{}, err
	}
	device, err := getDeviceForUpdateTx(ctx, tx, observed.OrganizationID, observed.DeviceID)
	if errors.Is(err, ErrNotFound) {
		return DeviceTransferFenceReconcileResult{}, ErrConflict
	}
	if err != nil {
		return DeviceTransferFenceReconcileResult{}, err
	}
	claim, err := getClaimForOverrideTx(ctx, tx, in.ClaimID, "", true)
	if err != nil {
		return DeviceTransferFenceReconcileResult{}, err
	}
	if claim.ID != observed.ID || claim.DeviceID != device.ID || claim.OrganizationID != observed.OrganizationID || claim.TokenID != observed.TokenID {
		return DeviceTransferFenceReconcileResult{}, ErrConflict
	}
	token, err := getClaimTokenForUpdateTx(ctx, tx, claim.TokenID)
	if err != nil {
		return DeviceTransferFenceReconcileResult{}, err
	}
	if strings.TrimSpace(token.VideoCloudDevid) == "" {
		return DeviceTransferFenceReconcileResult{}, ErrConflict
	}
	result := DeviceTransferFenceReconcileResult{
		ClaimID: claim.ID, DeviceID: device.ID, VideoCloudDeviceID: token.VideoCloudDevid,
		AccountOrganizationID: claim.OrganizationID,
	}
	fence, err := in.ReadFence(ctx, token.VideoCloudDevid)
	if errors.Is(err, ErrNotFound) {
		if _, bound := device.Metadata[model.DeviceMetadataVideoCloudTransferReservationID]; bound {
			result.Status = "manual_review"
		} else {
			result.Status = "absent"
		}
		if in.Cancel {
			return result, ErrConflict
		}
		return result, nil
	}
	if err != nil {
		return result, fmt.Errorf("read Video Cloud transfer fence: %w", err)
	}
	result.TargetOrganizationID, result.ReservationID = fence.OrganizationID, fence.ReservationID
	if fence.DeviceID != token.VideoCloudDevid || fence.AccountDeviceID != device.ID ||
		strings.TrimSpace(fence.OrganizationID) == "" || strings.TrimSpace(fence.ReservationID) == "" {
		result.Status = "manual_review"
	} else if claim.OrganizationID == fence.OrganizationID && device.OrganizationID == fence.OrganizationID &&
		(claim.Status == "transferred" || claim.Status == "reclaimed") &&
		device.Metadata[model.DeviceMetadataVideoCloudTransferReservationID] == fence.ReservationID &&
		token.OrganizationID != nil && *token.OrganizationID == fence.OrganizationID {
		result.Status = "committed"
	} else if claim.Status == "resolved" && claim.OrganizationID == device.OrganizationID &&
		claim.OrganizationID != fence.OrganizationID && token.RevokedAt == nil &&
		(token.OrganizationID == nil || *token.OrganizationID == claim.OrganizationID) &&
		device.Metadata[model.DeviceMetadataVideoCloudDevid] == token.VideoCloudDevid &&
		device.Metadata[model.DeviceMetadataVideoCloudTransferReservationID] == nil &&
		!claimLifecycleBound(device.Metadata) &&
		claimTransferReservationID(claim, fence.OrganizationID) == fence.ReservationID {
		var operationRecorded bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM device_operations WHERE device_id=$1 AND operation_type='provision')`, device.ID).Scan(&operationRecorded); err != nil {
			return result, err
		}
		if operationRecorded {
			result.Status = "manual_review"
		} else {
			result.Status = "cancelable"
		}
	} else {
		result.Status = "manual_review"
	}
	if !in.Cancel {
		return result, nil
	}
	if result.Status != "cancelable" || in.ExpectedReservationID != fence.ReservationID {
		return result, ErrConflict
	}
	// Stage the audit before the remote DELETE, so malformed evidence or a
	// database write failure cannot remove the fence without an audit row.
	if err := createAuditEventTx(ctx, tx, AuditEventInput{
		EventType: "device_claim_transfer_fence_cancelled", ActorUserID: &in.ActorUserID,
		OrganizationID: &claim.OrganizationID, SubjectType: "device_claim", SubjectID: claim.ID,
		Payload: map[string]any{
			"account_device_id": device.ID, "video_cloud_devid": token.VideoCloudDevid,
			"target_organization_id": fence.OrganizationID, "reservation_id": fence.ReservationID,
			"reason": strings.TrimSpace(in.Reason), "evidence": in.Evidence,
		},
	}); err != nil {
		return result, err
	}
	if err := in.ReleaseFence(ctx, token.VideoCloudDevid, fence.ReservationID); err != nil {
		return result, fmt.Errorf("cancel Video Cloud transfer fence: %w", err)
	}
	// The remote DELETE may already be committed even if this audit commit
	// fails. In that case the caller must re-inspect; account ownership did
	// not change and the exact generation cannot be reused by another claim.
	if err := tx.Commit(ctx); err != nil {
		return result, err
	}
	result.Status = "cancelled"
	return result, nil
}
