package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"rtk_account_manager/internal/model"
)

func TestDeviceTransferFenceReconciliationCancelsOnlyUncommittedGeneration(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	source, err := env.store.Register(ctx, RegisterInput{
		Email: "fence-reconcile-source@example.com", PasswordHash: "hash", OrganizationName: "Fence Source",
	})
	if err != nil {
		t.Fatal(err)
	}
	target, err := env.store.Register(ctx, RegisterInput{
		Email: "fence-reconcile-target@example.com", PasswordHash: "hash", OrganizationName: "Fence Target",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin=true WHERE id=$1`, target.User.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	token, err := env.store.CreateDeviceClaimToken(ctx, DeviceClaimTokenCreateInput{
		OrganizationID: &source.Organization.ID, TokenHash: "fence-reconcile-hash",
		Category: model.DeviceCategoryIPCamera, VideoCloudDevid: "fence-reconcile-video",
		ActivityID: "fence-reconcile-activity", ClipPublicKey: "fence-reconcile-key",
		ExpiresAt: now.Add(time.Hour), Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := env.store.ResolveDeviceClaimToken(ctx, DeviceClaimResolveInput{
		TokenHash: "fence-reconcile-hash", OrganizationID: source.Organization.ID,
		RequestedBy: source.User.ID, DeviceName: "Reconcile Camera", Now: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	reservationID := claimTransferReservationID(resolved.Claim, target.Organization.ID)
	fence := DeviceTransferFence{
		DeviceID: token.VideoCloudDevid, ReservationID: reservationID,
		OrganizationID: target.Organization.ID, AccountDeviceID: resolved.Device.ID,
	}
	read := func(context.Context, string) (DeviceTransferFence, error) {
		if fence.ReservationID == "" {
			return DeviceTransferFence{}, ErrNotFound
		}
		return fence, nil
	}
	var released bool
	release := func(ctx context.Context, videoID, gotID string) error {
		if videoID != token.VideoCloudDevid || gotID != reservationID {
			t.Errorf("wrong release binding: %s %s", videoID, gotID)
		}
		var lockedID string
		err := env.db.QueryRow(ctx, `SELECT id::text FROM devices WHERE id=$1 FOR UPDATE NOWAIT`, resolved.Device.ID).Scan(&lockedID)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
			t.Errorf("account device lock was not held through remote release: %v", err)
		}
		released = true
		fence = DeviceTransferFence{}
		return nil
	}
	inspect := func() DeviceTransferFenceReconcileResult {
		t.Helper()
		result, err := env.store.ReconcileDeviceTransferFence(ctx, DeviceTransferFenceReconcileInput{
			ClaimID: resolved.Claim.ID, ReadFence: read,
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if result := inspect(); result.Status != "cancelable" || result.ReservationID != reservationID {
		t.Fatalf("stale fence inspection = %+v", result)
	}
	base := DeviceTransferFenceReconcileInput{
		ClaimID: resolved.Claim.ID, Cancel: true, ActorUserID: target.User.ID,
		Reason: "account transaction did not commit", Evidence: map[string]any{"ticket": "SUP-RECONCILE"},
		ReadFence: read, ReleaseFence: release,
	}
	wrong := base
	wrong.ExpectedReservationID = "wrong"
	if _, err := env.store.ReconcileDeviceTransferFence(ctx, wrong); !errors.Is(err, ErrConflict) || released {
		t.Fatalf("wrong generation cancelled fence: %v released=%t", err, released)
	}
	invalidAudit := base
	invalidAudit.ExpectedReservationID = reservationID
	invalidAudit.Evidence = map[string]any{"invalid": make(chan int)}
	if _, err := env.store.ReconcileDeviceTransferFence(ctx, invalidAudit); err == nil || released {
		t.Fatalf("invalid audit evidence cancelled fence: %v released=%t", err, released)
	}
	base.ExpectedReservationID = reservationID
	result, err := env.store.ReconcileDeviceTransferFence(ctx, base)
	if err != nil || result.Status != "cancelled" || !released {
		t.Fatalf("safe cancellation = %+v, %v released=%t", result, err, released)
	}
	if result := inspect(); result.Status != "absent" {
		t.Fatalf("cancelled fence inspection = %+v", result)
	}
	events, err := env.store.ListAuditEvents(ctx, AuditEventListFilter{EventType: "device_claim_transfer_fence_cancelled", Limit: 10})
	if err != nil || events.Page.Total != 1 || events.Events[0].Payload["reservation_id"] != reservationID {
		t.Fatalf("cancellation audit = %+v, %v", events, err)
	}
	fence = DeviceTransferFence{DeviceID: token.VideoCloudDevid, ReservationID: reservationID,
		OrganizationID: target.Organization.ID, AccountDeviceID: resolved.Device.ID}
	if _, err := env.db.Exec(ctx, `UPDATE device_claims SET updated_at=updated_at+interval '1 microsecond' WHERE id=$1`, resolved.Claim.ID); err != nil {
		t.Fatal(err)
	}
	if result := inspect(); result.Status != "manual_review" {
		t.Fatalf("changed claim generation inspection = %+v", result)
	}
	if _, err := env.store.ReconcileDeviceTransferFence(ctx, base); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed claim generation cancellation error = %v", err)
	}
	var currentUpdatedAt time.Time
	if err := env.db.QueryRow(ctx, `SELECT updated_at FROM device_claims WHERE id=$1`, resolved.Claim.ID).Scan(&currentUpdatedAt); err != nil {
		t.Fatal(err)
	}
	currentClaim := resolved.Claim
	currentClaim.UpdatedAt = currentUpdatedAt
	nextReservationID := claimTransferReservationID(currentClaim, target.Organization.ID)
	fence.ReservationID = nextReservationID
	if _, err := env.store.TransferDeviceClaim(ctx, DeviceClaimTransferInput{
		ClaimID: resolved.Claim.ID, TargetOrganizationID: target.Organization.ID,
		ActorUserID: target.User.ID, Reason: "verified transfer", Evidence: map[string]any{"ticket": "SUP-RECONCILE"},
		ReserveNoVideoCloudLifecycle: func(_ context.Context, _, gotID, _, _ string) error {
			if gotID != nextReservationID {
				t.Errorf("retry generation = %s want %s", gotID, nextReservationID)
			}
			return nil
		},
		Now: now.Add(2 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if result := inspect(); result.Status != "committed" {
		t.Fatalf("committed fence inspection = %+v", result)
	}
	if _, err := env.store.ReconcileDeviceTransferFence(ctx, base); !errors.Is(err, ErrConflict) {
		t.Fatalf("committed generation cancellation error = %v", err)
	}
}
