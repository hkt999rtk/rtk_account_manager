package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"rtk_account_manager/internal/channel"
	"rtk_account_manager/internal/model"
)

func lifecycleUserWrites(s *Store, ctx context.Context, actor, cloud, device string) map[string]func() (DeviceLifecycleOperationResult, error) {
	return map[string]func() (DeviceLifecycleOperationResult, error){
		"provision": func() (DeviceLifecycleOperationResult, error) {
			return s.StartDeviceLifecycleOperation(ctx, DeviceLifecycleOperationInput{
				OperationID: "auth-provision", CorrelationID: "auth-provision", MessageID: "auth-provision-message",
				OrganizationID: cloud, DeviceID: device, RequestedBy: &actor,
				OperationType:     model.DeviceOperationTypeProvision,
				OutboxMessageType: string(channel.MessageTypeDeviceProvisionRequested),
				RequestPayload:    map[string]any{"video_cloud_devid": "auth-device"},
				OutboxPayload:     map[string]any{"org_id": cloud, "account_device_id": device, "requested_by": actor},
				MetadataPatch:     PendingProvisionMetadata("auth-device", "activity", "key", nil),
			})
		},
		"deactivate": func() (DeviceLifecycleOperationResult, error) {
			return s.StartDeviceDeactivationOperation(ctx, DeviceDeactivationOperationInput{
				OperationID: "auth-deactivate", CorrelationID: "auth-deactivate", MessageID: "auth-deactivate-message",
				OrganizationID: cloud, DeviceID: device, RequestedBy: &actor, Reason: "user_request",
			})
		},
	}
}

func TestLifecycleUserAdmissionRechecksAuthorityAndAuditsOnce(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "lifecycle-auth-owner")
	other := handoffDeveloper(t, env, "lifecycle-auth-other")
	d, err := env.store.CreateDevice(ctx, owner.BrandCloud.ID, DeviceInput{Name: "Original", Category: model.DeviceCategoryIPCamera, Metadata: map[string]any{"video_cloud_devid": "auth-device"}})
	if err != nil {
		t.Fatal(err)
	}
	deny := func(actor, cloud string) {
		t.Helper()
		for name, write := range lifecycleUserWrites(env.store, ctx, actor, cloud, d.ID) {
			if _, err := write(); !errors.Is(err, ErrNotFound) {
				t.Fatalf("%s bypassed admission: %v", name, err)
			}
		}
	}
	deny("", owner.BrandCloud.ID)
	deny(other.User.ID, owner.BrandCloud.ID)
	deny(owner.User.ID, other.BrandCloud.ID)
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin=true WHERE id=$1`, other.User.ID); err != nil {
		t.Fatal(err)
	}
	deny(other.User.ID, owner.BrandCloud.ID)
	if _, err := env.db.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role,access_scope) VALUES($1,$2,'viewer','{"kind":"all_products"}')`, owner.BrandCloud.ID, other.User.ID); err != nil {
		t.Fatal(err)
	}
	deny(other.User.ID, owner.BrandCloud.ID)
	if _, err := env.db.Exec(ctx, `UPDATE users SET signup_pending_verification=true WHERE id=$1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	deny(owner.User.ID, owner.BrandCloud.ID)
	if _, err := env.db.Exec(ctx, `UPDATE users SET signup_pending_verification=false WHERE id=$1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	for name, write := range lifecycleUserWrites(env.store, ctx, owner.User.ID, owner.BrandCloud.ID, d.ID) {
		first, err := write()
		if err != nil || !first.Created {
			t.Fatalf("%s failed: %+v %v", name, first, err)
		}
		replay, err := write()
		if err != nil || replay.Created || replay.Operation.ID != first.Operation.ID || replay.Message.ID != first.Message.ID {
			t.Fatalf("%s replay: %+v %v", name, replay, err)
		}
	}
	var audits int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE organization_id=$1 AND event_type LIKE 'device.lifecycle.%'`, owner.BrandCloud.ID).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("audit count %d: %v", audits, err)
	}
	configureTestHandoff(t, env)
	handoffRequest(t, env, owner, other, "lifecycle-auth-transfer")
	if _, err := env.store.AcceptBrandCloudOwnerTransfer(ctx, other.User.ID, "lifecycle-auth-transfer", time.Now()); err != nil {
		t.Fatal(err)
	}
	// Even an idempotent store replay rechecks the current lifecycle fence.
	deny(owner.User.ID, owner.BrandCloud.ID)
}

func TestLifecycleUserAdmissionAuditFailureRollsBackOutboxAndProjection(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "lifecycle-audit-owner")
	d, err := env.store.CreateDevice(ctx, owner.BrandCloud.ID, DeviceInput{Name: "Original", Category: model.DeviceCategoryIPCamera, Metadata: map[string]any{"video_cloud_devid": "auth-device"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `ALTER TABLE audit_events ADD CONSTRAINT reject_lifecycle_audit_test CHECK(event_type NOT LIKE 'device.lifecycle.%') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	defer env.db.Exec(context.Background(), `ALTER TABLE audit_events DROP CONSTRAINT IF EXISTS reject_lifecycle_audit_test`)
	for name, write := range lifecycleUserWrites(env.store, ctx, owner.User.ID, owner.BrandCloud.ID, d.ID) {
		if _, err := write(); err == nil {
			t.Fatalf("%s survived audit failure", name)
		}
	}
	var operations, messages int
	if err := env.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM device_operations WHERE organization_id=$1),(SELECT count(*) FROM device_message_outbox)`, owner.BrandCloud.ID).Scan(&operations, &messages); err != nil || operations != 0 || messages != 0 {
		t.Fatalf("audit failure left work: %d/%d %v", operations, messages, err)
	}
	got, err := env.store.GetDevice(ctx, owner.BrandCloud.ID, d.ID)
	if err != nil || len(got.Metadata) != 1 || got.Metadata["video_cloud_devid"] != "auth-device" {
		t.Fatalf("audit failure changed projection: %+v %v", got, err)
	}
}

func TestLifecycleUserAdmissionRejectsAbsentActorAndWrongOperation(t *testing.T) {
	s := &Store{}
	if _, err := s.StartDeviceLifecycleOperation(context.Background(), DeviceLifecycleOperationInput{}); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.StartDeviceDeactivationOperation(context.Background(), DeviceDeactivationOperationInput{}); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	actor := "actor"
	if _, err := s.StartDeviceLifecycleOperation(context.Background(), DeviceLifecycleOperationInput{RequestedBy: &actor, OperationType: model.DeviceOperationTypeUnprovision}); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
}
