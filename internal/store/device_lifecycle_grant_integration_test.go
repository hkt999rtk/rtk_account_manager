package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"rtk_account_manager/internal/channel"
	"rtk_account_manager/internal/model"
)

func TestProvisionOutboxPinsProductGrantAcrossReplayAndRevision(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "provision-grant")
	productInput := authorizedProductInput(owner.User.ID, owner.BrandCloud.ID, "provision-grant-product")
	productInput.ServiceOptions = []string{"mqtt", "iot_shadow"}
	product, err := env.store.CreateDeviceItemProfile(ctx, productInput)
	if err != nil {
		t.Fatal(err)
	}
	device, err := env.store.CreateDeviceAsUser(ctx, owner.User.ID, owner.BrandCloud.ID, DeviceInput{
		Name: "Provisioned device", Category: product.Category, DeviceItemProfileID: &product.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	insertGrant := func(options []string) {
		t.Helper()
		tx, err := env.db.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if err := insertProductServiceGrantTx(ctx, tx, product.ID, owner.BrandCloud.ID, options, nil, 1, &owner.User.ID); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	insertGrant([]string{"mqtt", "iot_shadow"})
	env.store.ConfigurePlatformServiceProductWrites("provision-grant-test", true)
	input := DeviceLifecycleOperationInput{
		OperationID: "pinned-provision-1", CorrelationID: "pinned-provision-1", MessageID: "pinned-provision-message-1",
		OrganizationID: owner.BrandCloud.ID, DeviceID: device.ID, OperationType: model.DeviceOperationTypeProvision,
		RequestedBy: &owner.User.ID, RequestPayload: map[string]any{"activity_id": "activity-1", "clip_public_key": "clip-key"},
		OutboxMessageType: string(channel.MessageTypeDeviceProvisionRequested),
		OutboxPayload: map[string]any{
			"org_id": owner.BrandCloud.ID, "account_device_id": device.ID, "video_cloud_devid": device.ID,
			"activity_id": "activity-1", "clip_public_key": "clip-key", "requested_by": owner.User.ID,
		},
		MetadataPatch: PendingProvisionMetadata(device.ID, "activity-1", "clip-key", nil), Now: time.Now().UTC(),
	}
	first, err := env.store.StartDeviceLifecycleOperation(ctx, input)
	if err != nil || !first.Created {
		t.Fatalf("first provision = %+v, err = %v", first, err)
	}
	assertGrant := func(result DeviceLifecycleOperationResult, revision string, options []string) {
		t.Helper()
		if result.Message.Payload["product_id"] != product.ID || fmt.Sprint(result.Message.Payload["product_service_revision"]) != revision ||
			result.Message.Payload["service_grant_sha256"] == "" {
			t.Fatalf("outbox missing Product grant: %+v", result.Message.Payload)
		}
		stored, ok := result.Message.Payload["service_options"].([]any)
		if !ok {
			t.Fatalf("outbox service options type = %T", result.Message.Payload["service_options"])
		}
		actual := make([]string, 0, len(stored))
		for _, option := range stored {
			actual = append(actual, fmt.Sprint(option))
		}
		if !slices.Equal(actual, options) {
			t.Fatalf("outbox services = %v, want %v", actual, options)
		}
	}
	assertGrant(first, "1", []string{"iot_shadow", "mqtt"})
	insertGrant([]string{"mqtt"})
	replayed, err := env.store.StartDeviceLifecycleOperation(ctx, input)
	if err != nil || replayed.Created || replayed.Message.ID != first.Message.ID {
		t.Fatalf("replay = %+v, err = %v", replayed, err)
	}
	assertGrant(replayed, "1", []string{"iot_shadow", "mqtt"})

	next := input
	next.OperationID, next.CorrelationID, next.MessageID = "pinned-provision-2", "pinned-provision-2", "pinned-provision-message-2"
	next.OutboxPayload = map[string]any{
		"org_id": owner.BrandCloud.ID, "account_device_id": device.ID, "video_cloud_devid": device.ID,
		"activity_id": "activity-1", "clip_public_key": "clip-key", "requested_by": owner.User.ID,
		"service_options": []string{"mqtt", "iot_shadow"},
	}
	if _, err := env.store.StartDeviceLifecycleOperation(ctx, next); !errors.Is(err, ErrClaimServiceOptionsMismatch) {
		t.Fatalf("stale caller options admitted: %v", err)
	}
	delete(next.OutboxPayload, "service_options")
	latest, err := env.store.StartDeviceLifecycleOperation(ctx, next)
	if err != nil || !latest.Created {
		t.Fatalf("latest provision = %+v, err = %v", latest, err)
	}
	assertGrant(latest, "2", []string{"mqtt"})
	if _, err := env.db.Exec(ctx, `UPDATE device_operations SET status='succeeded',completed_at=now() WHERE operation_id=$1`, next.OperationID); err != nil {
		t.Fatal(err)
	}
	insertGrant([]string{"mqtt", "iot_shadow"})
	reprovision := next
	reprovision.OperationID, reprovision.CorrelationID, reprovision.MessageID = "pinned-provision-3", "pinned-provision-3", "pinned-provision-message-3"
	reprovision.OutboxPayload = map[string]any{
		"org_id": owner.BrandCloud.ID, "account_device_id": device.ID, "video_cloud_devid": device.ID,
		"activity_id": "activity-1", "clip_public_key": "clip-key", "requested_by": owner.User.ID,
	}
	pinned, err := env.store.StartDeviceLifecycleOperation(ctx, reprovision)
	if err != nil || !pinned.Created {
		t.Fatalf("reprovision from successful pin = %+v, err = %v", pinned, err)
	}
	assertGrant(pinned, "2", []string{"mqtt"})
}

func TestProvisionUsesIssuedFactoryRunPinAfterProductRevision(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "factory-provision-pin")
	productInput := authorizedProductInput(owner.User.ID, owner.BrandCloud.ID, "factory-pin-product")
	productInput.ServiceOptions = []string{"mqtt"}
	product, err := env.store.CreateDeviceItemProfile(ctx, productInput)
	if err != nil {
		t.Fatal(err)
	}
	readyDevicePKIFixture(t, env, product.ID)
	device, err := env.store.CreateDeviceAsUser(ctx, owner.User.ID, owner.BrandCloud.ID, DeviceInput{
		Name: "Factory device", Category: product.Category, DeviceItemProfileID: &product.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := env.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertProductServiceGrantTx(ctx, tx, product.ID, owner.BrandCloud.ID, []string{"mqtt"}, nil, 1, &owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	env.store.ConfigurePlatformServiceProductWrites("factory-pin-test", true)
	run, _, err := env.store.IssueProductionRunAsUser(ctx, authorizedProductionInput(owner.User.ID, owner.BrandCloud.ID, product.ID),
		func(_ model.ProductionRun, _ model.DeviceItemProfile) (string, error) { return "signed-fixture", nil })
	if err != nil || run.ProductServiceRevision == nil {
		t.Fatalf("production run = %+v, err = %v", run, err)
	}
	admission := FactoryEnrollmentAdmission{
		RunID: run.ID, CloudID: owner.BrandCloud.ID, ProductID: product.ID, RequestID: "factory-pin-request",
		DeviceID: "factory-video-device", RequestSHA256: strings.Repeat("b", 64),
		ProductServiceRevision: run.ProductServiceRevision, ServiceGrantSHA256: run.ServiceGrantSHA256, ServiceOptions: []string{"mqtt"},
	}
	reservation, err := env.store.ReserveFactoryEnrollment(ctx, admission)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.CompleteFactoryEnrollment(ctx, FactoryEnrollmentResult{
		CloudID: owner.BrandCloud.ID, RunID: run.ID, ReservationID: reservation.ID,
		RequestSHA256: admission.RequestSHA256, Status: "issued", EvidenceSHA256: strings.Repeat("c", 64),
	}); err != nil {
		t.Fatal(err)
	}
	tx, err = env.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertProductServiceGrantTx(ctx, tx, product.ID, owner.BrandCloud.ID, []string{"mqtt", "iot_shadow"}, nil, 2, &owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	input := DeviceLifecycleOperationInput{
		OperationID: "factory-pinned-provision", CorrelationID: "factory-pinned-provision", MessageID: "factory-pinned-message",
		OrganizationID: owner.BrandCloud.ID, DeviceID: device.ID, OperationType: model.DeviceOperationTypeProvision, RequestedBy: &owner.User.ID,
		RequestPayload:    map[string]any{"activity_id": "activity-1", "clip_public_key": "clip-key"},
		OutboxMessageType: string(channel.MessageTypeDeviceProvisionRequested),
		OutboxPayload: map[string]any{
			"org_id": owner.BrandCloud.ID, "account_device_id": device.ID, "video_cloud_devid": admission.DeviceID,
			"activity_id": "activity-1", "clip_public_key": "clip-key", "requested_by": owner.User.ID,
		},
		Now: time.Now().UTC(),
	}
	result, err := env.store.StartDeviceLifecycleOperation(ctx, input)
	if err != nil || !result.Created || fmt.Sprint(result.Message.Payload["product_service_revision"]) != "1" {
		t.Fatalf("factory run pin was not retained: %+v, err = %v", result.Message.Payload, err)
	}
	if got := result.Message.Payload["service_options"].([]any); len(got) != 1 || got[0] != "mqtt" {
		t.Fatalf("factory run services changed: %+v", got)
	}
}

func TestProvisionUsesClaimTokenPinAfterProductRevision(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "claim-provision-pin")
	productInput := authorizedProductInput(owner.User.ID, owner.BrandCloud.ID, "claim-pin-product")
	productInput.ServiceOptions = []string{"mqtt"}
	product, err := env.store.CreateDeviceItemProfile(ctx, productInput)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := env.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertProductServiceGrantTx(ctx, tx, product.ID, owner.BrandCloud.ID, []string{"mqtt"}, nil, 1, &owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	env.store.ConfigurePlatformServiceProductWrites("claim-pin-test", true)
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin=true WHERE id=$1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	claimInput := platformTokenInput(owner.User.ID, owner.BrandCloud.ID, "claim-pinned-video-device", &product.ID)
	claimInput.ServiceOptions = nil
	if _, err := env.store.CreateDeviceClaimTokenAsPlatform(ctx, claimInput); err != nil {
		t.Fatal(err)
	}
	claimed, err := env.store.ResolveDeviceClaimToken(ctx, DeviceClaimResolveInput{
		OrganizationID: owner.BrandCloud.ID, RequestedBy: owner.User.ID, TokenHash: claimInput.TokenHash, DeviceName: "Claimed device",
	})
	if err != nil {
		t.Fatal(err)
	}
	tx, err = env.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertProductServiceGrantTx(ctx, tx, product.ID, owner.BrandCloud.ID, []string{"mqtt", "iot_shadow"}, nil, 2, &owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	input := DeviceLifecycleOperationInput{
		OperationID: "claim-pinned-provision", CorrelationID: "claim-pinned-provision", MessageID: "claim-pinned-message",
		OrganizationID: owner.BrandCloud.ID, DeviceID: claimed.Device.ID, OperationType: model.DeviceOperationTypeProvision, RequestedBy: &owner.User.ID,
		RequestPayload:    map[string]any{"activity_id": "activity", "clip_public_key": "clip-key"},
		OutboxMessageType: string(channel.MessageTypeDeviceProvisionRequested),
		OutboxPayload: map[string]any{
			"org_id": owner.BrandCloud.ID, "account_device_id": claimed.Device.ID, "video_cloud_devid": claimInput.VideoCloudDevid,
			"activity_id": "activity", "clip_public_key": "clip-key", "requested_by": owner.User.ID,
		},
		Now: time.Now().UTC(),
	}
	result, err := env.store.StartDeviceLifecycleOperation(ctx, input)
	if err != nil || fmt.Sprint(result.Message.Payload["product_service_revision"]) != "1" {
		t.Fatalf("Claim Token pin was not retained: %+v, err = %v", result.Message.Payload, err)
	}
	if got := result.Message.Payload["service_options"].([]any); len(got) != 1 || got[0] != "mqtt" {
		t.Fatalf("Claim Token services changed: %+v", got)
	}
	wrongDevice := input
	wrongDevice.OperationID, wrongDevice.CorrelationID, wrongDevice.MessageID = "wrong-claim-device", "wrong-claim-device", "wrong-claim-message"
	wrongDevice.OutboxPayload = map[string]any{
		"org_id": owner.BrandCloud.ID, "account_device_id": claimed.Device.ID, "video_cloud_devid": "another-video-device",
		"activity_id": "activity", "clip_public_key": "clip-key", "requested_by": owner.User.ID,
	}
	if _, err := env.store.StartDeviceLifecycleOperation(ctx, wrongDevice); !errors.Is(err, ErrConflict) {
		t.Fatalf("Claim Token grant moved to another device: %v", err)
	}
}
