package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"rtk_account_manager/internal/channel"
	"rtk_account_manager/internal/model"
)

func TestProductServiceApplyFreezesMoreThan250DevicesAndRequiresAppliedReceipt(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "product-service-apply")
	input := authorizedProductInput(owner.User.ID, owner.BrandCloud.ID, "product-service-apply")
	input.ServiceOptions = []string{"mqtt"}
	product, err := env.store.CreateDeviceItemProfile(ctx, input)
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
	env.store.ConfigurePlatformServiceProductWrites("test-apply", true)
	for i := 0; i < 251; i++ {
		device, err := env.store.CreateDeviceAsUser(ctx, owner.User.ID, owner.BrandCloud.ID, DeviceInput{
			Name: fmt.Sprintf("Batch device %03d", i), Category: product.Category, DeviceItemProfileID: &product.ID,
		})
		if err != nil {
			t.Fatal(err)
		}
		operationID := fmt.Sprintf("batch-provision-%03d", i)
		_, err = env.store.StartDeviceLifecycleOperation(ctx, DeviceLifecycleOperationInput{
			OperationID: operationID, CorrelationID: operationID, MessageID: operationID + "-message",
			OrganizationID: owner.BrandCloud.ID, DeviceID: device.ID, OperationType: model.DeviceOperationTypeProvision,
			RequestedBy: &owner.User.ID, RequestPayload: map[string]any{"activity_id": "activity", "clip_public_key": "clip-key"},
			OutboxMessageType: string(channel.MessageTypeDeviceProvisionRequested),
			OutboxPayload: map[string]any{"org_id": owner.BrandCloud.ID, "account_device_id": device.ID,
				"video_cloud_devid": device.ID, "activity_id": "activity", "clip_public_key": "clip-key", "requested_by": owner.User.ID},
			Now: time.Now().UTC(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.db.Exec(ctx, `UPDATE device_operations SET status='succeeded',completed_at=now() WHERE operation_id=$1`, operationID); err != nil {
			t.Fatal(err)
		}
		if _, err := env.db.Exec(ctx, `UPDATE devices SET metadata=jsonb_set(metadata,'{video_cloud_devid}',to_jsonb($2::text),true) WHERE id=$1`, device.ID, device.ID); err != nil {
			t.Fatal(err)
		}
	}
	preview, err := env.store.PreviewProductServiceApply(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID)
	if err != nil || preview.TotalDevices != 251 || len(preview.Blockers) != 0 {
		t.Fatalf("preview total=%d blockers=%d first=%v err=%v", preview.TotalDevices, len(preview.Blockers), firstApplyBlocker(preview.Blockers), err)
	}
	jobID := "job-aaaaaaaaaaaaaaaaaaaaaaaa"
	job, err := env.store.AdmitProductServiceApply(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, jobID, preview.PreviewToken)
	if err != nil || job.TotalDevices != 251 {
		t.Fatalf("admission = %+v, %v", job, err)
	}
	if _, err := env.store.AdmitProductServiceApply(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, jobID, preview.PreviewToken); err != nil {
		t.Fatalf("idempotent admission: %v", err)
	}
	if _, err := env.store.AdmitProductServiceApply(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, jobID, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed preview accepted: %v", err)
	}
	last, err := env.store.ListProductServiceApplyItems(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, jobID, 100, 250)
	if err != nil || last.Total != 251 || len(last.Items) != 1 || last.Items[0].OperationID == "" {
		t.Fatalf("last page = %+v, %v", last, err)
	}
	item, err := env.store.DispatchProductServiceApplyItem(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, jobID, last.Items[0].DeviceID, "batch-message")
	if err != nil || item.Status != "accepted" {
		t.Fatalf("dispatch = %+v, %v", item, err)
	}
	if _, err := env.store.FinishProductServiceApply(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, jobID, "complete"); !errors.Is(err, ErrConflict) {
		t.Fatalf("202 completed job: %v", err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE device_operations SET status='succeeded',result_payload=jsonb_build_object('platform_entitlement_revision',1),completed_at=now() WHERE operation_id=$1`, item.OperationID); err != nil {
		t.Fatal(err)
	}
	item, err = env.store.GetProductServiceApplyItem(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, jobID, item.DeviceID)
	if err != nil || item.Status != "applied" || item.AppliedRevision == nil || *item.AppliedRevision != job.TargetRevision {
		t.Fatalf("ack = %+v, %v", item, err)
	}
	firstPage, err := env.store.ListProductServiceApplyItems(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, jobID, 1, 0)
	if err != nil || len(firstPage.Items) != 1 {
		t.Fatalf("first page = %+v, %v", firstPage, err)
	}
	retryItem, err := env.store.DispatchProductServiceApplyItem(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, jobID, firstPage.Items[0].DeviceID, "retry-first-message")
	if err != nil || retryItem.Status != "accepted" {
		t.Fatalf("retry initial dispatch = %+v, %v", retryItem, err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE device_operations SET status='dead_lettered',error_code='publish_failed',retryable=true,completed_at=now() WHERE operation_id=$1`, retryItem.OperationID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE device_message_outbox SET status='dead_lettered',attempt_count=5 WHERE operation_id=$1`, retryItem.OperationID); err != nil {
		t.Fatal(err)
	}
	retryItem, err = env.store.GetProductServiceApplyItem(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, jobID, retryItem.DeviceID)
	if err != nil || retryItem.Status != "failed" || !retryItem.Retryable {
		t.Fatalf("retryable dead letter = %+v, %v", retryItem, err)
	}
	retryItem, err = env.store.DispatchProductServiceApplyItem(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, jobID, retryItem.DeviceID, "ignored-new-message")
	if err != nil || retryItem.Status != "accepted" {
		t.Fatalf("same-operation retry = %+v, %v", retryItem, err)
	}
	var attemptCount int
	var outboxStatus string
	if err := env.db.QueryRow(ctx, `SELECT attempt_count,status FROM device_message_outbox WHERE operation_id=$1`, retryItem.OperationID).Scan(&attemptCount, &outboxStatus); err != nil || attemptCount != 0 || outboxStatus != "retrying" {
		t.Fatalf("retry outbox = %d %s %v", attemptCount, outboxStatus, err)
	}
	if _, err := env.store.FinishProductServiceApply(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, jobID, "cancel"); err != nil {
		t.Fatal(err)
	}
}

func firstApplyBlocker(blockers []ProductServiceApplyBlocker) any {
	if len(blockers) == 0 {
		return nil
	}
	return blockers[0]
}

func TestProductServiceApplyRejectsChangedGrantAndMembership(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "product-service-apply-conflict")
	input := authorizedProductInput(owner.User.ID, owner.BrandCloud.ID, "product-service-apply-conflict")
	product, err := env.store.CreateDeviceItemProfile(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	insert := func() {
		t.Helper()
		tx, err := env.db.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if err := insertProductServiceGrantTx(ctx, tx, product.ID, owner.BrandCloud.ID, []string{"mqtt"}, nil, 1, &owner.User.ID); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	insert()
	env.store.ConfigurePlatformServiceProductWrites("test-apply-conflict", true)
	preview, err := env.store.PreviewProductServiceApply(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID)
	if err != nil {
		t.Fatal(err)
	}
	insert()
	jobID := "job-bbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := env.store.AdmitProductServiceApply(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, jobID, preview.PreviewToken); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed Product grant accepted: %v", err)
	}
	preview, err = env.store.PreviewProductServiceApply(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID)
	if err != nil || preview.TargetRevision != 2 {
		t.Fatalf("new target = %+v %v", preview, err)
	}
	if _, err := env.store.CreateDeviceAsUser(ctx, owner.User.ID, owner.BrandCloud.ID, DeviceInput{
		Name: "New member", Category: product.Category, DeviceItemProfileID: &product.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.AdmitProductServiceApply(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, jobID, preview.PreviewToken); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed membership accepted: %v", err)
	}
	preview, err = env.store.PreviewProductServiceApply(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID)
	if err != nil || preview.TotalDevices != 1 || len(preview.Blockers) != 1 || preview.Blockers[0].Code != "untrusted_entitlement" {
		t.Fatalf("unprovisioned member not blocked: %+v %v", preview, err)
	}
}

func TestProductRetentionEditCreatesPinnedGrantRevision(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "product-retention-version")
	days7, days30 := 7, 30
	input := authorizedProductInput(owner.User.ID, owner.BrandCloud.ID, "product-retention-version")
	input.ServiceOptions = []string{"mqtt", "device_logging"}
	input.LogRetentionDays = &days7
	product, err := env.store.CreateDeviceItemProfile(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := env.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertProductServiceGrantTx(ctx, tx, product.ID, owner.BrandCloud.ID, input.ServiceOptions, nil, 1, &owner.User.ID, &days7); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	env.store.ConfigurePlatformServiceProductWrites("test-retention-version", true)
	before, err := env.store.GetCurrentProductGrantSummary(ctx, owner.BrandCloud.ID, product.ID)
	if err != nil || before.Revision != 1 || before.LogRetentionDays == nil || *before.LogRetentionDays != 7 {
		t.Fatalf("initial grant = %+v %v", before, err)
	}
	updated, err := env.store.UpdateDeviceItemProfileAsUser(ctx, DeviceItemProfileUpdateInput{
		ActorUserID: &owner.User.ID, BrandCloudID: owner.BrandCloud.ID, ProfileID: product.ID, LogRetentionDays: &days30,
	})
	if err != nil || updated.LogRetentionDays == nil || *updated.LogRetentionDays != 30 {
		t.Fatalf("retention edit = %+v %v", updated, err)
	}
	after, err := env.store.GetCurrentProductGrantSummary(ctx, owner.BrandCloud.ID, product.ID)
	if err != nil || after.Revision != 2 || after.Digest == before.Digest || after.LogRetentionDays == nil || *after.LogRetentionDays != 30 {
		t.Fatalf("new immutable grant = %+v %v", after, err)
	}
	var originalRetention int
	if err := env.db.QueryRow(ctx, `SELECT log_retention_days FROM product_service_grants WHERE product_id=$1 AND revision=1`, product.ID).Scan(&originalRetention); err != nil || originalRetention != 7 {
		t.Fatalf("historical retention = %d %v", originalRetention, err)
	}
}

func TestProductServiceApplyAcceptsTrustedLegacyDeviceWithoutMQTT(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "product-legacy-mqtt-upgrade")
	input := authorizedProductInput(owner.User.ID, owner.BrandCloud.ID, "product-legacy-mqtt-upgrade")
	input.ServiceOptions = []string{"video_storage"}
	product, err := env.store.CreateDeviceItemProfile(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := env.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertProductServiceGrantTx(ctx, tx, product.ID, owner.BrandCloud.ID, input.ServiceOptions, nil, 1, &owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	device, err := env.store.CreateDeviceAsUser(ctx, owner.User.ID, owner.BrandCloud.ID, DeviceInput{
		Name: "Old video device", Category: product.Category, DeviceItemProfileID: &product.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	env.store.ConfigurePlatformServiceProductWrites("test-legacy-upgrade", true)
	op, err := env.store.StartDeviceLifecycleOperation(ctx, DeviceLifecycleOperationInput{
		OperationID: "legacy-video-provision", CorrelationID: "legacy-video-provision", MessageID: "legacy-video-message",
		OrganizationID: owner.BrandCloud.ID, DeviceID: device.ID, OperationType: model.DeviceOperationTypeProvision,
		RequestedBy: &owner.User.ID, RequestPayload: map[string]any{"activity_id": "activity", "clip_public_key": "clip-key"},
		OutboxMessageType: string(channel.MessageTypeDeviceProvisionRequested),
		OutboxPayload: map[string]any{"org_id": owner.BrandCloud.ID, "account_device_id": device.ID,
			"video_cloud_devid": device.ID, "activity_id": "activity", "clip_public_key": "clip-key", "requested_by": owner.User.ID},
		Now: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE device_operations SET status='succeeded',completed_at=now() WHERE operation_id=$1`, op.Operation.OperationID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE devices SET metadata=jsonb_set(metadata,'{video_cloud_devid}',to_jsonb($2::text),true) WHERE id=$1`, device.ID, device.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.UpdateDeviceItemProfile(ctx, DeviceItemProfileUpdateInput{
		BrandCloudID: owner.BrandCloud.ID, ProfileID: product.ID, ServiceOptions: []string{"mqtt", "video_storage"},
	}); err != nil {
		t.Fatal(err)
	}
	tx, err = env.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertProductServiceGrantTx(ctx, tx, product.ID, owner.BrandCloud.ID, []string{"mqtt", "video_storage"}, nil, 1, &owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	preview, err := env.store.PreviewProductServiceApply(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID)
	if err != nil || len(preview.Blockers) != 0 || preview.TargetRevision != 2 || preview.AddedCount != 1 || !slices.Equal(preview.AddedOptions, []string{"mqtt"}) {
		t.Fatalf("legacy MQTT upgrade preview = %+v %v", preview, err)
	}
	if _, err := env.store.AdmitProductServiceApply(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID,
		"job-cccccccccccccccccccccccc", preview.PreviewToken); err != nil {
		t.Fatalf("legacy MQTT upgrade admission: %v", err)
	}
}

func TestProductServiceApplyCompletesEmptyFrozenJobAndKeepsTerminalState(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "product-apply-empty-complete")
	input := authorizedProductInput(owner.User.ID, owner.BrandCloud.ID, "product-apply-empty-complete")
	product, err := env.store.CreateDeviceItemProfile(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.PreviewProductServiceApply(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("preview before registered writes are enabled = %v, want ErrConflict", err)
	}
	env.store.ConfigurePlatformServiceProductWrites("test-apply-complete", true)
	if _, err := env.store.PreviewProductServiceApply(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("preview without a trustworthy Product grant = %v, want ErrConflict", err)
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
	preview, err := env.store.PreviewProductServiceApply(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID)
	if err != nil || preview.TotalDevices != 0 || len(preview.Blockers) != 0 {
		t.Fatalf("empty Product preview = %+v, %v", preview, err)
	}
	jobID := "job-dddddddddddddddddddddddd"
	if _, err := env.store.AdmitProductServiceApply(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, "bad-job", preview.PreviewToken); !errors.Is(err, ErrConflict) {
		t.Fatalf("malformed operation ID admitted: %v", err)
	}
	if _, err := env.store.AdmitProductServiceApply(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, jobID, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("missing preview token admitted: %v", err)
	}
	job, err := env.store.AdmitProductServiceApply(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, jobID, preview.PreviewToken)
	if err != nil || job.Status != "active" || job.TotalDevices != 0 {
		t.Fatalf("empty Product admission = %+v, %v", job, err)
	}
	if _, err := env.store.AdmitProductServiceApply(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID,
		"job-eeeeeeeeeeeeeeeeeeeeeeee", preview.PreviewToken); !errors.Is(err, ErrConflict) {
		t.Fatalf("second active Product apply job = %v, want ErrConflict", err)
	}
	read, err := env.store.GetProductServiceApplyJob(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, jobID)
	if err != nil || read.TargetRevision != preview.TargetRevision || read.TargetDigest != preview.TargetDigest {
		t.Fatalf("frozen target read = %+v, %v", read, err)
	}
	page, err := env.store.ListProductServiceApplyItems(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, jobID, 0, -1)
	if err != nil || page.Total != 0 || len(page.Items) != 0 {
		t.Fatalf("empty job page = %+v, %v", page, err)
	}
	if _, err := env.store.GetProductServiceApplyItem(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, jobID, product.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing job item = %v, want ErrNotFound", err)
	}
	if _, err := env.store.FinishProductServiceApply(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, jobID, "pause"); !errors.Is(err, ErrConflict) {
		t.Fatalf("unsupported finish action = %v, want ErrConflict", err)
	}
	completed, err := env.store.FinishProductServiceApply(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, jobID, "complete")
	if err != nil || completed.Status != "completed" {
		t.Fatalf("empty job completion = %+v, %v", completed, err)
	}
	replay, err := env.store.FinishProductServiceApply(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, jobID, "complete")
	if err != nil || replay.Status != "completed" {
		t.Fatalf("completion replay = %+v, %v", replay, err)
	}
	if _, err := env.store.FinishProductServiceApply(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, jobID, "cancel"); !errors.Is(err, ErrConflict) {
		t.Fatalf("terminal job canceled = %v, want ErrConflict", err)
	}
	replay, err = env.store.AdmitProductServiceApply(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, jobID, preview.PreviewToken)
	if err != nil || replay.Status != "completed" {
		t.Fatalf("same immutable admission replay = %+v, %v", replay, err)
	}
}

func TestProductServiceApplyReadModelRequiresExactAppliedReceipt(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "product-apply-read-model")
	input := authorizedProductInput(owner.User.ID, owner.BrandCloud.ID, "product-apply-read-model")
	input.ServiceOptions = []string{"mqtt"}
	product, err := env.store.CreateDeviceItemProfile(ctx, input)
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
	env.store.ConfigurePlatformServiceProductWrites("test-apply-read-model", true)
	device, err := env.store.CreateDeviceAsUser(ctx, owner.User.ID, owner.BrandCloud.ID, DeviceInput{
		Name: "Read model device", Category: product.Category, DeviceItemProfileID: &product.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	applied, pending, result, err := env.store.GetDeviceEntitlementReadModel(ctx, owner.BrandCloud.ID, device.ID)
	if err != nil || applied != nil || pending != nil || result != nil {
		t.Fatalf("unprovisioned read model = %v, %v, %+v, %v", applied, pending, result, err)
	}
	provisionID := "read-model-provision"
	_, err = env.store.StartDeviceLifecycleOperation(ctx, DeviceLifecycleOperationInput{
		OperationID: provisionID, CorrelationID: provisionID, MessageID: provisionID + "-message",
		OrganizationID: owner.BrandCloud.ID, DeviceID: device.ID, OperationType: model.DeviceOperationTypeProvision,
		RequestedBy: &owner.User.ID, RequestPayload: map[string]any{"activity_id": "activity", "clip_public_key": "clip-key"},
		OutboxMessageType: string(channel.MessageTypeDeviceProvisionRequested),
		OutboxPayload: map[string]any{"org_id": owner.BrandCloud.ID, "account_device_id": device.ID,
			"video_cloud_devid": device.ID, "activity_id": "activity", "clip_public_key": "clip-key", "requested_by": owner.User.ID},
		Now: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE device_operations SET status='succeeded',completed_at=now() WHERE operation_id=$1`, provisionID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE devices SET metadata=jsonb_set(metadata,'{video_cloud_devid}',to_jsonb($2::text),true) WHERE id=$1`, device.ID, device.ID); err != nil {
		t.Fatal(err)
	}
	applied, pending, result, err = env.store.GetDeviceEntitlementReadModel(ctx, owner.BrandCloud.ID, device.ID)
	if err != nil || applied == nil || *applied != 1 || pending != nil || result != nil {
		t.Fatalf("provisioned grant read model = %v, %v, %+v, %v", applied, pending, result, err)
	}
	tx, err = env.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertProductServiceGrantTx(ctx, tx, product.ID, owner.BrandCloud.ID, []string{"mqtt"}, nil, 1, &owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `INSERT INTO device_operations
		(operation_id,correlation_id,organization_id,device_id,operation_type,status)
		VALUES('read-model-conflict','read-model-conflict',$1,$2,'entitlement_update','pending')`, owner.BrandCloud.ID, device.ID); err != nil {
		t.Fatal(err)
	}
	blocked, err := env.store.PreviewProductServiceApply(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID)
	if err != nil || len(blocked.Blockers) != 1 || blocked.Blockers[0].Code != "conflicting_operation" {
		t.Fatalf("pending operation preview = %+v, %v", blocked, err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE device_operations SET status='failed',completed_at=now() WHERE operation_id='read-model-conflict'`); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE devices SET metadata=jsonb_set(metadata,'{video_cloud_devid}',to_jsonb('wrong-device'::text),true) WHERE id=$1`, device.ID); err != nil {
		t.Fatal(err)
	}
	blocked, err = env.store.PreviewProductServiceApply(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID)
	if err != nil || len(blocked.Blockers) != 1 || blocked.Blockers[0].Code != "identity_conflict" {
		t.Fatalf("identity conflict preview = %+v, %v", blocked, err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE devices SET metadata=jsonb_set(metadata,'{video_cloud_devid}',to_jsonb($2::text),true) WHERE id=$1`, device.ID, device.ID); err != nil {
		t.Fatal(err)
	}
	preview, err := env.store.PreviewProductServiceApply(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID)
	if err != nil || preview.TargetRevision != 2 || len(preview.Blockers) != 0 {
		t.Fatalf("clean target preview = %+v, %v", preview, err)
	}
	jobID := "job-eeeeeeeeeeeeeeeeeeeeeeee"
	job, err := env.store.AdmitProductServiceApply(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, jobID, preview.PreviewToken)
	if err != nil || job.TargetRevision != 2 {
		t.Fatalf("frozen target admission = %+v, %v", job, err)
	}
	item, err := env.store.DispatchProductServiceApplyItem(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, jobID, device.ID, "read-model-message")
	if err != nil || item.Status != "accepted" {
		t.Fatalf("submitted item = %+v, %v", item, err)
	}
	if _, err := env.db.Exec(ctx, `INSERT INTO device_operations
		(operation_id,correlation_id,organization_id,device_id,operation_type,status)
		VALUES('read-model-replay-conflict','read-model-replay-conflict',$1,$2,'entitlement_update','pending')`, owner.BrandCloud.ID, device.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.DispatchProductServiceApplyItem(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, jobID, device.ID, "replay-with-conflict"); !errors.Is(err, ErrConflict) {
		t.Fatalf("replay across a concurrent operation = %v, want ErrConflict", err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE device_operations SET status='failed',completed_at=now() WHERE operation_id='read-model-replay-conflict'`); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE devices SET metadata=jsonb_set(metadata,'{video_cloud_devid}',to_jsonb('other-device'::text),true) WHERE id=$1`, device.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.DispatchProductServiceApplyItem(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, jobID, device.ID, "replay-with-new-identity"); !errors.Is(err, ErrConflict) {
		t.Fatalf("replay after identity change = %v, want ErrConflict", err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE devices SET metadata=jsonb_set(metadata,'{video_cloud_devid}',to_jsonb($2::text),true) WHERE id=$1`, device.ID, device.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE devices SET metadata=jsonb_set(metadata,'{video_cloud_activation_status}',to_jsonb('deactivated'::text),true) WHERE id=$1`, device.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.DispatchProductServiceApplyItem(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, jobID, device.ID, "replay-after-deactivation"); !errors.Is(err, ErrConflict) {
		t.Fatalf("replay after deactivation = %v, want ErrConflict", err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE devices SET metadata=metadata-'video_cloud_activation_status' WHERE id=$1`, device.ID); err != nil {
		t.Fatal(err)
	}
	applied, pending, result, err = env.store.GetDeviceEntitlementReadModel(ctx, owner.BrandCloud.ID, device.ID)
	if err != nil || applied == nil || *applied != 1 || pending == nil || *pending != 2 || result == nil || result.OperationID != item.OperationID {
		t.Fatalf("accepted grant treated as applied = %v, %v, %+v, %v", applied, pending, result, err)
	}
	var entitlementRevision int64
	if err := env.db.QueryRow(ctx, `SELECT revision FROM device_entitlement_snapshots WHERE operation_id=$1`, item.OperationID).Scan(&entitlementRevision); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE device_operations SET status='succeeded',result_payload=jsonb_build_object('platform_entitlement_revision',$2::bigint),completed_at=now() WHERE operation_id=$1`, item.OperationID, entitlementRevision); err != nil {
		t.Fatal(err)
	}
	applied, pending, result, err = env.store.GetDeviceEntitlementReadModel(ctx, owner.BrandCloud.ID, device.ID)
	if err != nil || applied == nil || *applied != 2 || pending != nil || result == nil || result.Status != "succeeded" {
		t.Fatalf("applied grant read model = %v, %v, %+v, %v", applied, pending, result, err)
	}
	item, err = env.store.DispatchProductServiceApplyItem(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, jobID, device.ID, "ignored-replay-message")
	if err != nil || item.Status != "applied" || item.OperationID != jobID+"/"+device.ID {
		t.Fatalf("applied item replay = %+v, %v", item, err)
	}
	completed, err := env.store.FinishProductServiceApply(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, jobID, "complete")
	if err != nil || completed.Status != "completed" {
		t.Fatalf("exact receipt job completion = %+v, %v", completed, err)
	}
	if _, err := env.store.GetProductServiceApplyJob(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, "job-ffffffffffffffffffffffff"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown apply job = %v, want ErrNotFound", err)
	}
	if _, err := env.store.GetProductServiceApplyItem(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, jobID, product.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown apply item = %v, want ErrNotFound", err)
	}
	tx, err = env.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertProductServiceGrantTx(ctx, tx, product.ID, owner.BrandCloud.ID, []string{"mqtt"}, nil, 1, &owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	preview, err = env.store.PreviewProductServiceApply(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID)
	if err != nil || preview.TargetRevision != 3 || len(preview.Blockers) != 0 {
		t.Fatalf("next Product revision preview = %+v, %v", preview, err)
	}
	failedJobID := "job-ffffffffffffffffffffffff"
	if _, err := env.store.AdmitProductServiceApply(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, failedJobID, preview.PreviewToken); err != nil {
		t.Fatalf("next Product revision admission: %v", err)
	}
	failedItem, err := env.store.DispatchProductServiceApplyItem(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, failedJobID, device.ID, "read-model-failed-message")
	if err != nil || failedItem.Status != "accepted" {
		t.Fatalf("next Product revision dispatch = %+v, %v", failedItem, err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE device_operations SET status='failed',error_code='not_authorized',retryable=false,completed_at=now() WHERE operation_id=$1`, failedItem.OperationID); err != nil {
		t.Fatal(err)
	}
	blocked, err = env.store.PreviewProductServiceApply(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID)
	if err != nil || len(blocked.Blockers) != 1 || blocked.Blockers[0].Code != "unapplied_entitlement" {
		t.Fatalf("failed grant operation preview = %+v, %v", blocked, err)
	}
	applied, pending, result, err = env.store.GetDeviceEntitlementReadModel(ctx, owner.BrandCloud.ID, device.ID)
	if err != nil || applied == nil || *applied != 2 || pending != nil || result == nil || result.Status != "failed" || result.ErrorCode != "not_authorized" {
		t.Fatalf("failed grant read model = %v, %v, %+v, %v", applied, pending, result, err)
	}
	if _, err := env.store.DispatchProductServiceApplyItem(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, failedJobID, device.ID, "ignored-failed-message"); !errors.Is(err, ErrConflict) {
		t.Fatalf("nonretryable failure dispatched again: %v", err)
	}
	if _, err := env.store.FinishProductServiceApply(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, failedJobID, "complete"); !errors.Is(err, ErrConflict) {
		t.Fatalf("failed item completed job: %v", err)
	}
	canceled, err := env.store.FinishProductServiceApply(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, failedJobID, "cancel")
	if err != nil || canceled.Status != "canceled" {
		t.Fatalf("failed job cancellation = %+v, %v", canceled, err)
	}
	canceled, err = env.store.FinishProductServiceApply(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, failedJobID, "cancel")
	if err != nil || canceled.Status != "canceled" {
		t.Fatalf("cancel replay = %+v, %v", canceled, err)
	}
}
