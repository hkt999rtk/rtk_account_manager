package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"rtk_account_manager/internal/channel"
	"rtk_account_manager/internal/model"
)

func TestDeviceEntitlementSnapshotsPinGrantAndCommitAuditWithOutbox(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "entitlement-snapshot")
	profileInput := authorizedProductInput(owner.User.ID, owner.BrandCloud.ID, "entitlement-product")
	profileInput.ServiceOptions = []string{"mqtt", "iot_shadow"}
	product, err := env.store.CreateDeviceItemProfile(ctx, profileInput)
	if err != nil {
		t.Fatal(err)
	}
	device, err := env.store.CreateDeviceAsUser(ctx, owner.User.ID, owner.BrandCloud.ID, DeviceInput{
		Name: "Entitled device", Category: product.Category, DeviceItemProfileID: &product.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	insertGrant := func(revision int64, options []string) {
		t.Helper()
		tx, err := env.db.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if err := insertProductServiceGrantTx(ctx, tx, product.ID, owner.BrandCloud.ID, options, nil, revision, &owner.User.ID); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	insertGrant(1, []string{"mqtt", "iot_shadow"})
	env.store.ConfigurePlatformServiceProductWrites("entitlement-test", true)
	requestedBy := owner.User.ID
	provision, err := env.store.StartDeviceLifecycleOperation(ctx, DeviceLifecycleOperationInput{
		OperationID: "entitlement-provision", CorrelationID: "entitlement-provision", MessageID: "entitlement-provision-message",
		OrganizationID: owner.BrandCloud.ID, DeviceID: device.ID, OperationType: model.DeviceOperationTypeProvision,
		RequestedBy: &requestedBy, RequestPayload: map[string]any{"activity_id": "activity", "clip_public_key": "clip-key"},
		OutboxMessageType: string(channel.MessageTypeDeviceProvisionRequested),
		OutboxPayload: map[string]any{"org_id": owner.BrandCloud.ID, "account_device_id": device.ID,
			"video_cloud_devid": device.ID, "activity_id": "activity", "clip_public_key": "clip-key", "requested_by": requestedBy},
		Now: time.Now().UTC(),
	})
	if err != nil || !provision.Created {
		t.Fatalf("provision=%+v err=%v", provision, err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE device_operations SET status='succeeded',completed_at=now() WHERE operation_id=$1`, provision.Operation.OperationID); err != nil {
		t.Fatal(err)
	}
	input := DeviceEntitlementSnapshotInput{OperationID: "entitlement-revoke-shadow", CorrelationID: "entitlement-revoke-shadow",
		MessageID: "entitlement-message-1", OrganizationID: owner.BrandCloud.ID, DeviceID: device.ID,
		RequestedBy: requestedBy, ServiceOptions: []string{"mqtt"}, State: "active", Now: time.Now().UTC()}
	first, err := env.store.StartDeviceEntitlementSnapshot(ctx, input)
	if err != nil || !first.Created || first.Snapshot.Revision != 1 || first.Snapshot.ProductServiceRevision != 1 ||
		first.Message.MessageType != string(channel.MessageTypeDeviceEntitlementSnapshotRequested) {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if got := fmt.Sprint(first.Message.Payload["service_options"]); got != "[mqtt]" {
		t.Fatalf("outbox services=%s", got)
	}
	replayed, err := env.store.StartDeviceEntitlementSnapshot(ctx, input)
	if err != nil || replayed.Created || replayed.Snapshot.Revision != 1 || replayed.Message.ID != first.Message.ID {
		t.Fatalf("replay=%+v err=%v", replayed, err)
	}
	invalid := input
	invalid.OperationID, invalid.CorrelationID, invalid.MessageID = "entitlement-invalid", "entitlement-invalid", "entitlement-message-invalid"
	invalid.ServiceOptions = []string{"mqtt", "video_storage"}
	if _, err := env.store.StartDeviceEntitlementSnapshot(ctx, invalid); !errors.Is(err, ErrClaimUnsupportedService) {
		t.Fatalf("ungranted option accepted: %v", err)
	}
	var count int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM device_operations WHERE operation_id=$1`, invalid.OperationID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed transaction left operation: %d %v", count, err)
	}
	next := input
	next.OperationID, next.CorrelationID, next.MessageID = "entitlement-suspend", "entitlement-suspend", "entitlement-message-2"
	next.ServiceOptions = nil
	next.State = "suspended"
	second, err := env.store.StartDeviceEntitlementSnapshot(ctx, next)
	if err != nil || second.Snapshot.Revision != 2 || fmt.Sprint(second.Snapshot.ServiceOptions) != "[mqtt]" {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM device_entitlement_snapshots WHERE account_device_id=$1`, device.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("snapshots=%d err=%v", count, err)
	}
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE event_type='device.entitlement.snapshot.requested' AND subject_id=$1`, device.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("audit events=%d err=%v", count, err)
	}
	insertGrant(2, []string{"mqtt"})
	oldRevision := int64(1)
	stale := input
	stale.OperationID, stale.CorrelationID, stale.MessageID = "entitlement-stale-grant", "entitlement-stale-grant", "entitlement-message-stale"
	stale.TargetProductServiceRevision = &oldRevision
	if _, err := env.store.StartDeviceEntitlementSnapshot(ctx, stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale Product revision admitted: %v", err)
	}
	newRevision := int64(2)
	migrate := input
	migrate.OperationID, migrate.CorrelationID, migrate.MessageID = "entitlement-migrate-grant", "entitlement-migrate-grant", "entitlement-message-3"
	migrate.TargetProductServiceRevision = &newRevision
	migrated, err := env.store.StartDeviceEntitlementSnapshot(ctx, migrate)
	if err != nil || migrated.Snapshot.Revision != 3 || migrated.Snapshot.ProductServiceRevision != 2 ||
		migrated.Snapshot.ServiceGrantSHA256 == first.Snapshot.ServiceGrantSHA256 {
		t.Fatalf("migrated=%+v err=%v", migrated, err)
	}
	if _, err := env.db.Exec(ctx, `DELETE FROM device_entitlement_snapshots WHERE operation_id=$1`, next.OperationID); err == nil {
		t.Fatal("immutable snapshot was deleted")
	}
	if _, err := env.db.Exec(ctx, `DELETE FROM devices WHERE id=$1`, device.ID); err != nil {
		t.Fatalf("physical unprovision must not be blocked by retained history: %v", err)
	}
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM device_entitlement_snapshots WHERE account_device_id=$1`, device.ID).Scan(&count); err != nil || count != 3 {
		t.Fatalf("history after unprovision=%d err=%v", count, err)
	}
}
