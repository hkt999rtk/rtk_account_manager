package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"rtk_account_manager/internal/model"
)

func authorizedProductInput(actor, cloud, key string) DeviceItemProfileCreateInput {
	return DeviceItemProfileCreateInput{ActorUserID: &actor, BrandCloudID: cloud, ProfileKey: key, DisplayName: key, Category: model.DeviceCategoryIPCamera, CAProfile: "ca", IssuerProfile: "issuer", ServiceOptions: []string{"mqtt"}}
}

func productUserWrite(s *Store, ctx context.Context, action, actor, cloud, product string, platform bool) (model.DeviceItemProfile, error) {
	if action == "create" {
		in := authorizedProductInput(actor, cloud, "new-product")
		in.PlatformOverride = platform
		return s.CreateDeviceItemProfileAsUser(ctx, in)
	}
	if action == "disable" {
		return s.DisableDeviceItemProfileAsUser(ctx, cloud, product, actor, platform)
	}
	name := "Updated Product"
	return s.UpdateDeviceItemProfileAsUser(ctx, DeviceItemProfileUpdateInput{ActorUserID: &actor, BrandCloudID: cloud, ProfileID: product, PlatformOverride: platform, DisplayName: &name})
}

func TestProductWritesRequireCurrentAuthorityAndApprovedScope(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "product-authority-owner")
	member := handoffDeveloper(t, env, "product-authority-member")
	operator := handoffDeveloper(t, env, "product-authority-operator")
	p, err := env.store.CreateDeviceItemProfileAsUser(ctx, authorizedProductInput(owner.User.ID, owner.BrandCloud.ID, "approved-product"))
	if err != nil {
		t.Fatal(err)
	}
	deny := func(actor string, platform bool) {
		t.Helper()
		for _, action := range []string{"create", "update", "disable"} {
			if _, err := productUserWrite(env.store, ctx, action, actor, owner.BrandCloud.ID, p.ID, platform); !errors.Is(err, ErrNotFound) {
				t.Fatalf("%s authorization bypass: %v", action, err)
			}
		}
	}
	deny("", false)
	deny(member.User.ID, false)
	deny(operator.User.ID, true)
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin=true WHERE id=$1`, operator.User.ID); err != nil {
		t.Fatal(err)
	}
	deny(operator.User.ID, false)
	if _, err := env.store.UpdateDeviceItemProfileAsUser(ctx, DeviceItemProfileUpdateInput{ActorUserID: &operator.User.ID, PlatformOverride: true, BrandCloudID: owner.BrandCloud.ID, ProfileID: p.ID}); err != nil {
		t.Fatal("explicit platform route failed", err)
	}
	if _, err := env.db.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role) VALUES($1,$2,'admin');`, owner.BrandCloud.ID, member.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `INSERT INTO role_assignments(role_id,actor_type,actor_id,scope_type,scope_id,organization_id) SELECT id,'user',$1,'organization',$2::text,$2::uuid FROM roles WHERE name='admin' ON CONFLICT DO NOTHING`, member.User.ID, owner.BrandCloud.ID); err != nil {
		t.Fatal(err)
	}
	if allowed, err := env.store.HasPermission(ctx, member.User.ID, owner.BrandCloud.ID, "registry_device.manage"); err != nil || !allowed {
		t.Fatalf("expected broad ACL fixture: %v %v", allowed, err)
	}
	deny(member.User.ID, false) // A broad ACL cannot invent Product admission.
	if _, err := env.db.Exec(ctx, `INSERT INTO brand_cloud_product_admissions(organization_id,user_id,product_id,provenance,approved_by) VALUES($1,$2,$3,'owner_invitation',$4)`, owner.BrandCloud.ID, member.User.ID, p.ID, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := productUserWrite(env.store, ctx, "update", member.User.ID, owner.BrandCloud.ID, p.ID, false); err != nil {
		t.Fatal("approved Product edit denied", err)
	}
	if _, err := productUserWrite(env.store, ctx, "create", member.User.ID, owner.BrandCloud.ID, p.ID, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Product grant created another Product: %v", err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE organization_members SET role='viewer',access_scope='{"kind":"all_products"}' WHERE organization_id=$1 AND user_id=$2`, owner.BrandCloud.ID, member.User.ID); err != nil {
		t.Fatal(err)
	}
	deny(member.User.ID, false)
	if _, err := env.db.Exec(ctx, `UPDATE users SET signup_pending_verification=true WHERE id=$1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	deny(owner.User.ID, false)
	deny(operator.User.ID, true)
	if _, err := env.db.Exec(ctx, `UPDATE users SET signup_pending_verification=false WHERE id=$1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	configureTestHandoff(t, env)
	handoffRequest(t, env, owner, operator, "product-authority-fence")
	op, err := env.store.AcceptBrandCloudOwnerTransfer(ctx, operator.User.ID, "product-authority-fence", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	deny(owner.User.ID, false)
	deny(operator.User.ID, true)
	if _, err := env.store.CancelBrandCloudOwnerTransfer(ctx, BrandCloudOwnerTransferQuery{BrandCloudID: owner.BrandCloud.ID, TransferID: op.ID, RequesterID: owner.User.ID}, time.Now()); err != nil {
		t.Fatal(err)
	}
	deny(owner.User.ID, false)
	deny(operator.User.ID, true)
	for _, participant := range []string{"billing", "test_resources"} {
		if _, err := env.store.RecordCloudHandoffAbortAck(ctx, HandoffAbortAck{CloudID: owner.BrandCloud.ID, OperationID: op.ID, OwnershipVersion: 1, Participant: participant, ReceiptSHA256: strings.Repeat("a", 64)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, action := range []string{"create", "update", "disable"} {
		if _, err := productUserWrite(env.store, ctx, action, owner.User.ID, owner.BrandCloud.ID, p.ID, false); err != nil {
			t.Fatalf("%s after release: %v", action, err)
		}
	}
}

func TestProductMutationAuditAndCommitFailuresRollBack(t *testing.T) {
	for _, action := range []string{"create", "update", "disable"} {
		for _, stage := range []string{"audit", "commit"} {
			t.Run(action+"/"+stage, func(t *testing.T) {
				env := newStoreIntegrationEnv(t)
				ctx := context.Background()
				owner := handoffDeveloper(t, env, "product-rollback")
				p, err := env.store.CreateDeviceItemProfileAsUser(ctx, authorizedProductInput(owner.User.ID, owner.BrandCloud.ID, "original-product"))
				if err != nil {
					t.Fatal(err)
				}
				setup := `ALTER TABLE audit_events ADD CONSTRAINT product_audit_failure_test CHECK(subject_type<>'device_item_profile') NOT VALID`
				cleanup := `ALTER TABLE audit_events DROP CONSTRAINT product_audit_failure_test`
				if stage == "commit" {
					setup = `CREATE FUNCTION reject_product_commit_test() RETURNS TRIGGER LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'isolated Product commit rejection' USING ERRCODE='23514'; END $$;CREATE CONSTRAINT TRIGGER product_commit_failure_test AFTER INSERT OR UPDATE ON device_item_profiles DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_product_commit_test()`
					cleanup = `DROP TRIGGER product_commit_failure_test ON device_item_profiles;DROP FUNCTION reject_product_commit_test()`
				}
				if _, err := env.db.Exec(ctx, setup); err != nil {
					t.Fatal(err)
				}
				defer env.db.Exec(context.Background(), cleanup)
				_, err = productUserWrite(env.store, ctx, action, owner.User.ID, owner.BrandCloud.ID, p.ID, false)
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
					t.Fatalf("did not reach failure: %v", err)
				}
				got, err := env.store.GetDeviceItemProfile(ctx, owner.BrandCloud.ID, p.ID)
				if err != nil || got.DisplayName != p.DisplayName || got.Status != p.Status {
					t.Fatalf("partial Product: %+v %v", got, err)
				}
				var products, grants, audits int
				if err := env.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM device_item_profiles),(SELECT count(*) FROM role_assignments WHERE scope_type='product'),(SELECT count(*) FROM audit_events WHERE subject_type='device_item_profile')`).Scan(&products, &grants, &audits); err != nil || products != 1 || grants != 1 || audits != 1 {
					t.Fatalf("partial create/grant/audit: %d/%d/%d %v", products, grants, audits, err)
				}
				if _, err := env.db.Exec(ctx, cleanup); err != nil {
					t.Fatal(err)
				}
				if _, err := productUserWrite(env.store, ctx, action, owner.User.ID, owner.BrandCloud.ID, p.ID, false); err != nil {
					t.Fatal("retry failed", err)
				}
			})
		}
	}
}

func TestConcurrentProductPatchesPreserveDisjointFields(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	owner := handoffDeveloper(t, env, "product-patch-owner")
	operator := handoffDeveloper(t, env, "product-patch-operator")
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin=true WHERE id=$1`, operator.User.ID); err != nil {
		t.Fatal(err)
	}
	p, err := env.store.CreateDeviceItemProfileAsUser(ctx, authorizedProductInput(owner.User.ID, owner.BrandCloud.ID, "concurrent-product"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := env.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `SELECT 1 FROM organizations WHERE id=$1 FOR UPDATE`, owner.BrandCloud.ID); err != nil {
		t.Fatal(err)
	}
	blockerPID := transactionBackendPID(t, ctx, tx)
	name, modelName := "New name", "New model"
	result := make(chan error, 2)
	go func() {
		_, err := env.store.UpdateDeviceItemProfileAsUser(ctx, DeviceItemProfileUpdateInput{ActorUserID: &owner.User.ID, BrandCloudID: owner.BrandCloud.ID, ProfileID: p.ID, DisplayName: &name})
		result <- err
	}()
	go func() {
		_, err := env.store.UpdateDeviceItemProfileAsUser(ctx, DeviceItemProfileUpdateInput{ActorUserID: &operator.User.ID, PlatformOverride: true, BrandCloudID: owner.BrandCloud.ID, ProfileID: p.ID, Model: &modelName})
		result <- err
	}()
	awaitBlockedConnections(t, ctx, env.db, blockerPID, 2, result)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case err := <-result:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	got, err := env.store.GetDeviceItemProfile(ctx, owner.BrandCloud.ID, p.ID)
	if err != nil || got.DisplayName != name || got.Model == nil || *got.Model != modelName {
		t.Fatalf("lost concurrent patch: %+v %v", got, err)
	}
}
