package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"rtk_account_manager/internal/model"
)

func claimAuthorizationToken(t *testing.T, env storeIntegrationEnv, cloud, name string, product *string) model.DeviceClaimToken {
	t.Helper()
	token, err := env.store.CreateDeviceClaimToken(context.Background(), DeviceClaimTokenCreateInput{
		OrganizationID: &cloud, TokenHash: name, Category: model.DeviceCategoryIPCamera,
		DeviceItemProfileID: product, VideoCloudDevid: name, ActivityID: "activity", ClipPublicKey: "private-claim-material",
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestClaimAdmissionRequiresCurrentOwnerApprovedProductScope(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "claim-auth-owner")
	member := handoffDeveloper(t, env, "claim-auth-member")
	product, err := env.store.CreateDeviceItemProfile(ctx, DeviceItemProfileCreateInput{
		ActorUserID: &owner.User.ID, BrandCloudID: owner.BrandCloud.ID, ProfileKey: "claim-scope", DisplayName: "Claim Scope",
		Category: model.DeviceCategoryIPCamera, CAProfile: "ca", IssuerProfile: "issuer", ServiceOptions: []string{"mqtt"},
	})
	if err != nil {
		t.Fatal(err)
	}
	claimAuthorizationToken(t, env, owner.BrandCloud.ID, "claim-product-scope", &product.ID)
	resolve := func(actor string) (DeviceClaimResolveResult, error) {
		return env.store.ResolveDeviceClaimToken(ctx, DeviceClaimResolveInput{OrganizationID: owner.BrandCloud.ID, RequestedBy: actor, TokenHash: "claim-product-scope", DeviceName: "Device"})
	}
	deny := func(actor string) {
		t.Helper()
		if _, err := resolve(actor); !errors.Is(err, ErrNotFound) {
			t.Fatalf("claim bypass: %v", err)
		}
	}
	deny("")
	deny(member.User.ID)
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin=true WHERE id=$1`, member.User.ID); err != nil {
		t.Fatal(err)
	}
	deny(member.User.ID)
	if _, err := env.db.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role,access_scope) VALUES($1,$2,'viewer','{"kind":"all_products"}')`, owner.BrandCloud.ID, member.User.ID); err != nil {
		t.Fatal(err)
	}
	// Retain a write-capable ACL while testing the viewer ceiling. Membership
	// and Product admission are independently required even with this assignment.
	if _, err := env.db.Exec(ctx, `INSERT INTO role_assignments(role_id,actor_type,actor_id,scope_type,scope_id,organization_id)
		SELECT id,'user',$1,'organization',$2::text,$2::uuid FROM roles WHERE name='member'`, member.User.ID, owner.BrandCloud.ID); err != nil {
		t.Fatal(err)
	}
	deny(member.User.ID)
	if _, err := env.db.Exec(ctx, `UPDATE organization_members SET role='member',access_scope=NULL WHERE organization_id=$1 AND user_id=$2`, owner.BrandCloud.ID, member.User.ID); err != nil {
		t.Fatal(err)
	}
	// Role transition deliberately retires the former ACL. Model a new explicit
	// grant; never reactivate the old viewer-era assignment.
	if _, err := env.db.Exec(ctx, `INSERT INTO role_assignments(role_id,actor_type,actor_id,scope_type,scope_id,organization_id)
		SELECT id,'user',$1,'organization',$2::text,$2::uuid FROM roles WHERE name='member'`, member.User.ID, owner.BrandCloud.ID); err != nil {
		t.Fatal(err)
	}
	if allowed, err := env.store.HasPermission(ctx, member.User.ID, owner.BrandCloud.ID, "claim.resolve"); err != nil || !allowed {
		t.Fatalf("fixture lacks claim ACL: %t %v", allowed, err)
	}
	// An organization-level member ACL is not owner approval of this Product.
	deny(member.User.ID)
	if _, err := env.db.Exec(ctx, `INSERT INTO brand_cloud_product_admissions(organization_id,user_id,product_id,provenance,approved_by) VALUES($1,$2,$3,'owner_invitation',$4)`, owner.BrandCloud.ID, member.User.ID, product.ID, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	got, err := resolve(member.User.ID)
	if err != nil || got.Device.DeviceItemProfileID == nil || *got.Device.DeviceItemProfileID != product.ID {
		t.Fatalf("approved claim failed: %+v %v", got, err)
	}
	if _, err := env.db.Exec(ctx, `DELETE FROM brand_cloud_product_admissions WHERE organization_id=$1 AND user_id=$2`, owner.BrandCloud.ID, member.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.UnprovisionDevice(ctx, DeviceUnprovisionInput{OrganizationID: owner.BrandCloud.ID, DeviceID: got.Device.ID, ActorUserID: member.User.ID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked Product could unprovision: %v", err)
	}
	if _, err := env.store.UnprovisionDevice(ctx, DeviceUnprovisionInput{OrganizationID: owner.BrandCloud.ID, DeviceID: got.Device.ID, ActorUserID: owner.User.ID}); err != nil {
		t.Fatal(err)
	}
}

func TestClaimAndUnprovisionAuditFailurePreservesDeviceAndClaim(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "claim-audit-owner")
	token := claimAuthorizationToken(t, env, owner.BrandCloud.ID, "claim-audit-token", nil)
	input := DeviceClaimResolveInput{OrganizationID: owner.BrandCloud.ID, RequestedBy: owner.User.ID, TokenHash: "claim-audit-token", DeviceName: "Device"}
	if _, err := env.db.Exec(ctx, `ALTER TABLE audit_events ADD CONSTRAINT reject_claim_audit_test CHECK(event_type<>'device_claim_resolved') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	defer env.db.Exec(context.Background(), `ALTER TABLE audit_events DROP CONSTRAINT IF EXISTS reject_claim_audit_test`)
	if _, err := env.store.ResolveDeviceClaimToken(ctx, input); err == nil {
		t.Fatal("auditless claim accepted")
	}
	var devices, claims int
	if err := env.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM devices WHERE organization_id=$1),(SELECT count(*) FROM device_claims WHERE claim_token_id=$2)`, owner.BrandCloud.ID, token.ID).Scan(&devices, &claims); err != nil || devices != 0 || claims != 0 {
		t.Fatalf("partial claim: %d/%d %v", devices, claims, err)
	}
	fresh, err := env.store.GetDeviceClaimToken(ctx, token.ID)
	if err != nil || fresh.ClaimedAt != nil {
		t.Fatalf("failed claim consumed token: %+v %v", fresh, err)
	}
	if _, err := env.db.Exec(ctx, `ALTER TABLE audit_events DROP CONSTRAINT reject_claim_audit_test`); err != nil {
		t.Fatal(err)
	}
	resolved, err := env.store.ResolveDeviceClaimToken(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	var payload string
	if err := env.db.QueryRow(ctx, `SELECT payload::text FROM audit_events WHERE event_type='device_claim_resolved' AND subject_id=$1`, resolved.Claim.ID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, "private-claim-material") || strings.Contains(payload, "claim-audit-token") {
		t.Fatal("claim secrets leaked to audit")
	}
	if _, err := env.db.Exec(ctx, `ALTER TABLE audit_events ADD CONSTRAINT reject_unprovision_audit_test CHECK(event_type<>'device_unprovisioned') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	defer env.db.Exec(context.Background(), `ALTER TABLE audit_events DROP CONSTRAINT IF EXISTS reject_unprovision_audit_test`)
	remove := DeviceUnprovisionInput{OrganizationID: owner.BrandCloud.ID, ActorUserID: owner.User.ID, DeviceID: resolved.Device.ID}
	if _, err := env.store.UnprovisionDevice(ctx, remove); err == nil {
		t.Fatal("auditless unprovision accepted")
	}
	if _, err := env.store.GetDevice(ctx, owner.BrandCloud.ID, resolved.Device.ID); err != nil {
		t.Fatal("audit failure lost device", err)
	}
	var operations, outbox int
	if err := env.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM device_operations WHERE organization_id=$1),(SELECT count(*) FROM device_message_outbox)`, owner.BrandCloud.ID).Scan(&operations, &outbox); err != nil || operations != 0 || outbox != 0 {
		t.Fatalf("audit failure left queued work %d/%d %v", operations, outbox, err)
	}
	if _, err := env.db.Exec(ctx, `ALTER TABLE audit_events DROP CONSTRAINT reject_unprovision_audit_test`); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.UnprovisionDevice(ctx, remove); err != nil {
		t.Fatal(err)
	}
}

func TestPlatformUnprovisionRechecksPrivilegeAndCloudFence(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "override-auth-owner")
	operator := handoffDeveloper(t, env, "override-auth-operator")
	claimAuthorizationToken(t, env, owner.BrandCloud.ID, "override-claim", nil)
	claimed, err := env.store.ResolveDeviceClaimToken(ctx, DeviceClaimResolveInput{OrganizationID: owner.BrandCloud.ID, RequestedBy: owner.User.ID, TokenHash: "override-claim"})
	if err != nil {
		t.Fatal(err)
	}
	input := DeviceUnprovisionInput{DeviceID: claimed.Device.ID, ActorUserID: operator.User.ID, PlatformOverride: true, Reason: "support verified", Evidence: map[string]any{"ticket": "isolated-test"}}
	deny := func() {
		t.Helper()
		if _, err := env.store.UnprovisionDevice(ctx, input); !errors.Is(err, ErrNotFound) {
			t.Fatalf("platform bypass: %v", err)
		}
	}
	deny()
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin=true,disabled_at=now() WHERE id=$1`, operator.User.ID); err != nil {
		t.Fatal(err)
	}
	deny()
	if _, err := env.db.Exec(ctx, `UPDATE users SET disabled_at=NULL,signup_pending_verification=true WHERE id=$1`, operator.User.ID); err != nil {
		t.Fatal(err)
	}
	deny()
	if _, err := env.db.Exec(ctx, `UPDATE users SET signup_pending_verification=false WHERE id=$1`, operator.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE users SET signup_pending_verification=true WHERE id=$1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	deny()
	if _, err := env.db.Exec(ctx, `UPDATE users SET signup_pending_verification=false WHERE id=$1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	configureTestHandoff(t, env)
	handoffRequest(t, env, owner, operator, "override-transfer")
	transfer, err := env.store.AcceptBrandCloudOwnerTransfer(ctx, operator.User.ID, "override-transfer", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	deny()
	query := BrandCloudOwnerTransferQuery{BrandCloudID: owner.BrandCloud.ID, TransferID: transfer.ID, RequesterID: owner.User.ID}
	if _, err := env.store.CancelBrandCloudOwnerTransfer(ctx, query, time.Now()); err != nil {
		t.Fatal(err)
	}
	deny() // Canceling is still fenced until every participant releases its hold.
	for _, participant := range []string{"billing", "test_resources"} {
		if _, err := env.store.RecordCloudHandoffAbortAck(ctx, HandoffAbortAck{CloudID: owner.BrandCloud.ID, OperationID: transfer.ID, OwnershipVersion: 1, Participant: participant, ReceiptSHA256: strings.Repeat("a", 64)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.store.UnprovisionDevice(ctx, input); err != nil {
		t.Fatal("valid platform recovery failed", err)
	}
}

func TestEndUserClaimKeepsIdentitySeparateAndRollsBackBindingFailure(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "app-claim-owner")
	consumer, err := env.store.CreateEndUser(ctx, EndUserCreateInput{Email: "isolated-consumer@example.test", PasswordHash: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	token := claimAuthorizationToken(t, env, owner.BrandCloud.ID, "app-claim-atomic", nil)
	input := EndUserDeviceClaimResolveInput{EndUserID: consumer.ID, TokenHash: "app-claim-atomic", DeviceName: "App device"}
	if _, err := env.store.ResolveDeviceClaimToken(ctx, DeviceClaimResolveInput{RequestedBy: consumer.ID, OrganizationID: owner.BrandCloud.ID, TokenHash: input.TokenHash}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("end user became global human: %v", err)
	}
	wrong := input
	wrong.EndUserID = owner.User.ID
	if _, err := env.store.ResolveEndUserDeviceClaimToken(ctx, wrong); !errors.Is(err, ErrNotFound) {
		t.Fatalf("global human became end user: %v", err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE end_users SET disabled_at=now() WHERE id=$1`, consumer.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.ResolveEndUserDeviceClaimToken(ctx, input); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disabled end user claimed: %v", err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE end_users SET disabled_at=NULL WHERE id=$1`, consumer.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `ALTER TABLE device_user_bindings ADD CONSTRAINT reject_app_binding_test CHECK(false) NOT VALID`); err != nil {
		t.Fatal(err)
	}
	defer env.db.Exec(context.Background(), `ALTER TABLE device_user_bindings DROP CONSTRAINT IF EXISTS reject_app_binding_test`)
	if _, err := env.store.ResolveEndUserDeviceClaimToken(ctx, input); err == nil {
		t.Fatal("binding failure accepted")
	}
	fresh, err := env.store.GetDeviceClaimToken(ctx, token.ID)
	if err != nil || fresh.ClaimedAt != nil {
		t.Fatalf("binding failure consumed token: %+v %v", fresh, err)
	}
	var devices, links int
	if err := env.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM devices WHERE organization_id=$1),(SELECT count(*) FROM brand_cloud_end_users WHERE end_user_id=$2)`, owner.BrandCloud.ID, consumer.ID).Scan(&devices, &links); err != nil || devices != 0 || links != 0 {
		t.Fatalf("partial app claim %d/%d %v", devices, links, err)
	}
	if _, err := env.db.Exec(ctx, `ALTER TABLE device_user_bindings DROP CONSTRAINT reject_app_binding_test`); err != nil {
		t.Fatal(err)
	}
	result, err := env.store.ResolveEndUserDeviceClaimToken(ctx, input)
	if err != nil || result.DeviceBinding.EndUserID != consumer.ID {
		t.Fatalf("app retry: %+v %v", result, err)
	}
	other := handoffDeveloper(t, env, "app-other-product-owner")
	product, err := env.store.CreateDeviceItemProfile(ctx, DeviceItemProfileCreateInput{
		ActorUserID: &other.User.ID, BrandCloudID: other.BrandCloud.ID, ProfileKey: "other-app-product", DisplayName: "Other Product",
		Category: model.DeviceCategoryIPCamera, CAProfile: "ca", IssuerProfile: "issuer", ServiceOptions: []string{"mqtt"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Historical/trusted token material can be inconsistent. A Brand Cloud must
	// not claim a device under another cloud's Product, even on the App path.
	mixed := claimAuthorizationToken(t, env, owner.BrandCloud.ID, "app-mixed-product", &product.ID)
	input.TokenHash = "app-mixed-product"
	if _, err := env.store.ResolveEndUserDeviceClaimToken(ctx, input); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-cloud Product claimed by App: %v", err)
	}
	if got, err := env.store.GetDeviceClaimToken(ctx, mixed.ID); err != nil || got.ClaimedAt != nil {
		t.Fatalf("invalid Product consumed token: %+v %v", got, err)
	}
}
