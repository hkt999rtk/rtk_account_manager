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

func authorizedProductionInput(actor, cloud, product string) ProductionRunCreateInput {
	now := time.Now().UTC()
	return ProductionRunCreateInput{ActorUserID: &actor, BrandCloudID: cloud, DeviceItemProfileID: product, AllowedQuantity: 10, ValidFrom: now.Add(-time.Minute), ValidUntil: now.Add(time.Hour)}
}

func TestProductionRunIssuanceRequiresCurrentProductAuthority(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "production-issue-owner")
	member := handoffDeveloper(t, env, "production-issue-member")
	operator := handoffDeveloper(t, env, "production-issue-operator")
	p, err := env.store.CreateDeviceItemProfileAsUser(ctx, authorizedProductInput(owner.User.ID, owner.BrandCloud.ID, "production-issue"))
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	issuer := func(run model.ProductionRun, profile model.DeviceItemProfile) (string, error) {
		calls++
		if profile.ID != p.ID || profile.ProfileKey != p.ProfileKey || run.BrandCloudID != owner.BrandCloud.ID || run.DeviceItemProfileID != profile.ID || run.AllowedQuantity != 10 {
			t.Fatal("signer received inconsistent run/Product snapshot")
		}
		return "isolated-production-token", nil
	}
	issue := func(actor string, platform bool) error {
		in := authorizedProductionInput(actor, owner.BrandCloud.ID, p.ID)
		in.PlatformOverride = platform
		run, token, err := env.store.IssueProductionRunAsUser(ctx, in, issuer)
		if err != nil && (run.ID != "" || token != "") {
			t.Fatal("failed issuance exposed run/token")
		}
		return err
	}
	deny := func(actor string, platform bool) {
		t.Helper()
		before := calls
		if err := issue(actor, platform); !errors.Is(err, ErrNotFound) {
			t.Fatalf("issue bypass: %v", err)
		}
		if calls != before {
			t.Fatal("unauthorized signer invocation")
		}
	}
	deny("", false)
	deny(member.User.ID, false)
	deny(operator.User.ID, true)
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin=true WHERE id=$1`, operator.User.ID); err != nil {
		t.Fatal(err)
	}
	deny(operator.User.ID, false)
	if err := issue(operator.User.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role) VALUES($1,$2,'member')`, owner.BrandCloud.ID, member.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `INSERT INTO role_assignments(role_id,actor_type,actor_id,scope_type,scope_id,organization_id) SELECT id,'user',$1,'product',$3::text,$2::uuid FROM roles WHERE name='product_editor'`, member.User.ID, owner.BrandCloud.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	deny(member.User.ID, false)
	if _, err := env.db.Exec(ctx, `INSERT INTO brand_cloud_product_admissions(organization_id,user_id,product_id,provenance,approved_by) VALUES($1,$2,$3,'owner_invitation',$4)`, owner.BrandCloud.ID, member.User.ID, p.ID, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if err := issue(member.User.ID, false); err != nil {
		t.Fatal("approved editor rejected", err)
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
	handoffRequest(t, env, owner, operator, "production-issue-fence")
	op, err := env.store.AcceptBrandCloudOwnerTransfer(ctx, operator.User.ID, "production-issue-fence", time.Now())
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
	if err := issue(owner.User.ID, false); err != nil {
		t.Fatal(err)
	}
}

func TestProductionRunIssuanceFailuresNeverReturnTokenOrCommitRun(t *testing.T) {
	for _, stage := range []string{"nil_issuer", "empty_token", "sign_error", "audit", "write", "commit", "disabled_product", "missing_product", "invalid_quantity", "canceled"} {
		t.Run(stage, func(t *testing.T) {
			env := newStoreIntegrationEnv(t)
			ctx := context.Background()
			owner := handoffDeveloper(t, env, "production-issue-failure")
			p, err := env.store.CreateDeviceItemProfileAsUser(ctx, authorizedProductInput(owner.User.ID, owner.BrandCloud.ID, "issue-failure"))
			if err != nil {
				t.Fatal(err)
			}
			in := authorizedProductionInput(owner.User.ID, owner.BrandCloud.ID, p.ID)
			calls := 0
			issuer := ProductionRunIssuer(func(model.ProductionRun, model.DeviceItemProfile) (string, error) {
				calls++
				return "sensitive-issued-token", nil
			})
			expected := ErrProductionRunSigning
			var setup, cleanup string
			switch stage {
			case "nil_issuer":
				issuer = nil
			case "empty_token":
				issuer = func(model.ProductionRun, model.DeviceItemProfile) (string, error) { calls++; return " ", nil }
			case "sign_error":
				issuer = func(model.ProductionRun, model.DeviceItemProfile) (string, error) {
					calls++
					return "partial-token", errors.New("private signing failure detail")
				}
			case "audit":
				setup = `ALTER TABLE audit_events ADD CONSTRAINT production_issue_failure_test CHECK(subject_type<>'factory_production_run') NOT VALID`
				cleanup = `ALTER TABLE audit_events DROP CONSTRAINT production_issue_failure_test`
			case "write":
				setup = `ALTER TABLE factory_production_runs ADD CONSTRAINT production_issue_failure_test CHECK(false) NOT VALID`
				cleanup = `ALTER TABLE factory_production_runs DROP CONSTRAINT production_issue_failure_test`
			case "commit":
				setup = `CREATE FUNCTION reject_production_issue_test() RETURNS TRIGGER LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'isolated issue rejection' USING ERRCODE='23514'; END $$;CREATE CONSTRAINT TRIGGER production_issue_failure_test AFTER INSERT ON factory_production_runs DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_production_issue_test()`
				cleanup = `DROP TRIGGER production_issue_failure_test ON factory_production_runs;DROP FUNCTION reject_production_issue_test()`
			case "disabled_product":
				setup = `UPDATE device_item_profiles SET status='disabled',disabled_at=now()`
				expected = ErrDeviceItemProfileDisabled
			case "missing_product":
				in.DeviceItemProfileID = owner.User.ID
				expected = ErrNotFound
			case "invalid_quantity":
				in.AllowedQuantity = 0
				expected = ErrConflict
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				expected = context.Canceled
			}
			if setup != "" {
				if _, err := env.db.Exec(ctx, setup); err != nil {
					t.Fatal(err)
				}
			}
			if cleanup != "" {
				defer env.db.Exec(context.Background(), cleanup)
			}
			run, token, err := env.store.IssueProductionRunAsUser(ctx, in, issuer)
			if run.ID != "" || token != "" {
				t.Fatal("failed issuance returned run or token")
			}
			if cleanup != "" {
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
					t.Fatalf("did not reach failure: %v", err)
				}
			} else if !errors.Is(err, expected) {
				t.Fatalf("wrong rejection: %v want %v", err, expected)
			}
			if stage == "sign_error" && strings.Contains(err.Error(), "private signing") {
				t.Fatal("signer diagnostic leaked")
			}
			wantCalls := 0
			if stage == "empty_token" || stage == "sign_error" || stage == "commit" {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatalf("signer calls %d want %d", calls, wantCalls)
			}
			var runs, audits int
			if err := env.db.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM factory_production_runs),(SELECT count(*) FROM audit_events WHERE subject_type='factory_production_run')`).Scan(&runs, &audits); err != nil || runs != 0 || audits != 0 {
				t.Fatalf("partial issuance: %d/%d %v", runs, audits, err)
			}
			if cleanup != "" {
				if _, err := env.db.Exec(ctx, cleanup); err != nil {
					t.Fatal(err)
				}
				run, token, err = env.store.IssueProductionRunAsUser(ctx, in, issuer)
				if err != nil || run.ID == "" || token == "" {
					t.Fatalf("retry failed: %v", err)
				}
			}
		})
	}
}
