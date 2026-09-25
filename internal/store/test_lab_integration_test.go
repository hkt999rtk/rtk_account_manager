package store

import (
	"context"
	"errors"
	"rtk_account_manager/internal/model"
	"strings"
	"testing"
	"time"
)

func TestTestLabBindingLifecycleIsolationAndRevocation(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "lab-owner")
	other := handoffDeveloper(t, env, "lab-other")
	p, err := env.store.CreateDeviceItemProfileAsUser(ctx, authorizedProductInput(owner.User.ID, owner.BrandCloud.ID, "lab"))
	if err != nil {
		t.Fatal(err)
	}
	readyDevicePKIFixture(t, env, p.ID)
	in := authorizedProductionInput(owner.User.ID, owner.BrandCloud.ID, p.ID)
	in.FactoryID = "developer-console"
	in.BatchID = "pki-test-fixture"
	run, _, err := env.store.IssueProductionRunAsUser(ctx, in, func(model.ProductionRun, model.DeviceItemProfile) (string, error) { return "fixture", nil })
	if err != nil {
		t.Fatal(err)
	}
	d, err := env.store.CreateDeviceAsUser(ctx, owner.User.ID, owner.BrandCloud.ID, DeviceInput{Name: "Lab camera", Category: p.Category, DeviceItemProfileID: &p.ID, Metadata: map[string]any{"purpose": "test"}})
	if err != nil {
		t.Fatal(err)
	}
	if stringValue(d.DeviceItemProfileID) != p.ID {
		t.Fatal("device creation lost Product association")
	}
	if _, err = env.store.CreateDeviceAsUser(ctx, other.User.ID, other.BrandCloud.ID, DeviceInput{Name: "Cross-cloud", Category: p.Category, DeviceItemProfileID: &p.ID}); err == nil {
		t.Fatal("cross-cloud Product creation admitted")
	}
	adm := FactoryEnrollmentAdmission{RunID: run.ID, CloudID: owner.BrandCloud.ID, ProductID: p.ID, RequestID: "lab-issuance", DeviceID: d.ID, RequestSHA256: strings.Repeat("a", 64)}
	res, err := env.store.ReserveFactoryEnrollment(ctx, adm)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = env.store.CompleteFactoryEnrollment(ctx, factoryResult(adm, res, "issued")); err != nil {
		t.Fatal(err)
	}
	account, err := env.store.ConsoleLabAccount(ctx, owner.User.ID, owner.BrandCloud.ID)
	if err != nil {
		t.Fatal(err)
	}
	act := func(action, hash string) error {
		return env.store.LabBindingAction(ctx, owner.User.ID, owner.BrandCloud.ID, p.ID, account.ID, d.ID, action, hash)
	}
	if err = act("bind", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing proof admitted: %v", err)
	}
	if err = env.store.LabBindingAction(ctx, other.User.ID, owner.BrandCloud.ID, p.ID, account.ID, d.ID, "grant", "foreign"); err == nil {
		t.Fatal("foreign developer admitted")
	}
	if err = act("grant", "proof1"); err != nil {
		t.Fatal(err)
	}
	if _, err = env.db.Exec(ctx, `UPDATE test_lab_bind_grants SET expires_at=now()-interval '1 second' WHERE token_hash='proof1'`); err != nil {
		t.Fatal(err)
	}
	if err = act("bind", "proof1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired proof admitted: %v", err)
	}
	if _, err = env.db.Exec(ctx, `UPDATE test_lab_bind_grants SET expires_at=now()+interval '2 minutes' WHERE token_hash='proof1'`); err != nil {
		t.Fatal(err)
	}
	if err = act("bind", "proof1"); err != nil {
		t.Fatal(err)
	}
	if err = act("bind", "proof1"); err != nil {
		t.Fatalf("bind replay: %v", err)
	}
	devices, err := env.store.ListLabDevices(ctx, owner.User.ID, owner.BrandCloud.ID, p.ID, account.ID, 25, 0)
	if err != nil || len(devices) != 1 || !devices[0].Bound {
		t.Fatalf("bound list: %+v %v", devices, err)
	}
	if _, err = env.store.CreateTestLabSession(ctx, owner.User.ID, owner.BrandCloud.ID, p.ID, d.ID, account.ID); err == nil {
		t.Fatal("unprovisioned device admitted")
	}
	op, err := env.store.ProvisionLabDevice(ctx, owner.User.ID, owner.BrandCloud.ID, p.ID, account.ID, d.ID, "lab-provision-op", "lab-activity", "fixture-public-key")
	if err != nil {
		t.Fatal(err)
	}
	if !op.Created {
		t.Fatal("provision did not enqueue")
	}
	again, err := env.store.ProvisionLabDevice(ctx, owner.User.ID, owner.BrandCloud.ID, p.ID, account.ID, d.ID, "lab-provision-op", "lab-activity", "fixture-public-key")
	if err != nil || again.Created {
		t.Fatalf("provision replay: %v", err)
	}
	if err = act("unbind", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("pending provision unbound: %v", err)
	}
	if _, err = env.db.Exec(ctx, `UPDATE devices SET metadata=metadata||'{"video_cloud_activation_status":"activated"}'::jsonb WHERE id=$1`, d.ID); err != nil {
		t.Fatal(err)
	}
	lease, err := env.store.CreateTestLabSession(ctx, owner.User.ID, owner.BrandCloud.ID, p.ID, d.ID, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if remaining := time.Until(lease.ExpiresAt); remaining < 10*time.Minute || remaining > 12*time.Minute {
		t.Fatalf("session expiry remaining = %s, want about 11 minutes", remaining)
	}
	if _, err = env.store.GetTestLabSession(ctx, lease.ID); err != nil {
		t.Fatal(err)
	}
	if err = act("unbind", ""); err != nil {
		t.Fatal(err)
	}
	if err = act("unbind", ""); err != nil {
		t.Fatalf("unbind replay: %v", err)
	}
	if _, err = env.store.GetTestLabSession(ctx, lease.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old authorization survived: %v", err)
	}
	if _, err = env.store.GetDevice(ctx, owner.BrandCloud.ID, d.ID); err != nil {
		t.Fatal("unbind deleted device")
	}
	if err = act("bind", "proof1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("consumed claim replayed after unbind: %v", err)
	}
	if err = act("grant", "proof2"); err != nil {
		t.Fatal(err)
	}
	if err = act("bind", "proof2"); err != nil {
		t.Fatal(err)
	}
	lease, err = env.store.CreateTestLabSession(ctx, owner.User.ID, owner.BrandCloud.ID, p.ID, d.ID, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = env.store.CloseLabAccount(ctx, owner.User.ID, owner.BrandCloud.ID, account.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = env.store.GetTestLabSession(ctx, lease.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("account revocation ignored: %v", err)
	}
	account, err = env.store.ConsoleLabAccount(ctx, owner.User.ID, owner.BrandCloud.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := env.store.CreateEndUser(ctx, EndUserCreateInput{Email: "lab-second@example.test", PasswordHash: "fixture-hash"})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an independently managed sharing binding. Unbind must not touch it.
	if _, err = env.db.Exec(ctx, `INSERT INTO device_user_bindings(device_id,brand_cloud_id,end_user_id,role) VALUES($1,$2,$3,'owner')`, d.ID, owner.BrandCloud.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	if err = act("unbind", ""); err != nil {
		t.Fatal(err)
	}
	var preserved bool
	if err = env.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM device_user_bindings WHERE device_id=$1 AND end_user_id=$2 AND disabled_at IS NULL)`, d.ID, second.ID).Scan(&preserved); err != nil || !preserved {
		t.Fatalf("other user's binding changed: %v", err)
	}
	if err = act("grant", "takeover"); !errors.Is(err, ErrConflict) {
		t.Fatalf("other user takeover admitted: %v", err)
	}
	operation := "77777777-7777-4777-8777-777777777777"
	if _, err = env.store.BeginLabDeviceRetirement(ctx, other.User.ID, owner.BrandCloud.ID, p.ID, d.ID, operation); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-cloud retirement admitted: %v", err)
	}
	retirement, err := env.store.BeginLabDeviceRetirement(ctx, owner.User.ID, owner.BrandCloud.ID, p.ID, d.ID, operation)
	if err != nil || retirement.Status != "pending" || retirement.OperationID != operation {
		t.Fatalf("retirement did not start: %+v %v", retirement, err)
	}
	var disabled bool
	if err = env.db.QueryRow(ctx, `SELECT disabled_at IS NOT NULL FROM device_user_bindings WHERE device_id=$1 AND end_user_id=$2`, d.ID, second.ID).Scan(&disabled); err != nil || !disabled {
		t.Fatalf("shared binding survived retirement: %v", err)
	}
	if err = act("grant", "after-retirement"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retired device admitted test access: %v", err)
	}
	devices, err = env.store.ListLabDevices(ctx, owner.User.ID, owner.BrandCloud.ID, p.ID, account.ID, 25, 0)
	if err != nil || len(devices) != 1 || devices[0].RetirementStatus != "pending" || devices[0].Bound || devices[0].Bindable {
		t.Fatalf("pending retirement disappeared from history: %+v %v", devices, err)
	}
	retirement, err = env.store.FinishLabDeviceRetirement(ctx, owner.User.ID, owner.BrandCloud.ID, p.ID, d.ID, operation, false)
	if err != nil || retirement.Status != "failed" {
		t.Fatalf("failed cross-service step hidden: %+v %v", retirement, err)
	}
	if _, err = env.store.BeginLabDeviceRetirement(ctx, owner.User.ID, owner.BrandCloud.ID, p.ID, d.ID, "88888888-8888-4888-8888-888888888888"); !errors.Is(err, ErrConflict) {
		t.Fatalf("different retirement operation reused prior request: %v", err)
	}
	retirement, err = env.store.BeginLabDeviceRetirement(ctx, owner.User.ID, owner.BrandCloud.ID, p.ID, d.ID, operation)
	if err != nil || retirement.Status != "pending" || retirement.OperationID != operation {
		t.Fatalf("retirement retry lost operation identity: %+v %v", retirement, err)
	}
	retirement, err = env.store.FinishLabDeviceRetirement(ctx, owner.User.ID, owner.BrandCloud.ID, p.ID, d.ID, operation, true)
	if err != nil || retirement.Status != "completed" {
		t.Fatalf("retirement did not complete: %+v %v", retirement, err)
	}
	retirement, err = env.store.BeginLabDeviceRetirement(ctx, owner.User.ID, owner.BrandCloud.ID, p.ID, d.ID, operation)
	if err != nil || retirement.Status != "completed" {
		t.Fatalf("completed retirement was not idempotent: %+v %v", retirement, err)
	}
}
