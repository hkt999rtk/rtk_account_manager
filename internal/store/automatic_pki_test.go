package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"rtk_account_manager/internal/model"
)

func TestDevicePKIReceiptRejectsInvalidAuthority(t *testing.T) {
	for _, receipt := range []DevicePKIReceipt{
		{Status: "unknown"},
		{Status: "ready", OperationID: "bad", IssuerID: "6baf97c3-7d83-4ec2-85b0-9e092f5a4df0"},
		{Status: "pending", IssuerID: "6baf97c3-7d83-4ec2-85b0-9e092f5a4df0"},
		{Status: "failed", OperationID: "bad"},
	} {
		if err := receipt.Validate(); !errors.Is(err, ErrConflict) {
			t.Fatalf("invalid Device PKI receipt was accepted: %+v", receipt)
		}
	}
}

// Authorization tests do not run a CA service. Mark only their named Product
// fixture ready; automatic provisioning tests deliberately do not use this helper.
func readyDevicePKIFixture(t *testing.T, env storeIntegrationEnv, product string) {
	t.Helper()
	if _, err := env.db.Exec(context.Background(), `UPDATE device_item_profiles SET pki_status='ready',pki_issuer_id=gen_random_uuid() WHERE id=$1`, product); err != nil {
		t.Fatal(err)
	}
}

func TestAutomaticPKIOwnerTransferRetainsIssuerIdentity(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	_, ack, _ := readyCommitFixture(t, env)
	var productID string
	if err := env.db.QueryRow(ctx, `INSERT INTO device_item_profiles(brand_cloud_id,profile_key,display_name,category,ca_profile,issuer_profile) VALUES($1,'retained-ca','Retained CA','ip_camera','ca','issuer') RETURNING id::text`, ack.CloudID).Scan(&productID); err != nil {
		t.Fatal(err)
	}
	cloudIssuer, productIssuer := "cfa65e7c-b29c-4657-95f6-fc7a03bd5792", "fbff2f76-56a1-4df7-895f-4f685c0926f4"
	if _, err := env.db.Exec(ctx, `UPDATE organizations SET pki_status='ready',pki_issuer_id=$2 WHERE id=$1`, ack.CloudID, cloudIssuer); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE device_item_profiles SET pki_status='ready',pki_issuer_id=$2 WHERE id=$1`, productID, productIssuer); err != nil {
		t.Fatal(err)
	}
	beforeCloud, err := env.store.GetBrandCloud(ctx, ack.CloudID)
	if err != nil {
		t.Fatal(err)
	}
	beforeProduct, err := env.store.GetDeviceItemProfile(ctx, ack.CloudID, productID)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := env.store.CommitOwnerHandoff(ctx, ack.CloudID, ack.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = env.store.FinalizeOwnerHandoff(ctx, ack.CloudID, ack.OperationID); err != nil {
		t.Fatal(err)
	}
	for _, participant := range RequiredHandoffProducers() {
		if _, err = env.store.RecordHandoffFinalizationAck(ctx, HandoffFinalizationAck{CloudID: ack.CloudID, OperationID: ack.OperationID, OwnershipVersion: 1, DecisionSHA256: decision.DecisionSHA256, Participant: participant, ReceiptSHA256: strings.Repeat("c", 64)}); err != nil {
			t.Fatal(err)
		}
	}
	assertHandoffOwner(t, env, ack, ack.TargetUserID, "succeeded", 2)
	afterCloud, err := env.store.GetBrandCloud(ctx, ack.CloudID)
	if err != nil || afterCloud.PKIIssuerID != cloudIssuer || afterCloud.PKIStatus != "ready" || afterCloud.PKIOperationID != beforeCloud.PKIOperationID {
		t.Fatal("handoff changed Cloud CA", err)
	}
	afterProduct, err := env.store.GetDeviceItemProfile(ctx, ack.CloudID, productID)
	if err != nil || afterProduct.PKIIssuerID != productIssuer || afterProduct.PKIStatus != "ready" || afterProduct.PKIOperationID != beforeProduct.PKIOperationID {
		t.Fatal("handoff changed Product CA", err)
	}
	if _, err = env.store.GetManagedBrandCloud(ctx, ack.SourceUserID, ack.CloudID); !errors.Is(err, ErrNotFound) {
		t.Fatal("old owner retained management", err)
	}
	var jobs int
	if err = env.db.QueryRow(ctx, `SELECT count(*) FROM device_pki_outbox WHERE cloud_id=$1`, ack.CloudID).Scan(&jobs); err != nil || jobs != 2 {
		t.Fatal("handoff created replacement CA work", err)
	}
}

func TestDevicePKIRequeuePreservesBusinessAndCancelledScopes(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "requeue-pki")
	product, err := env.store.CreateDeviceItemProfileAsUser(ctx, authorizedProductInput(owner.User.ID, owner.BrandCloud.ID, "requeue"))
	if err != nil {
		t.Fatal(err)
	}
	rootID := "33d2b843-8de2-459f-bbf1-cc2791cf33bf"
	if _, err = env.db.Exec(ctx, `UPDATE device_pki_outbox SET status='ready',issuer_id=$2 WHERE cloud_id=$1;`, owner.BrandCloud.ID, rootID); err != nil {
		t.Fatal(err)
	}
	for _, apply := range []bool{false, true} {
		if _, err = env.store.RequeueDevicePKI(ctx, "production", owner.BrandCloud.ID, rootID, apply); !errors.Is(err, ErrConflict) {
			t.Fatal("production requeue allowed", err)
		}
	}
	preview, err := env.store.RequeueDevicePKI(ctx, "dev", owner.BrandCloud.ID, rootID, false)
	if err != nil || len(preview) != 2 {
		t.Fatal("wrong preview", err)
	}
	var pending int
	if err = env.db.QueryRow(ctx, `SELECT count(*) FROM device_pki_outbox WHERE status='pending'`).Scan(&pending); err != nil || pending != 0 {
		t.Fatal("preview changed jobs", err)
	}
	// A disabled Product must not be requeued with its active Cloud.
	if _, err = env.db.Exec(ctx, `UPDATE device_item_profiles SET status='disabled',disabled_at=now() WHERE id=$1`, product.ID); err != nil {
		t.Fatal(err)
	}
	changed, err := env.store.RequeueDevicePKI(ctx, "dev", owner.BrandCloud.ID, rootID, true)
	if err != nil || len(changed) != 1 || changed[0].ProductID != "" {
		t.Fatal("disabled Product requeued", err)
	}
	cloud, err := env.store.GetBrandCloud(ctx, owner.BrandCloud.ID)
	if err != nil || cloud.ID != owner.BrandCloud.ID || cloud.Name != owner.BrandCloud.Name || cloud.PKIStatus != "pending" || cloud.PKIIssuerID != "" || cloud.PKIOperationID == owner.BrandCloud.PKIOperationID {
		t.Fatal("Cloud readiness epoch incorrect", err)
	}
	again, err := env.store.RequeueDevicePKI(ctx, "dev", owner.BrandCloud.ID, rootID, true)
	if err != nil || len(again) != 0 {
		t.Fatal("requeue replaced pending work", err)
	}
	job, err := env.store.ClaimDevicePKI(ctx)
	if err != nil || job.OperationID != cloud.PKIOperationID {
		t.Fatal("new job not claimable", err)
	}
	var auditCount int
	if err = env.db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE event_type='nonproduction_device_pki_requeued' AND organization_id=$1`, owner.BrandCloud.ID).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatal("requeue audit missing", err)
	}
}

func TestAutomaticPKIOutboxAndReadiness(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "automatic-pki")
	if owner.BrandCloud.PKIStatus != "pending" || owner.BrandCloud.PKIOperationID == "" {
		t.Fatal("signup omitted PKI state")
	}
	product, err := env.store.CreateDeviceItemProfileAsUser(ctx, authorizedProductInput(owner.User.ID, owner.BrandCloud.ID, "automatic"))
	if err != nil {
		t.Fatal(err)
	}
	if product.PKIStatus != "pending" || product.PKIOperationID == "" {
		t.Fatal("Product omitted PKI state")
	}
	called := false
	_, _, err = env.store.IssueProductionRunAsUser(ctx, authorizedProductionInput(owner.User.ID, owner.BrandCloud.ID, product.ID), func(model.ProductionRun, model.DeviceItemProfile) (string, error) {
		called = true
		return "fixture", nil
	})
	if !errors.Is(err, ErrDevicePKINotReady) || called {
		t.Fatal("pending PKI signed authorization", err)
	}
	j, err := env.store.ClaimDevicePKI(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if j.OperationID != owner.BrandCloud.PKIOperationID || j.ProductID != "" {
		t.Fatal("Cloud job scope")
	}
	if err = env.store.FinishDevicePKI(ctx, j, DevicePKIReceipt{Status: "pending"}, "provider_unavailable"); err != nil {
		t.Fatal(err)
	}
	if _, err = env.db.Exec(ctx, `UPDATE device_pki_outbox SET available_at=now()-interval '1 minute' WHERE operation_id=$1`, j.OperationID); err != nil {
		t.Fatal(err)
	}
	again, err := env.store.ClaimDevicePKI(ctx)
	if err != nil || again.OperationID != j.OperationID || again.LeaseID == j.LeaseID {
		t.Fatal("retry identity", err)
	}
	if err = env.store.FinishDevicePKI(ctx, j, DevicePKIReceipt{Status: "pending"}, ""); !errors.Is(err, ErrConflict) {
		t.Fatal("stale lease completed", err)
	}
	r := DevicePKIReceipt{Status: "ready", OperationID: product.PKIOperationID, IssuerID: product.ID}
	if err = env.store.FinishDevicePKI(ctx, again, r, ""); err != nil {
		t.Fatal(err)
	}
	cloud, err := env.store.GetBrandCloud(ctx, owner.BrandCloud.ID)
	if err != nil || cloud.PKIStatus != "ready" || cloud.PKIIssuerID != product.ID {
		t.Fatal("ready state not published", err)
	}
	pj, err := env.store.ClaimDevicePKI(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = env.db.Exec(ctx, `UPDATE device_item_profiles SET status='disabled',disabled_at=now() WHERE id=$1`, product.ID); err != nil {
		t.Fatal(err)
	}
	if err = env.store.FinishDevicePKI(ctx, pj, r, ""); err != nil {
		t.Fatal(err)
	}
	p, err := env.store.GetDeviceItemProfile(ctx, owner.BrandCloud.ID, product.ID)
	if err != nil || p.PKIStatus != "cancelled" || p.PKIIssuerID != "" {
		t.Fatal("disabled Product resurrected", err)
	}
}
