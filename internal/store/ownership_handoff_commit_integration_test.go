package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"rtk_account_manager/internal/billinghandoff"
)

// These receipts are synthetic. Real Billing transport is checked separately;
// no fixture boolean is wired into production preparation/settlement.
type commitTestBilling struct {
	*balanceTestBilling
	grantMu                 sync.Mutex
	grant                   *billinghandoff.Authorization
	loseGrant, loseFinalize bool
	onAuthorize             func()
}

func (b *commitTestBilling) AuthorizeCommit(_ context.Context, in billinghandoff.Binding, id string, version int64) (billinghandoff.Authorization, error) {
	b.grantMu.Lock()
	defer b.grantMu.Unlock()
	if b.onAuthorize != nil {
		b.onAuthorize()
	}
	b.mu.Lock()
	valid := b.version == version && b.amount >= 0 && b.confirmed[in.SourceUserID] && b.confirmed[in.TargetUserID]
	b.mu.Unlock()
	if !valid {
		return billinghandoff.Authorization{}, billinghandoff.ErrUnavailable
	}
	if b.grant == nil {
		b.grant = &billinghandoff.Authorization{OperationID: in.OperationID, AuthorizationID: id, SnapshotVersion: version, CreatedAt: time.Now().UTC()}
	}
	if b.grant.AuthorizationID != id {
		return billinghandoff.Authorization{}, billinghandoff.ErrInvalid
	}
	if b.loseGrant {
		b.loseGrant = false
		return billinghandoff.Authorization{}, billinghandoff.ErrUnavailable
	}
	return *b.grant, nil
}
func (b *commitTestBilling) Finalize(_ context.Context, in billinghandoff.Binding, id string, _ time.Time, _ string) (billinghandoff.ProtocolAck, error) {
	b.grantMu.Lock()
	defer b.grantMu.Unlock()
	if b.grant == nil || b.grant.AuthorizationID != id {
		return billinghandoff.ProtocolAck{}, billinghandoff.ErrInvalid
	}
	if b.loseFinalize {
		b.loseFinalize = false
		return billinghandoff.ProtocolAck{}, billinghandoff.ErrUnavailable
	}
	return billinghandoff.ProtocolAck{OperationID: in.OperationID, Phase: "finalized"}, nil
}
func readyCommitFixture(t *testing.T, env storeIntegrationEnv) (*commitTestBilling, HandoffPrepareAck, BrandCloudOwnerTransferQuery) {
	t.Helper()
	b, ack, q := readyBalanceFixture(t, env, 0)
	view, err := env.store.PreviewOwnerHandoff(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	q.RequesterID = ack.SourceUserID
	confirmBalance(t, env, q, *view.BalanceSnapshot, "source")
	q.RequesterID = ack.TargetUserID
	confirmBalance(t, env, q, *view.BalanceSnapshot, "target")
	remote := &commitTestBilling{balanceTestBilling: b}
	if err := env.store.ConfigureHandoffBilling(remote); err != nil {
		t.Fatal(err)
	}
	return remote, ack, q
}
func assertHandoffOwner(t *testing.T, env storeIntegrationEnv, ack HandoffPrepareAck, owner, phase string, version int64) {
	t.Helper()
	var actualOwner, actualPhase string
	var actualVersion int64
	if err := env.db.QueryRow(context.Background(), `SELECT m.user_id::text,h.phase,o.ownership_version FROM cloud_ownership_handoffs h
		JOIN organizations o ON o.id=h.brand_cloud_id JOIN organization_members m ON m.organization_id=o.id AND m.role='owner' WHERE h.id=$1`, ack.OperationID).Scan(&actualOwner, &actualPhase, &actualVersion); err != nil || actualOwner != owner || actualPhase != phase || actualVersion != version {
		t.Fatalf("owner/phase/version %s %s %d, want %s %s %d: %v", actualOwner, actualPhase, actualVersion, owner, phase, version, err)
	}
}

func TestHandoffCommitRevokesSourceAndFinalizationReleasesOnlyAfterAllAcks(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	remote, ack, q := readyCommitFixture(t, env)
	otherCloud, err := env.store.CreateDeveloperBrandCloud(ctx, ack.SourceUserID, BrandCloudInput{Name: "Unrelated source cloud"})
	if err != nil {
		t.Fatal(err)
	}
	collaborator := handoffDeveloper(t, env, "retained-collaborator")
	// Existing Product/ACL fixtures, inserted directly without claiming producer coverage.
	var product string
	if err := env.db.QueryRow(ctx, `INSERT INTO device_item_profiles(brand_cloud_id,profile_key,display_name,category,ca_profile,issuer_profile)
		VALUES($1,'handoff','Handoff','ip_camera','ca','issuer') RETURNING id::text`, ack.CloudID).Scan(&product); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role) VALUES($1,$2,'member')`, ack.CloudID, collaborator.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `INSERT INTO brand_cloud_product_admissions(organization_id,user_id,product_id,provenance,approved_by) VALUES($1,$2,$3,'owner_invitation',$4)`, ack.CloudID, collaborator.User.ID, product, ack.SourceUserID); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ actor, role string }{{ack.SourceUserID, "product_owner"}, {collaborator.User.ID, "product_editor"}} {
		if _, err := env.db.Exec(ctx, `INSERT INTO role_assignments(role_id,actor_type,actor_id,scope_type,scope_id,organization_id,created_by)
			SELECT id,'user',$1,'product',$2,$3,$4 FROM roles WHERE name=$5`, row.actor, product, ack.CloudID, ack.SourceUserID, row.role); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.db.Exec(ctx, `INSERT INTO role_assignments(role_id,actor_type,actor_id,scope_type,scope_id,organization_id,created_by)
		SELECT id,'service_account','source-service','organization',$1::text,$1::uuid,$2 FROM roles WHERE name='owner'`, ack.CloudID, ack.SourceUserID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `INSERT INTO product_collaborator_invitations(brand_cloud_id,product_id,invited_by_user_id,target_user_id,target_email,role,token_hash,expires_at)
		VALUES($1,$2,$3,$4,'prepare-target@example.test','product_viewer','pending-at-commit',now()+interval '1 hour')`, ack.CloudID, product, ack.SourceUserID, ack.TargetUserID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `INSERT INTO device_claim_tokens(organization_id,token_hash,category,video_cloud_devid,activity_id,clip_public_key,expires_at,created_by)
		VALUES($1,'source-claim','ip_camera','device','activity','key',now()+interval '1 hour',$2)`, ack.CloudID, ack.SourceUserID); err != nil {
		t.Fatal(err)
	}
	decision, err := env.store.CommitOwnerHandoff(ctx, ack.CloudID, ack.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	assertHandoffOwner(t, env, ack, ack.TargetUserID, "finalizing", 2)
	var unrelatedAllowed bool
	if err := env.db.QueryRow(ctx, `SELECT user_can_access_brand_cloud($1,$2)`, ack.SourceUserID, otherCloud.ID).Scan(&unrelatedAllowed); err != nil || !unrelatedAllowed {
		t.Fatal("handoff revoked unrelated cloud", err)
	}
	page, err := env.store.ListManagedBrandClouds(ctx, ack.TargetUserID, "all", 100, 0)
	if err != nil || page.OwnedCount != 2 || page.ReservedCount != 0 {
		t.Fatalf("quota not consumed: %+v %v", page, err)
	}
	var sourceACL, serviceACL, collabACL, targetProductOwner, invites, claims int
	if err := env.db.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM role_assignments WHERE organization_id=$1 AND actor_type='user' AND actor_id=$2 AND disabled_at IS NULL),
		(SELECT count(*) FROM role_assignments WHERE organization_id=$1 AND actor_type='service_account' AND created_by=$2::uuid AND disabled_at IS NULL),
		(SELECT count(*) FROM role_assignments WHERE organization_id=$1 AND actor_id=$3 AND disabled_at IS NULL),
		(SELECT count(*) FROM role_assignments a JOIN roles r ON r.id=a.role_id WHERE a.organization_id=$1 AND a.actor_id=$4 AND r.name='product_owner' AND a.disabled_at IS NULL),
		(SELECT count(*) FROM product_collaborator_invitations WHERE brand_cloud_id=$1 AND status='pending'),
		(SELECT count(*) FROM device_claim_tokens WHERE organization_id=$1 AND revoked_at IS NULL)`, ack.CloudID, ack.SourceUserID, collaborator.User.ID, ack.TargetUserID).Scan(&sourceACL, &serviceACL, &collabACL, &targetProductOwner, &invites, &claims); err != nil || sourceACL != 0 || serviceACL != 0 || collabACL != 1 || targetProductOwner != 1 || invites != 0 || claims != 0 {
		t.Fatalf("revocation/duties %d %d %d %d %d %d %v", sourceACL, serviceACL, collabACL, targetProductOwner, invites, claims, err)
	}
	q.RequesterID = ack.SourceUserID
	if _, err := env.store.CancelBrandCloudOwnerTransfer(ctx, q, time.Now()); !errors.Is(err, ErrConflict) {
		t.Fatalf("committed cancellation: %v", err)
	}
	if _, err := env.store.CommitOwnerHandoff(ctx, ack.OperationID, ack.CloudID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong cloud commit: %v", err)
	}
	restarted := New(env.db)
	if replay, err := restarted.CommitOwnerHandoff(ctx, ack.CloudID, ack.OperationID); err != nil || replay.DecisionSHA256 != decision.DecisionSHA256 {
		t.Fatalf("durable replay: %+v %v", replay, err)
	}
	release := HandoffFinalizationAck{CloudID: ack.CloudID, OperationID: ack.OperationID, OwnershipVersion: 1, DecisionSHA256: decision.DecisionSHA256, Participant: "test_resources", ReceiptSHA256: strings.Repeat("c", 64)}
	if _, err := restarted.RecordHandoffFinalizationAck(ctx, release); !errors.Is(err, ErrConflict) {
		t.Fatalf("release before Billing: %v", err)
	}
	remote.loseFinalize = true
	if err := restarted.ConfigureHandoffBilling(remote); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.FinalizeOwnerHandoff(ctx, ack.CloudID, ack.OperationID); !errors.Is(err, ErrHandoffUnavailable) {
		t.Fatalf("lost finalize reply: %v", err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE users SET disabled_at=now() WHERE id=$1`, ack.SourceUserID); err != nil {
		t.Fatal(err)
	}
	if phase, err := restarted.FinalizeOwnerHandoff(ctx, ack.CloudID, ack.OperationID); err != nil || phase != "finalizing" {
		t.Fatalf("forward recovery: %s %v", phase, err)
	}
	var accessible bool
	if err := env.db.QueryRow(ctx, `SELECT user_can_access_brand_cloud($1,$2)`, ack.TargetUserID, ack.CloudID).Scan(&accessible); err != nil || accessible {
		t.Fatal("Billing ack alone released producer fence", err)
	}
	if phase, err := restarted.RecordHandoffFinalizationAck(ctx, release); err != nil || phase != "succeeded" {
		t.Fatalf("complete release: %s %v", phase, err)
	}
	assertHandoffOwner(t, env, ack, ack.TargetUserID, "succeeded", 2)
	if err := env.db.QueryRow(ctx, `SELECT user_can_access_brand_cloud($1,$2)`, ack.TargetUserID, ack.CloudID).Scan(&accessible); err != nil || !accessible {
		t.Fatal("new owner inaccessible after all acks", err)
	}
	if role, err := env.store.GetUserProductCollaboratorRole(ctx, collaborator.User.ID, ack.CloudID, product); err != nil || role != ProductEditorRole {
		t.Fatalf("collaborator lost scope: %s %v", role, err)
	}
	q.RequesterID = ack.TargetUserID
	if view, err := restarted.GetOwnerHandoffStatus(ctx, q); err != nil || view.Phase != "succeeded" || !*view.SourceConfirmed || !*view.TargetConfirmed {
		t.Fatalf("final status lost consent: %+v %v", view, err)
	}
	for _, sql := range []string{`DELETE FROM cloud_handoff_committed_decisions WHERE operation_id=$1`, `UPDATE cloud_handoff_commit_requests SET billing_snapshot_version=99 WHERE operation_id=$1`, `UPDATE cloud_ownership_handoffs SET phase='canceled',version=version+1 WHERE id=$1`} {
		if _, err := env.db.Exec(ctx, sql, ack.OperationID); err == nil {
			t.Fatal("committed history/phase rewritten")
		}
	}
}

func TestHandoffCommitLostGrantAndAuditFailureRetrySameDecision(t *testing.T) {
	for _, failure := range []string{"grant_reply", "commit_audit"} {
		t.Run(failure, func(t *testing.T) {
			env := newStoreIntegrationEnv(t)
			ctx := context.Background()
			remote, ack, _ := readyCommitFixture(t, env)
			if failure == "grant_reply" {
				remote.loseGrant = true
			} else {
				if _, err := env.db.Exec(ctx, `ALTER TABLE audit_events ADD CONSTRAINT reject_owner_commit CHECK(event_type<>'brand_cloud_owner_transfer_committed') NOT VALID`); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					_, _ = env.db.Exec(context.Background(), `ALTER TABLE audit_events DROP CONSTRAINT IF EXISTS reject_owner_commit`)
				})
			}
			if _, err := env.store.CommitOwnerHandoff(ctx, ack.CloudID, ack.OperationID); err == nil {
				t.Fatal("injected failure ignored")
			}
			assertHandoffOwner(t, env, ack, ack.SourceUserID, "committing", 1)
			if failure == "commit_audit" {
				if _, err := env.db.Exec(ctx, `ALTER TABLE audit_events DROP CONSTRAINT reject_owner_commit`); err != nil {
					t.Fatal(err)
				}
			}
			var firstAuthorization string
			if err := env.db.QueryRow(ctx, `SELECT authorization_id::text FROM cloud_handoff_commit_requests WHERE operation_id=$1`, ack.OperationID).Scan(&firstAuthorization); err != nil {
				t.Fatal(err)
			}
			decision, err := env.store.CommitOwnerHandoff(ctx, ack.CloudID, ack.OperationID)
			if err != nil || decision.AuthorizationID != firstAuthorization {
				t.Fatalf("retry changed grant: %+v %v", decision, err)
			}
			assertHandoffOwner(t, env, ack, ack.TargetUserID, "finalizing", 2)
		})
	}
}

func TestHandoffCommitRechecksCancellationEligibilityAndQuotaAfterGrant(t *testing.T) {
	for _, change := range []string{"cancel", "disabled", "quota", "expired"} {
		t.Run(change, func(t *testing.T) {
			env := newStoreIntegrationEnv(t)
			ctx := context.Background()
			remote, ack, q := readyCommitFixture(t, env)
			remote.onAuthorize = func() {
				var err error
				switch change {
				case "cancel":
					q.RequesterID = ack.SourceUserID
					_, err = env.store.CancelBrandCloudOwnerTransfer(ctx, q, time.Now())
				case "disabled":
					_, err = env.db.Exec(ctx, `UPDATE users SET disabled_at=now() WHERE id=$1`, ack.TargetUserID)
				case "quota":
					_, err = env.db.Exec(ctx, `UPDATE users SET developer_cloud_limit=1 WHERE id=$1`, ack.TargetUserID)
				case "expired":
					_, err = env.db.Exec(ctx, `UPDATE brand_cloud_owner_transfers SET expires_at=now()-interval '1 second' WHERE id=$1`, ack.OperationID)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := env.store.CommitOwnerHandoff(ctx, ack.CloudID, ack.OperationID); err == nil {
				t.Fatal("ineligible owner committed")
			}
			phase := "committing"
			if change == "cancel" {
				phase = "canceling"
			}
			assertHandoffOwner(t, env, ack, ack.SourceUserID, phase, 1)
			var count int
			if err := env.db.QueryRow(ctx, `SELECT count(*) FROM cloud_handoff_committed_decisions`).Scan(&count); err != nil || count != 0 {
				t.Fatal("ineligible decision persisted", err)
			}
		})
	}
}

func TestHandoffConcurrentCommitIsOneOwnerDecision(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	_, ack, _ := readyCommitFixture(t, env)
	results := make(chan HandoffCommittedDecision, 4)
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() {
			d, e := env.store.CommitOwnerHandoff(ctx, ack.CloudID, ack.OperationID)
			results <- d
			errs <- e
		}()
	}
	var digest string
	for i := 0; i < 4; i++ {
		d := <-results
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		if digest != "" && d.DecisionSHA256 != digest {
			t.Fatal("concurrent commits differ")
		}
		digest = d.DecisionSHA256
	}
	assertHandoffOwner(t, env, ack, ack.TargetUserID, "finalizing", 2)
	var events int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE event_type='brand_cloud_owner_transfer_committed'`).Scan(&events); err != nil || events != 1 {
		t.Fatalf("duplicate owner commit: %d %v", events, err)
	}
}

func TestHandoffCommitCannotSkipPreparationConsentOrLatestBalance(t *testing.T) {
	for _, mode := range []string{"no_consent", "negative_after_consent", "sql_skip"} {
		t.Run(mode, func(t *testing.T) {
			env := newStoreIntegrationEnv(t)
			ctx := context.Background()
			b, ack, _ := readyBalanceFixture(t, env, 0)
			remote := &commitTestBilling{balanceTestBilling: b}
			if err := env.store.ConfigureHandoffBilling(remote); err != nil {
				t.Fatal(err)
			}
			if mode == "sql_skip" {
				for _, phase := range []string{"committing", "finalizing", "succeeded"} {
					if _, err := env.db.Exec(ctx, `UPDATE cloud_ownership_handoffs SET phase=$2,version=version+1 WHERE id=$1`, ack.OperationID, phase); err == nil {
						t.Fatal("direct SQL skipped protocol", phase)
					}
				}
				return
			}
			if mode == "negative_after_consent" {
				q := BrandCloudOwnerTransferQuery{BrandCloudID: ack.CloudID, TransferID: ack.OperationID, RequesterID: ack.TargetUserID}
				view, err := env.store.PreviewOwnerHandoff(ctx, q)
				if err != nil {
					t.Fatal(err)
				}
				confirmBalance(t, env, q, *view.BalanceSnapshot, "target")
				q.RequesterID = ack.SourceUserID
				confirmBalance(t, env, q, *view.BalanceSnapshot, "source")
				b.mu.Lock()
				b.amount = -1
				b.mu.Unlock()
			}
			if _, err := env.store.CommitOwnerHandoff(ctx, ack.CloudID, ack.OperationID); err == nil {
				t.Fatal("commit without valid current consent")
			}
			phase := "preparing"
			if mode == "negative_after_consent" {
				phase = "committing"
			}
			assertHandoffOwner(t, env, ack, ack.SourceUserID, phase, 1)
			if remote.grant != nil {
				t.Fatal("incomplete consent acquired grant")
			}
		})
	}
}

func TestHandoffFinalizationAuditFailureRetainsFenceAndRetries(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	_, ack, _ := readyCommitFixture(t, env)
	decision, err := env.store.CommitOwnerHandoff(ctx, ack.CloudID, ack.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `ALTER TABLE audit_events ADD CONSTRAINT reject_finalize_ack CHECK(event_type<>'brand_cloud_owner_transfer_finalization_acknowledged') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = env.db.Exec(context.Background(), `ALTER TABLE audit_events DROP CONSTRAINT IF EXISTS reject_finalize_ack`)
	})
	if _, err := env.store.FinalizeOwnerHandoff(ctx, ack.CloudID, ack.OperationID); err == nil {
		t.Fatal("auditless finalize acknowledgment accepted")
	}
	assertHandoffOwner(t, env, ack, ack.TargetUserID, "finalizing", 2)
	var receipts, releases int
	if err := env.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM cloud_handoff_finalization_acknowledgments),(SELECT count(*) FROM cloud_handoff_outbox WHERE action='release')`).Scan(&receipts, &releases); err != nil || receipts != 0 || releases != 0 {
		t.Fatalf("partial finalization persisted: %d %d %v", receipts, releases, err)
	}
	if _, err := env.db.Exec(ctx, `ALTER TABLE audit_events DROP CONSTRAINT reject_finalize_ack`); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.FinalizeOwnerHandoff(ctx, ack.CloudID, ack.OperationID); err != nil {
		t.Fatal(err)
	}
	in := HandoffFinalizationAck{CloudID: ack.CloudID, OperationID: ack.OperationID, OwnershipVersion: 1, DecisionSHA256: decision.DecisionSHA256, Participant: "test_resources", ReceiptSHA256: strings.Repeat("e", 64)}
	for i := 0; i < 2; i++ {
		if phase, err := env.store.RecordHandoffFinalizationAck(ctx, in); err != nil || phase != "succeeded" {
			t.Fatalf("release retry: %s %v", phase, err)
		}
	}
	in.ReceiptSHA256 = strings.Repeat("f", 64)
	if _, err := env.store.RecordHandoffFinalizationAck(ctx, in); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed receipt accepted: %v", err)
	}
}

func TestHandoffCommitDecisionCannotPersistWithoutOwnerTransaction(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	remote, ack, _ := readyCommitFixture(t, env)
	remote.loseGrant = true
	if _, err := env.store.CommitOwnerHandoff(ctx, ack.CloudID, ack.OperationID); !errors.Is(err, ErrHandoffUnavailable) {
		t.Fatalf("expected lost grant reply: %v", err)
	}
	if _, err := env.db.Exec(ctx, `INSERT INTO cloud_handoff_committed_decisions(operation_id,authorization_id,billing_snapshot_version,committed_ownership_version,committed_at,decision_sha256)
		SELECT operation_id,authorization_id,billing_snapshot_version,2,clock_timestamp(),repeat('a',64) FROM cloud_handoff_commit_requests WHERE operation_id=$1`, ack.OperationID); err == nil {
		t.Fatal("deferred constraint accepted decision without owner swap")
	}
	assertHandoffOwner(t, env, ack, ack.SourceUserID, "committing", 1)
	var decisions int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM cloud_handoff_committed_decisions`).Scan(&decisions); err != nil || decisions != 0 {
		t.Fatal("orphan decision survived", err)
	}
}
