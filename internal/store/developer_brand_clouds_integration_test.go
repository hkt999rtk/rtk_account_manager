package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"rtk_account_manager/internal/emaildelivery"
	"rtk_account_manager/internal/model"
)

func TestDeveloperSignupCreatesDefaultBrandCloudAndEnforcesCloudLimit(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()

	result, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{
		Email:                     " Dev.Owner@Example.COM ",
		PasswordHash:              "hash",
		DisplayName:               stringPtr("Dev Owner"),
		OrganizationName:          "Developer Named Cloud",
		SignupPendingVerification: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.User.Email != "dev.owner@example.com" || result.User.DeveloperCloudLimit != 8 {
		t.Fatalf("unexpected developer user: %+v", result.User)
	}
	if result.BrandCloud.Name != "Developer Named Cloud" ||
		result.BrandCloud.OrganizationKind != model.OrganizationKindBrandCloud ||
		result.BrandCloud.Role != model.RoleOwner {
		t.Fatalf("unexpected default brand cloud: %+v", result.BrandCloud)
	}

	page, err := env.store.ListDeveloperBrandClouds(ctx, result.User.ID, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.Page.Total != 1 || len(page.Organizations) != 1 || page.Organizations[0].ID != result.BrandCloud.ID {
		t.Fatalf("expected default brand cloud in developer list, got %+v", page)
	}

	if err := env.store.SetDeveloperCloudLimit(ctx, result.User.ID, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.CreateDeveloperBrandCloud(ctx, result.User.ID, BrandCloudInput{Name: "Pending must fail"}); !errors.Is(err, ErrAccountNotActivated) {
		t.Fatalf("pending cloud creation err=%v", err)
	}
	// This store fixture represents completed activation; API/email evidence is separate.
	if _, err := env.db.Exec(ctx, `UPDATE users SET email_verified=true,signup_pending_verification=false WHERE id=$1`, result.User.ID); err != nil {
		t.Fatal(err)
	}
	second, err := env.store.CreateDeveloperBrandCloud(ctx, result.User.ID, BrandCloudInput{Name: "Second Cloud", TenantSlug: "second-cloud"})
	if err != nil {
		t.Fatal(err)
	}
	if second.Role != model.RoleOwner {
		t.Fatalf("developer-created brand cloud should return owner role, got %+v", second)
	}
	if _, err := env.store.CreateDeveloperBrandCloud(ctx, result.User.ID, BrandCloudInput{Name: "Third Cloud", TenantSlug: "third-cloud"}); !errors.Is(err, ErrDeveloperCloudLimitExceeded) {
		t.Fatalf("expected developer cloud limit error, got %v", err)
	}
	if err := env.store.SetDeveloperCloudLimit(ctx, result.User.ID, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.CreateDeveloperBrandCloud(ctx, result.User.ID, BrandCloudInput{Name: "Third Cloud", TenantSlug: "third-cloud"}); err != nil {
		t.Fatalf("expected increased developer cloud limit to allow third cloud, got %v", err)
	}
}

func TestDeveloperBrandCloudErrorPaths(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()

	result, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{
		Email:        "cloud-errors@example.com",
		PasswordHash: "hash",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{
		Email:        "cloud-errors@example.com",
		PasswordHash: "hash",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected duplicate developer signup conflict, got %v", err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE users SET email_verified=true,signup_pending_verification=false WHERE id=$1`, result.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.CreateDeveloperBrandCloud(ctx, result.User.ID, BrandCloudInput{
		Name:       "Invalid Slug",
		TenantSlug: "!!!",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected invalid tenant slug conflict, got %v", err)
	}
	if _, err := env.store.CreateDeveloperBrandCloud(ctx, "00000000-0000-0000-0000-000000000000", BrandCloudInput{
		Name: "Missing User",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected missing developer not found, got %v", err)
	}
	if err := env.store.SetDeveloperCloudLimit(ctx, "00000000-0000-0000-0000-000000000000", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected missing developer limit update not found, got %v", err)
	}
	if _, err := env.store.GetDeveloperBrandCloudMember(ctx, result.BrandCloud.ID, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected missing developer brand cloud member not found, got %v", err)
	}
}

func TestEnsurePlatformAdminCreatesRealtekConnectBrandCloud(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()

	root, err := env.store.EnsurePlatformAdmin(ctx, " Root.Admin@Example.COM ", "hash-one", stringPtr("Root Admin"))
	if err != nil {
		t.Fatal(err)
	}
	page, err := env.store.ListDeveloperBrandClouds(ctx, root.ID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.Page.Total != 1 || len(page.Organizations) != 1 ||
		page.Organizations[0].Name != "Realtek Connect+" ||
		page.Organizations[0].Role != model.RoleOwner {
		t.Fatalf("expected root Realtek Connect+ cloud, got %+v", page)
	}

	rootAgain, err := env.store.EnsurePlatformAdmin(ctx, "root.admin@example.com", "hash-two", nil)
	if err != nil {
		t.Fatal(err)
	}
	pageAgain, err := env.store.ListDeveloperBrandClouds(ctx, rootAgain.ID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if pageAgain.Page.Total != 1 || pageAgain.Organizations[0].ID != page.Organizations[0].ID {
		t.Fatalf("platform admin bootstrap must be idempotent, got before=%+v after=%+v", page, pageAgain)
	}
}

func TestBrandCloudMemberInvitationLifecycleAndConflicts(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()

	owner, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{
		Email:        "invitation-owner@example.com",
		PasswordHash: "hash",
	})
	if err != nil {
		t.Fatal(err)
	}
	target, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{
		Email:        "invitation-target@example.com",
		PasswordHash: "hash",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE users SET email_verified = true, email_verified_at = $2 WHERE id = $1`, target.User.ID, now); err != nil {
		t.Fatal(err)
	}

	base := BrandCloudMemberInvitationInput{
		BrandCloudID:    owner.BrandCloud.ID,
		InvitedByUserID: owner.User.ID,
		TargetEmail:     target.User.Email,
		Role:            model.RoleMember,
		TokenHash:       "invitation-token-1",
		ExpiresAt:       now.Add(time.Hour),
	}
	invalidRole := base
	invalidRole.Role = model.RoleOwner
	if _, _, err := env.store.CreateBrandCloudMemberInvitation(ctx, invalidRole, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("owner invitation role should conflict, got %v", err)
	}
	nonOwner := base
	nonOwner.InvitedByUserID = target.User.ID
	if _, _, err := env.store.CreateBrandCloudMemberInvitation(ctx, nonOwner, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-owner invitation should be hidden, got %v", err)
	}
	missingTarget := base
	missingTarget.TargetEmail = "missing-invitation-target@example.com"
	if _, _, err := env.store.CreateBrandCloudMemberInvitation(ctx, missingTarget, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing invitation target should be hidden, got %v", err)
	}
	pendingTarget, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{
		Email:                     "pending-invitation-target@example.com",
		PasswordHash:              "hash",
		SignupPendingVerification: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	pendingVerification := base
	pendingVerification.TargetEmail = pendingTarget.User.Email
	if _, _, err := env.store.CreateBrandCloudMemberInvitation(ctx, pendingVerification, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unverified invitation target should be hidden, got %v", err)
	}

	expiredInput := base
	expiredInput.TokenHash = "expired-invitation-token"
	expiredInput.ExpiresAt = now.Add(-time.Minute)
	expired, created, err := env.store.CreateBrandCloudMemberInvitation(ctx, expiredInput, now.Add(-time.Hour))
	if err != nil || !created {
		t.Fatalf("create expired invitation fixture: created=%v err=%v", created, err)
	}
	if _, _, err := env.store.AcceptBrandCloudMemberInvitation(ctx, target.User.ID, expiredInput.TokenHash, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired invitation acceptance should be hidden, got %v", err)
	}
	expiredInput.TokenHash = "expired-invitation-token-for-create"
	if _, created, err := env.store.CreateBrandCloudMemberInvitation(ctx, expiredInput, now.Add(-time.Hour)); err != nil || !created {
		t.Fatalf("create expiration-on-invite fixture: created=%v err=%v", created, err)
	}
	temporaryInput := base
	temporaryInput.TokenHash = "expiration-path-valid-token"
	temporary, created, err := env.store.CreateBrandCloudMemberInvitation(ctx, temporaryInput, now)
	if err != nil || !created {
		t.Fatalf("new invitation should expire stale pending rows: created=%v err=%v", created, err)
	}
	if _, err := env.store.CancelBrandCloudMemberInvitation(ctx, BrandCloudMemberInvitationMutation{
		BrandCloudID: owner.BrandCloud.ID, InvitationID: temporary.ID, ActorUserID: owner.User.ID,
	}, now); err != nil {
		t.Fatalf("cancel expiration-path fixture: %v", err)
	}
	expiredInput.TokenHash = "expired-invitation-token-for-list"
	expired, created, err = env.store.CreateBrandCloudMemberInvitation(ctx, expiredInput, now.Add(-time.Hour))
	if err != nil || !created {
		t.Fatalf("create expired invitation list fixture: created=%v err=%v", created, err)
	}
	items, err := env.store.ListBrandCloudMemberInvitations(ctx, owner.BrandCloud.ID, owner.User.ID, now)
	foundExpired := false
	for _, item := range items {
		if item.ID == expired.ID && item.Status == "expired" {
			foundExpired = true
		}
	}
	if err != nil || !foundExpired {
		t.Fatalf("expected expired invitation in owner list, got %+v, %v", items, err)
	}
	if _, err := env.store.ListBrandCloudMemberInvitations(ctx, owner.BrandCloud.ID, target.User.ID, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-owner invitation list should be hidden, got %v", err)
	}

	outbox := EmailOutboxInput{
		IdempotencyKey: "member-invitation:token-1",
		MessageType:    "brand_cloud_membership_invitation",
		Payload: emaildelivery.Payload{
			RecipientEmail: target.User.Email,
			Token:          "raw-invitation-token-1",
			ExpiresAt:      base.ExpiresAt.Format(time.RFC3339),
		},
		ExpiresAt: &base.ExpiresAt,
	}
	base.Email = &outbox
	if _, _, err := env.store.CreateBrandCloudMemberInvitation(ctx, base, now); err == nil {
		t.Fatal("invitation unexpectedly committed without an email outbox cipher")
	}
	env.store.ConfigureEmailOutboxCipher(integrationEmailCipher(t))
	invitation, created, err := env.store.CreateBrandCloudMemberInvitation(ctx, base, now)
	if err != nil || !created {
		t.Fatalf("create pending invitation: created=%v err=%v", created, err)
	}
	duplicate, created, err := env.store.CreateBrandCloudMemberInvitation(ctx, base, now)
	if err != nil || created || duplicate.ID != invitation.ID {
		t.Fatalf("matching invitation should be idempotent, got %+v created=%v err=%v", duplicate, created, err)
	}
	roleConflict := base
	roleConflict.Role = model.RoleAdmin
	if _, _, err := env.store.CreateBrandCloudMemberInvitation(ctx, roleConflict, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("pending invitation role change should conflict, got %v", err)
	}

	missingMutation := BrandCloudMemberInvitationMutation{
		BrandCloudID: owner.BrandCloud.ID, InvitationID: "00000000-0000-0000-0000-000000000000", ActorUserID: owner.User.ID,
		TokenHash: "missing-token", ExpiresAt: now.Add(time.Hour),
	}
	nonOwnerMutation := missingMutation
	nonOwnerMutation.InvitationID = invitation.ID
	nonOwnerMutation.ActorUserID = target.User.ID
	if _, err := env.store.ResendBrandCloudMemberInvitation(ctx, nonOwnerMutation, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-owner invitation resend should be hidden, got %v", err)
	}
	if _, err := env.store.CancelBrandCloudMemberInvitation(ctx, nonOwnerMutation, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-owner invitation cancel should be hidden, got %v", err)
	}
	if _, err := env.store.ResendBrandCloudMemberInvitation(ctx, missingMutation, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("missing invitation resend should conflict, got %v", err)
	}
	mutation := missingMutation
	mutation.InvitationID = invitation.ID
	mutation.TokenHash = "invitation-token-2"
	resendOutbox := outbox
	resendOutbox.IdempotencyKey = "member-invitation:token-2"
	resendOutbox.Payload.Token = "raw-invitation-token-2"
	mutation.Email = &resendOutbox
	env.store.ConfigureEmailOutboxCipher(nil)
	if _, err := env.store.ResendBrandCloudMemberInvitation(ctx, mutation, now); err == nil {
		t.Fatal("invitation resend unexpectedly committed without an email outbox cipher")
	}
	env.store.ConfigureEmailOutboxCipher(integrationEmailCipher(t))
	resent, err := env.store.ResendBrandCloudMemberInvitation(ctx, mutation, now)
	if err != nil || resent.ID != invitation.ID {
		t.Fatalf("resend pending invitation: %+v, %v", resent, err)
	}
	if _, err := env.store.CancelBrandCloudMemberInvitation(ctx, missingMutation, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("missing invitation cancel should conflict, got %v", err)
	}
	canceled, err := env.store.CancelBrandCloudMemberInvitation(ctx, mutation, now)
	if err != nil || canceled.Status != "canceled" {
		t.Fatalf("cancel pending invitation: %+v, %v", canceled, err)
	}
	if _, _, err := env.store.AcceptBrandCloudMemberInvitation(ctx, target.User.ID, mutation.TokenHash, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("canceled invitation should reject acceptance, got %v", err)
	}

	base.TokenHash = "invitation-token-3"
	invitation, created, err = env.store.CreateBrandCloudMemberInvitation(ctx, base, now)
	if err != nil || !created {
		t.Fatalf("recreate invitation after cancellation: created=%v err=%v", created, err)
	}
	if _, err := env.db.Exec(ctx, `INSERT INTO organization_members (organization_id, user_id, role) VALUES ($1, $2, 'member')`, owner.BrandCloud.ID, target.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.store.AcceptBrandCloudMemberInvitation(ctx, target.User.ID, base.TokenHash, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("invitation acceptance with an existing member should conflict, got %v", err)
	}
	if _, err := env.db.Exec(ctx, `DELETE FROM organization_members WHERE organization_id = $1 AND user_id = $2`, owner.BrandCloud.ID, target.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.store.AcceptBrandCloudMemberInvitation(ctx, owner.User.ID, base.TokenHash, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong developer should not accept invitation, got %v", err)
	}
	accepted, member, err := env.store.AcceptBrandCloudMemberInvitation(ctx, target.User.ID, base.TokenHash, now)
	if err != nil || accepted.Status != "accepted" || member.Role != model.RoleMember {
		t.Fatalf("accept pending invitation: invitation=%+v member=%+v err=%v", accepted, member, err)
	}
	if _, _, err := env.store.AcceptBrandCloudMemberInvitation(ctx, target.User.ID, base.TokenHash, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("accepted invitation replay should be hidden, got %v", err)
	}
	base.TokenHash = "member-already-exists"
	if _, _, err := env.store.CreateBrandCloudMemberInvitation(ctx, base, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("existing member invitation should conflict, got %v", err)
	}

	unavailableDB, err := pgxpool.New(ctx, "postgres://invalid:invalid@127.0.0.1:1/invalid?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer unavailableDB.Close()
	unavailableStore := New(unavailableDB)
	if _, _, err := unavailableStore.CreateBrandCloudMemberInvitation(ctx, base, now); err == nil {
		t.Fatal("invitation creation unexpectedly succeeded with an unavailable database")
	}
	if _, err := unavailableStore.ListBrandCloudMemberInvitations(ctx, owner.BrandCloud.ID, owner.User.ID, now); err == nil {
		t.Fatal("invitation listing unexpectedly succeeded with an unavailable database")
	}
	if _, err := unavailableStore.ResendBrandCloudMemberInvitation(ctx, mutation, now); err == nil {
		t.Fatal("invitation resend unexpectedly succeeded with an unavailable database")
	}
	if _, err := unavailableStore.CancelBrandCloudMemberInvitation(ctx, mutation, now); err == nil {
		t.Fatal("invitation cancellation unexpectedly succeeded with an unavailable database")
	}
	if _, _, err := unavailableStore.AcceptBrandCloudMemberInvitation(ctx, target.User.ID, base.TokenHash, now); err == nil {
		t.Fatal("invitation acceptance unexpectedly succeeded with an unavailable database")
	}
}

func TestBrandCloudOwnerTransferRequiresExistingTargetAndAcceptsWithLoggedInDeveloper(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()

	source, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{
		Email:        "source-owner@example.com",
		PasswordHash: "hash",
	})
	if err != nil {
		t.Fatal(err)
	}
	target, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{
		Email:        "target-owner@example.com",
		PasswordHash: "hash",
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := env.store.CreateBrandCloudOwnerTransfer(ctx, BrandCloudOwnerTransferInput{
		BrandCloudID:      source.BrandCloud.ID,
		RequestedByUserID: source.User.ID,
		TargetEmail:       "missing-target@example.com",
		TokenHash:         "missing-token-hash",
		ExpiresAt:         time.Now().Add(time.Hour),
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected missing developer target to reject transfer, got %v", err)
	}
	if _, err := env.store.CreateBrandCloudOwnerTransfer(ctx, BrandCloudOwnerTransferInput{
		BrandCloudID:      source.BrandCloud.ID,
		RequestedByUserID: source.User.ID,
		TargetEmail:       "source-owner@example.com",
		TokenHash:         "self-token-hash",
		ExpiresAt:         time.Now().Add(time.Hour),
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected self-transfer conflict, got %v", err)
	}
	if _, err := env.store.CreateBrandCloudOwnerTransfer(ctx, BrandCloudOwnerTransferInput{
		BrandCloudID:      source.BrandCloud.ID,
		RequestedByUserID: target.User.ID,
		TargetEmail:       "source-owner@example.com",
		TokenHash:         "non-owner-token-hash",
		ExpiresAt:         time.Now().Add(time.Hour),
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected non-owner transfer request not found, got %v", err)
	}

	transfer, err := env.store.CreateBrandCloudOwnerTransfer(ctx, BrandCloudOwnerTransferInput{
		BrandCloudID:      source.BrandCloud.ID,
		RequestedByUserID: source.User.ID,
		TargetEmail:       " target-owner@example.com ",
		TokenHash:         "transfer-token-hash",
		ExpiresAt:         time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if transfer.Status != "pending" || transfer.TargetUserID != target.User.ID {
		t.Fatalf("unexpected owner transfer: %+v", transfer)
	}
	status, err := env.store.GetBrandCloudOwnerTransfer(ctx, BrandCloudOwnerTransferQuery{BrandCloudID: source.BrandCloud.ID, TransferID: transfer.ID, RequesterID: source.User.ID}, time.Now())
	if err != nil || status.Status != "pending" {
		t.Fatalf("expected pending transfer status, got %+v, %v", status, err)
	}
	cancelled, err := env.store.CancelBrandCloudOwnerTransfer(ctx, BrandCloudOwnerTransferQuery{BrandCloudID: source.BrandCloud.ID, TransferID: transfer.ID, RequesterID: source.User.ID}, time.Now())
	if err != nil || cancelled.Status != "canceled" {
		t.Fatalf("expected canceled transfer, got %+v, %v", cancelled, err)
	}
	if _, err := env.store.AcceptBrandCloudOwnerTransfer(ctx, target.User.ID, "transfer-token-hash", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("canceled transfer must reject, got %v", err)
	}
	transfer, err = env.store.CreateBrandCloudOwnerTransfer(ctx, BrandCloudOwnerTransferInput{BrandCloudID: source.BrandCloud.ID, RequestedByUserID: source.User.ID, TargetEmail: "target-owner@example.com", TokenHash: "transfer-token-hash-2", ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	expiredTransfer, err := env.store.CreateBrandCloudOwnerTransfer(ctx, BrandCloudOwnerTransferInput{
		BrandCloudID:      source.BrandCloud.ID,
		RequestedByUserID: source.User.ID,
		TargetEmail:       "target-owner@example.com",
		TokenHash:         "expired-token-hash",
		ExpiresAt:         time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	expiredStatus, err := env.store.GetBrandCloudOwnerTransfer(ctx, BrandCloudOwnerTransferQuery{
		BrandCloudID: source.BrandCloud.ID,
		TransferID:   expiredTransfer.ID,
		RequesterID:  source.User.ID,
	}, time.Now())
	if err != nil || expiredStatus.Status != "expired" {
		t.Fatalf("expected expired transfer status, got %+v, %v", expiredStatus, err)
	}
	if _, err := env.store.GetBrandCloudOwnerTransfer(ctx, BrandCloudOwnerTransferQuery{
		BrandCloudID: source.BrandCloud.ID,
		TransferID:   "00000000-0000-0000-0000-000000000000",
		RequesterID:  source.User.ID,
	}, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing transfer status error = %v, want ErrNotFound", err)
	}
	if _, err := env.store.AcceptBrandCloudOwnerTransfer(ctx, target.User.ID, "expired-token-hash", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired transfer token must reject, got %v", err)
	}
	if _, err := env.store.AcceptBrandCloudOwnerTransfer(ctx, source.User.ID, "transfer-token-hash-2", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-target developer must not accept transfer, got %v", err)
	}
	accepted, err := env.store.AcceptBrandCloudOwnerTransfer(ctx, target.User.ID, "transfer-token-hash-2", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Status != "accepted" || accepted.AcceptedAt == nil {
		t.Fatalf("expected accepted transfer, got %+v", accepted)
	}

	sourceMember, err := env.store.GetDeveloperBrandCloudMember(ctx, source.BrandCloud.ID, source.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	targetMember, err := env.store.GetDeveloperBrandCloudMember(ctx, source.BrandCloud.ID, target.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sourceMember.Role != model.RoleAdmin || targetMember.Role != model.RoleOwner {
		t.Fatalf("expected source admin and target owner after transfer, source=%+v target=%+v", sourceMember, targetMember)
	}
	if _, err := env.store.AcceptBrandCloudOwnerTransfer(ctx, target.User.ID, "transfer-token-hash-2", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("accepted transfer token must reject replay, got %v", err)
	}
}
