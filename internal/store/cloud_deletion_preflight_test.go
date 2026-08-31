package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"rtk_account_manager/internal/billinghandoff"
	"rtk_account_manager/internal/model"
)

type deletionBillingFunc func(context.Context, billinghandoff.CloudDeletionScope) (billinghandoff.CloudDeletionPreflight, error)

func (f deletionBillingFunc) CloudDeletionPreflight(ctx context.Context, in billinghandoff.CloudDeletionScope) (billinghandoff.CloudDeletionPreflight, error) {
	return f(ctx, in)
}

type deletionResourceFunc func(context.Context, CloudDeletionResourceScope) (CloudDeletionResourceEvidence, error)

func (f deletionResourceFunc) ObserveCloudDeletion(ctx context.Context, in CloudDeletionResourceScope) (CloudDeletionResourceEvidence, error) {
	return f(ctx, in)
}
func syntheticDeletionBilling(_ context.Context, in billinghandoff.CloudDeletionScope) (billinghandoff.CloudDeletionPreflight, error) {
	now := time.Now()
	return billinghandoff.CloudDeletionPreflight{CloudDeletionScope: in, Eligible: true, Blockers: []string{}, Currency: "TWD", ObservedAt: now, ExpiresAt: now.Add(time.Minute)}, nil
}
func syntheticDeletionResources(_ context.Context, in CloudDeletionResourceScope) (CloudDeletionResourceEvidence, error) {
	now := time.Now()
	return CloudDeletionResourceEvidence{Scope: in, Complete: true, ReceiptID: "synthetic", EvidenceSHA256: strings.Repeat("a", 64), Blockers: []string{}, ObservedAt: now, ExpiresAt: now.Add(time.Minute)}, nil
}
func deletionBlocker(t *testing.T, view CloudDeletionPreflight, code string) model.CloudBlocker {
	t.Helper()
	for _, b := range view.Blockers {
		if b.Code == code {
			return b
		}
	}
	t.Fatalf("missing %s: %+v", code, view)
	return model.CloudBlocker{}
}

func TestDeletionPreflightResourceCountsAndNoSideEffects(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "delete-owner")
	cloud := owner.BrandCloud.ID
	missing, err := env.store.PreflightDeveloperBrandCloudDeletion(ctx, owner.User.ID, cloud)
	if err != nil || missing.Eligible {
		t.Fatalf("missing dependencies: %+v %v", missing, err)
	}
	deletionBlocker(t, missing, "evidence_unavailable")
	if err := env.store.ConfigureCloudDeletionPreflight(CloudDeletionPreflightOptions{Billing: deletionBillingFunc(syntheticDeletionBilling), Resources: map[string]CloudDeletionResourceObserver{"test_resources": deletionResourceFunc(syntheticDeletionResources)}}); err != nil {
		t.Fatal(err)
	}
	view, err := env.store.PreflightDeveloperBrandCloudDeletion(ctx, owner.User.ID, cloud)
	if err != nil || !view.Eligible || view.Blockers == nil {
		t.Fatalf("synthetic clean fixture: %+v %v", view, err)
	}
	var products string
	if err := env.db.QueryRow(ctx, `INSERT INTO device_item_profiles(brand_cloud_id,profile_key,display_name,status,category,ca_profile,issuer_profile) VALUES($1,'blocked','disabled is not deleted','disabled','generic','ca','issuer') RETURNING id::text`, cloud).Scan(&products); err != nil {
		t.Fatal(err)
	}
	device, err := env.store.CreateDevice(ctx, cloud, DeviceInput{Name: "disabled also blocks", Category: model.DeviceCategoryGeneric})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE devices SET disabled_at=now() WHERE id=$1`, device.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `INSERT INTO device_operations(operation_id,correlation_id,organization_id,device_id,operation_type,status) VALUES('delete-test-op','delete-test-op',$1,$2,'unprovision','dead_lettered')`, cloud, device.ID); err != nil {
		t.Fatal(err)
	}
	var auditBefore, auditAfter int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM audit_events`).Scan(&auditBefore); err != nil {
		t.Fatal(err)
	}
	view, err = env.store.PreflightDeveloperBrandCloudDeletion(ctx, owner.User.ID, cloud)
	if err != nil || view.Eligible {
		t.Fatalf("nonempty accepted: %+v %v", view, err)
	}
	for _, code := range []string{"products_present", "devices_present", "jobs_running"} {
		b := deletionBlocker(t, view, code)
		if b.Count == nil || *b.Count != 1 {
			t.Fatalf("count %s: %+v", code, b)
		}
	}
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM audit_events`).Scan(&auditAfter); err != nil || auditBefore != auditAfter {
		t.Fatalf("preflight wrote audit %d %d %v", auditBefore, auditAfter, err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE device_operations SET status='succeeded',completed_at=now() WHERE operation_id='delete-test-op'`); err != nil {
		t.Fatal(err)
	}
	view, err = env.store.PreflightDeveloperBrandCloudDeletion(ctx, owner.User.ID, cloud)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range view.Blockers {
		if b.Code == "jobs_running" {
			t.Fatal("completed history blocks as active work")
		}
	}
}

func TestDeletionPreflightRejectsScopeAndDependencyChanges(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "delete-scope-owner")
	other := handoffDeveloper(t, env, "delete-scope-other")
	for _, role := range []string{"admin", "member", "viewer"} {
		if _, err := env.db.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role,access_scope) VALUES($1,$2,$3,CASE WHEN $3='viewer' THEN '{"kind":"all_products"}'::jsonb ELSE NULL END) ON CONFLICT(organization_id,user_id) DO UPDATE SET role=EXCLUDED.role,access_scope=EXCLUDED.access_scope`, owner.BrandCloud.ID, other.User.ID, role); err != nil {
			t.Fatal(err)
		}
		if _, err := env.store.PreflightDeveloperBrandCloudDeletion(ctx, other.User.ID, owner.BrandCloud.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s sees billing preflight: %v", role, err)
		}
	}
	if err := env.store.ConfigureCloudDeletionPreflight(CloudDeletionPreflightOptions{Billing: deletionBillingFunc(syntheticDeletionBilling), Resources: map[string]CloudDeletionResourceObserver{"test_resources": deletionResourceFunc(func(ctx context.Context, in CloudDeletionResourceScope) (CloudDeletionResourceEvidence, error) {
		out, err := syntheticDeletionResources(ctx, in)
		_, mutationErr := env.db.Exec(ctx, `UPDATE organizations SET authorization_version=authorization_version+1 WHERE id=$1`, in.CloudID)
		if mutationErr != nil {
			return out, mutationErr
		}
		return out, err
	})}}); err != nil {
		t.Fatal(err)
	}
	result, err := env.store.PreflightDeveloperBrandCloudDeletion(ctx, owner.User.ID, owner.BrandCloud.ID)
	if err != nil || result.Eligible {
		t.Fatalf("stale authorization context: %+v %v", result, err)
	}
	deletionBlocker(t, result, "evidence_unavailable")
}

func TestDeletionPreflightFinancialAndMissingEvidence(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "delete-financial-owner")
	for _, tc := range []struct {
		name    string
		amount  int64
		failure bool
	}{{"negative", -1, false}, {"positive", 1, false}, {"outage", 0, true}} {
		s := New(env.db)
		if err := s.ConfigureCloudDeletionPreflight(CloudDeletionPreflightOptions{Billing: deletionBillingFunc(func(ctx context.Context, in billinghandoff.CloudDeletionScope) (billinghandoff.CloudDeletionPreflight, error) {
			out, _ := syntheticDeletionBilling(ctx, in)
			if tc.failure {
				return out, context.DeadlineExceeded
			}
			out.BalanceMinor = tc.amount
			out.Eligible = false
			out.Blockers = []string{"balance_nonzero", "payment_pending"}
			return out, nil
		}), Resources: map[string]CloudDeletionResourceObserver{"test_resources": deletionResourceFunc(syntheticDeletionResources)}}); err != nil {
			t.Fatal(err)
		}
		result, err := s.PreflightDeveloperBrandCloudDeletion(ctx, owner.User.ID, owner.BrandCloud.ID)
		if err != nil || result.Eligible {
			t.Fatalf("%s: %+v %v", tc.name, result, err)
		}
		if tc.failure {
			deletionBlocker(t, result, "evidence_unavailable")
		} else {
			b := deletionBlocker(t, result, "balance_nonzero")
			if b.BalanceMinor == nil || *b.BalanceMinor != tc.amount {
				t.Fatalf("lost amount: %+v", b)
			}
			deletionBlocker(t, result, "payment_pending")
		}
	}
	for _, alter := range []func(*CloudDeletionResourceEvidence){func(e *CloudDeletionResourceEvidence) { e.Complete = false }, func(e *CloudDeletionResourceEvidence) { e.Scope.OwnershipVersion++ }, func(e *CloudDeletionResourceEvidence) { e.ExpiresAt = time.Now().Add(-time.Second) }, func(e *CloudDeletionResourceEvidence) { e.EvidenceSHA256 = "" }, func(e *CloudDeletionResourceEvidence) { e.Blockers = []string{"unknown"} }} {
		s := New(env.db)
		err := s.ConfigureCloudDeletionPreflight(CloudDeletionPreflightOptions{Billing: deletionBillingFunc(syntheticDeletionBilling), Resources: map[string]CloudDeletionResourceObserver{"test_resources": deletionResourceFunc(func(ctx context.Context, in CloudDeletionResourceScope) (CloudDeletionResourceEvidence, error) {
			out, err := syntheticDeletionResources(ctx, in)
			alter(&out)
			return out, err
		})}})
		if err != nil {
			t.Fatal(err)
		}
		result, err := s.PreflightDeveloperBrandCloudDeletion(ctx, owner.User.ID, owner.BrandCloud.ID)
		if err != nil || result.Eligible {
			t.Fatalf("bad resource proof: %+v %v", result, err)
		}
		deletionBlocker(t, result, "evidence_unavailable")
	}
}

func TestDeletionPreflightRechecksResourcesAndOwnerAfterDependencyIO(t *testing.T) {
	for _, mutation := range []string{"new-product", "disabled-owner", "handoff"} {
		t.Run(mutation, func(t *testing.T) {
			env := newStoreIntegrationEnv(t)
			ctx := context.Background()
			owner := handoffDeveloper(t, env, "deletion-race-owner")
			target := handoffDeveloper(t, env, "deletion-race-target")
			if mutation == "handoff" {
				configureTestHandoff(t, env)
				handoffRequest(t, env, owner, target, "deletion-race-handoff")
			}
			if err := env.store.ConfigureCloudDeletionPreflight(CloudDeletionPreflightOptions{Billing: deletionBillingFunc(syntheticDeletionBilling), Resources: map[string]CloudDeletionResourceObserver{"test_resources": deletionResourceFunc(func(ctx context.Context, in CloudDeletionResourceScope) (CloudDeletionResourceEvidence, error) {
				evidence, err := syntheticDeletionResources(ctx, in)
				if err != nil {
					return evidence, err
				}
				switch mutation {
				case "new-product":
					_, err = env.db.Exec(ctx, `INSERT INTO device_item_profiles(brand_cloud_id,profile_key,display_name,category,ca_profile,issuer_profile) VALUES($1,'during-check','during-check','generic','ca','issuer')`, in.CloudID)
				case "disabled-owner":
					_, err = env.db.Exec(ctx, `UPDATE users SET disabled_at=now() WHERE id=$1`, in.OwnerUserID)
				case "handoff":
					_, err = env.store.AcceptBrandCloudOwnerTransfer(ctx, target.User.ID, "deletion-race-handoff", time.Now())
				}
				return evidence, err
			})}}); err != nil {
				t.Fatal(err)
			}
			result, err := env.store.PreflightDeveloperBrandCloudDeletion(ctx, owner.User.ID, owner.BrandCloud.ID)
			if mutation == "disabled-owner" {
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("revoked caller got financial data: %+v %v", result, err)
				}
				return
			}
			if err != nil || result.Eligible {
				t.Fatalf("changed state ready: %+v %v", result, err)
			}
			if mutation == "new-product" {
				deletionBlocker(t, result, "products_present")
			} else {
				deletionBlocker(t, result, "lifecycle_conflict")
			}
		})
	}
}
