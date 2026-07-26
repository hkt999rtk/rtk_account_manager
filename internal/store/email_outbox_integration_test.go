package store

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"rtk_account_manager/internal/emaildelivery"
	"rtk_account_manager/internal/model"
)

func TestEmailOutboxTokenAndQueueAreTransactional(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	cipher := integrationEmailCipher(t)
	env.store.ConfigureEmailOutboxCipher(cipher)
	registered, err := env.store.Register(ctx, RegisterInput{
		Email: "email-outbox@example.com", PasswordHash: "hash", OrganizationName: "Email Outbox Org",
	})
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Now().UTC().Add(time.Hour)
	input := EmailOutboxInput{
		IdempotencyKey: "auth-token:hash-1", MessageType: "email_verification", TemplateVersion: 1,
		Payload: emaildelivery.Payload{
			RecipientEmail: registered.User.Email, Token: "raw-secret-token",
			ExpiresAt: expiresAt.Format(time.RFC3339),
		},
		ExpiresAt: &expiresAt,
	}
	if err := env.store.CreateAuthTokenAndEmail(ctx, registered.User.ID, "email_verification", "hash-1", expiresAt, input); err != nil {
		t.Fatal(err)
	}
	var tokenCount, outboxCount int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM auth_tokens WHERE token_hash = 'hash-1'`).Scan(&tokenCount); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM email_outbox WHERE idempotency_key = 'auth-token:hash-1'`).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if tokenCount != 1 || outboxCount != 1 {
		t.Fatalf("token=%d outbox=%d, want 1/1", tokenCount, outboxCount)
	}
	var ciphertext []byte
	if err := env.db.QueryRow(ctx, `SELECT payload_ciphertext FROM email_outbox WHERE idempotency_key = 'auth-token:hash-1'`).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ciphertext), "raw-secret-token") {
		t.Fatal("outbox ciphertext contains raw token")
	}

	bad := input
	bad.IdempotencyKey = "auth-token:hash-2"
	bad.MessageType = "unsupported"
	if err := env.store.CreateAuthTokenAndEmail(ctx, registered.User.ID, "email_verification", "hash-2", expiresAt, bad); err == nil {
		t.Fatal("invalid email type unexpectedly committed")
	}
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM auth_tokens WHERE token_hash = 'hash-2'`).Scan(&tokenCount); err != nil {
		t.Fatal(err)
	}
	if tokenCount != 0 {
		t.Fatalf("rolled-back token count = %d, want 0", tokenCount)
	}
}

func TestEmailOutboxClaimLeaseAndIdempotency(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	env.store.ConfigureEmailOutboxCipher(integrationEmailCipher(t))
	input := EmailOutboxInput{
		IdempotencyKey: "quota:one", MessageType: "quota_declined",
		Payload: emaildelivery.Payload{RecipientEmail: "user@example.com", OrganizationID: "org-1"},
	}
	if err := env.store.EnqueueEmail(ctx, input); err != nil {
		t.Fatal(err)
	}
	if err := env.store.EnqueueEmail(ctx, input); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM email_outbox WHERE idempotency_key = 'quota:one'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("idempotent row count = %d, want 1", count)
	}
	now := time.Now().UTC()
	first, err := env.store.ClaimEmailOutboxReady(ctx, now, now.Add(time.Minute), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].Status != model.EmailOutboxStatusSending {
		t.Fatalf("first claim = %+v", first)
	}
	second, err := env.store.ClaimEmailOutboxReady(ctx, now.Add(time.Second), now.Add(2*time.Minute), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Fatalf("leased message was claimed twice: %+v", second)
	}
	reclaimed, err := env.store.ClaimEmailOutboxReady(ctx, now.Add(2*time.Minute), now.Add(3*time.Minute), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 1 {
		t.Fatalf("expired lease was not reclaimed: %+v", reclaimed)
	}
}

func TestEmailOutboxTransitionsListRequeueAndCounts(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	env.store.ConfigureEmailOutboxCipher(integrationEmailCipher(t))
	now := time.Now().UTC()
	expiresAt := now.Add(time.Hour)
	for _, input := range []EmailOutboxInput{
		{IdempotencyKey: "transition:sent", MessageType: "email_verification", Payload: emaildelivery.Payload{RecipientEmail: "sent@example.com"}},
		{IdempotencyKey: "transition:retry", MessageType: "password_reset", Payload: emaildelivery.Payload{RecipientEmail: "retry@example.com"}},
		{IdempotencyKey: "transition:expired", MessageType: "login_activation", Payload: emaildelivery.Payload{RecipientEmail: "expired@example.com"}, ExpiresAt: &expiresAt},
	} {
		if err := env.store.EnqueueEmail(ctx, input); err != nil {
			t.Fatal(err)
		}
	}
	claimed, err := env.store.ClaimEmailOutboxReady(ctx, now.Add(time.Second), now.Add(time.Minute), 3)
	if err != nil || len(claimed) != 3 {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}
	byKey := make(map[string]model.EmailOutbox, len(claimed))
	for _, message := range claimed {
		byKey[message.IdempotencyKey] = message
	}
	sentAt := now.Add(2 * time.Second)
	if changed, err := env.store.TransitionEmailOutbox(ctx, EmailOutboxTransitionInput{
		ID: byKey["transition:sent"].ID, FromAttempt: 0, Status: model.EmailOutboxStatusSent,
		AttemptCount: 1, AvailableAt: sentAt, SentAt: &sentAt, ClearPayload: true,
	}); err != nil || !changed {
		t.Fatalf("mark sent = %v, %v", changed, err)
	}
	if changed, err := env.store.TransitionEmailOutbox(ctx, EmailOutboxTransitionInput{
		ID: byKey["transition:sent"].ID, FromAttempt: 0, Status: model.EmailOutboxStatusSent,
		AttemptCount: 1, AvailableAt: sentAt, SentAt: &sentAt, ClearPayload: true,
	}); err != nil || changed {
		t.Fatalf("stale transition = %v, %v", changed, err)
	}
	if changed, err := env.store.TransitionEmailOutbox(ctx, EmailOutboxTransitionInput{
		ID: byKey["transition:retry"].ID, FromAttempt: 0, Status: model.EmailOutboxStatusDeadLettered,
		AttemptCount: 1, AvailableAt: sentAt,
	}); err != nil || !changed {
		t.Fatalf("mark dead letter = %v, %v", changed, err)
	}
	if changed, err := env.store.TransitionEmailOutbox(ctx, EmailOutboxTransitionInput{
		ID: byKey["transition:expired"].ID, FromAttempt: 0, Status: model.EmailOutboxStatusExpired,
		AttemptCount: 1, AvailableAt: sentAt, ClearPayload: true,
	}); err != nil || !changed {
		t.Fatalf("mark expired = %v, %v", changed, err)
	}
	if changed, err := env.store.RequeueEmailOutbox(ctx, byKey["transition:retry"].ID, sentAt); err != nil || !changed {
		t.Fatalf("requeue = %v, %v", changed, err)
	}
	if changed, err := env.store.RequeueEmailOutbox(ctx, byKey["transition:expired"].ID, sentAt); err != nil || changed {
		t.Fatalf("requeue expired = %v, %v", changed, err)
	}
	retrying, err := env.store.ListEmailOutbox(ctx, model.EmailOutboxStatusRetrying, 0)
	if err != nil || len(retrying) != 1 || retrying[0].PayloadNonce != nil || retrying[0].PayloadCiphertext != nil {
		t.Fatalf("retrying list = %+v, %v", retrying, err)
	}
	counts, err := env.store.GetEmailOutboxCounts(ctx, sentAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if counts.Retrying != 1 || counts.Sent != 1 || counts.Expired != 1 || counts.DeadLettered != 0 || counts.OldestPendingAge <= 0 || counts.DeliveryLatency <= 0 {
		t.Fatalf("counts = %+v", counts)
	}
}

func TestQuotaDecisionAndEmailOutboxCommitOrRollbackTogether(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	registered, err := env.store.Register(ctx, RegisterInput{
		Email: "quota-email@example.com", PasswordHash: "hash", OrganizationName: "Quota Email Org",
		OrganizationTier: model.OrganizationTierEvaluation, EvaluationDeviceQuota: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := env.store.CreateQuotaRaiseRequest(ctx, QuotaRaiseRequestInput{
		OrganizationID: registered.Organization.ID,
		RequestedBy:    registered.User.ID,
		RequestedQuota: 25,
		UseCase:        "production launch",
		ContactInfo:    map[string]any{"email": registered.User.Email},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = env.store.DecideQuotaRaiseRequest(ctx, QuotaRaiseDecisionInput{
		RequestID: request.ID, DecidedBy: registered.User.ID, Approved: true,
		ApprovedQuota: 25, EnqueueEmail: true,
	})
	if err == nil {
		t.Fatal("decision unexpectedly succeeded without an outbox cipher")
	}
	var status model.QuotaRaiseRequestStatus
	var quota int
	if err := env.db.QueryRow(ctx, `SELECT status FROM quota_raise_requests WHERE id = $1`, request.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(ctx, `SELECT evaluation_device_quota FROM organizations WHERE id = $1`, registered.Organization.ID).Scan(&quota); err != nil {
		t.Fatal(err)
	}
	if status != model.QuotaRaiseRequestStatusPending || quota != 5 {
		t.Fatalf("failed enqueue did not roll back: status=%s quota=%d", status, quota)
	}

	env.store.ConfigureEmailOutboxCipher(integrationEmailCipher(t))
	decided, org, _, err := env.store.DecideQuotaRaiseRequest(ctx, QuotaRaiseDecisionInput{
		RequestID: request.ID, DecidedBy: registered.User.ID, Approved: true,
		ApprovedQuota: 25, EnqueueEmail: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if decided.Status != model.QuotaRaiseRequestStatusApproved || org.EvaluationDeviceQuota != 25 {
		t.Fatalf("unexpected decision: request=%+v org=%+v", decided, org)
	}
	var outboxCount int
	if err := env.db.QueryRow(ctx, `
		SELECT count(*) FROM email_outbox
		WHERE idempotency_key = $1 AND message_type = 'quota_approved'
	`, "quota-decision:"+request.ID+":approved").Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if outboxCount != 1 {
		t.Fatalf("quota outbox count = %d, want 1", outboxCount)
	}
}

func TestOwnerTransferAndEmailOutboxCommitOrRollbackTogether(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	requester, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{
		Email: "owner-transfer-requester@example.com", PasswordHash: "hash",
	})
	if err != nil {
		t.Fatal(err)
	}
	target, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{
		Email: "owner-transfer-target@example.com", PasswordHash: "hash",
	})
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Now().UTC().Add(time.Hour)
	outbox := EmailOutboxInput{
		IdempotencyKey: "owner-transfer:token-hash", MessageType: "brand_cloud_owner_transfer",
		Payload: emaildelivery.Payload{
			RecipientEmail: target.User.Email, Token: "raw-owner-transfer-token",
			ExpiresAt: expiresAt.Format(time.RFC3339),
		},
		ExpiresAt: &expiresAt,
	}
	_, err = env.store.CreateBrandCloudOwnerTransfer(ctx, BrandCloudOwnerTransferInput{
		BrandCloudID: requester.BrandCloud.ID, RequestedByUserID: requester.User.ID,
		TargetEmail: target.User.Email, TokenHash: "owner-transfer-token-hash",
		ExpiresAt: expiresAt, Email: &outbox,
	})
	if err == nil {
		t.Fatal("owner transfer unexpectedly succeeded without an outbox cipher")
	}
	var transferCount int
	if err := env.db.QueryRow(ctx, `
		SELECT count(*) FROM brand_cloud_owner_transfers WHERE token_hash = 'owner-transfer-token-hash'
	`).Scan(&transferCount); err != nil {
		t.Fatal(err)
	}
	if transferCount != 0 {
		t.Fatalf("failed enqueue left %d owner transfer rows", transferCount)
	}

	env.store.ConfigureEmailOutboxCipher(integrationEmailCipher(t))
	transfer, err := env.store.CreateBrandCloudOwnerTransfer(ctx, BrandCloudOwnerTransferInput{
		BrandCloudID: requester.BrandCloud.ID, RequestedByUserID: requester.User.ID,
		TargetEmail: target.User.Email, TokenHash: "owner-transfer-token-hash",
		ExpiresAt: expiresAt, Email: &outbox,
	})
	if err != nil {
		t.Fatal(err)
	}
	if transfer.ID == "" || transfer.TargetEmail != target.User.Email {
		t.Fatalf("unexpected owner transfer: %+v", transfer)
	}
	var outboxCount int
	if err := env.db.QueryRow(ctx, `
		SELECT count(*) FROM email_outbox
		WHERE idempotency_key = 'owner-transfer:token-hash'
		  AND message_type = 'brand_cloud_owner_transfer'
	`).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if outboxCount != 1 {
		t.Fatalf("owner-transfer outbox count = %d, want 1", outboxCount)
	}
}

func TestBrandCloudLoginTokenAndEmailOutboxCommitTogether(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{
		Email: "brand-login-owner@example.com", PasswordHash: "hash",
	})
	if err != nil {
		t.Fatal(err)
	}
	brandUser, err := env.store.CreateBrandCloudUser(ctx, owner.User.ID, owner.BrandCloud.ID, BrandCloudUserInput{
		Email: "brand-login-user@example.com", PasswordHash: "hash", Role: model.RoleMember,
	})
	if err != nil {
		t.Fatal(err)
	}
	if owner.BrandCloud.TenantSlug == nil {
		t.Fatal("developer brand cloud has no tenant slug")
	}
	env.store.ConfigureEmailOutboxCipher(integrationEmailCipher(t))
	expiresAt := time.Now().UTC().Add(time.Hour)
	created, err := env.store.CreateBrandCloudLoginActivationTokenForEmailAndEmail(
		ctx,
		*owner.BrandCloud.TenantSlug,
		brandUser.BrandCloudUser.Email,
		"brand-login-token-hash",
		expiresAt,
		EmailOutboxInput{
			IdempotencyKey: "brand-login:token-hash", MessageType: "login_activation",
			Payload: emaildelivery.Payload{
				RecipientEmail: brandUser.BrandCloudUser.Email, Token: "raw-brand-login-token",
				ExpiresAt: expiresAt.Format(time.RFC3339),
			},
			ExpiresAt: &expiresAt,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("brand-cloud login token was not created")
	}
	var tokenCount, outboxCount int
	if err := env.db.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM auth_tokens
			 WHERE token_hash = 'brand-login-token-hash'
			   AND subject_type = 'brand_cloud_user'),
			(SELECT count(*) FROM email_outbox
			 WHERE idempotency_key = 'brand-login:token-hash'
			   AND message_type = 'login_activation')
	`).Scan(&tokenCount, &outboxCount); err != nil {
		t.Fatal(err)
	}
	if tokenCount != 1 || outboxCount != 1 {
		t.Fatalf("brand login token=%d outbox=%d, want 1/1", tokenCount, outboxCount)
	}
}

func TestBrandCloudOwnerEmailActivationIsAtomicTenantScopedAndOneTime(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{
		Email: "load-owner-admin@example.com", PasswordHash: "hash",
	})
	if err != nil {
		t.Fatal(err)
	}
	env.store.ConfigureEmailOutboxCipher(integrationEmailCipher(t))
	expiresAt := time.Now().UTC().Add(time.Hour)
	pending, err := env.store.CreateBrandCloudUser(ctx, owner.User.ID, owner.BrandCloud.ID, BrandCloudUserInput{
		Email: "imap-test01+load-run-b01@realtekconnect.com", PasswordHash: "inaccessible-hash",
		DisplayName: stringPtr("RTK Load 50K run Brand 01 Owner"), Role: model.RoleOwner,
		ActivationMode: "email", ActivationTokenHash: "owner-activation-token-hash",
		ActivationExpiresAt: expiresAt,
		ActivationEmail: &EmailOutboxInput{
			IdempotencyKey: "load-owner:token-hash", MessageType: "brand_cloud_user_activation",
			Payload: emaildelivery.Payload{
				RecipientEmail: "imap-test01+load-run-b01@realtekconnect.com",
				Token:          "raw-owner-activation-token", ExpiresAt: expiresAt.Format(time.RFC3339),
			},
			ExpiresAt: &expiresAt,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if pending.BrandCloudUser.EmailVerified || !pending.BrandCloudUser.SignupPendingVerification {
		t.Fatalf("email activation user must start pending: %+v", pending.BrandCloudUser)
	}
	var tokenCount, outboxCount int
	if err := env.db.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM auth_tokens WHERE token_hash = 'owner-activation-token-hash' AND purpose = 'brand_cloud_user_activation'),
			(SELECT count(*) FROM email_outbox WHERE idempotency_key = 'load-owner:token-hash' AND message_type = 'brand_cloud_user_activation')
	`).Scan(&tokenCount, &outboxCount); err != nil {
		t.Fatal(err)
	}
	if tokenCount != 1 || outboxCount != 1 {
		t.Fatalf("token/outbox counts = %d/%d, want 1/1", tokenCount, outboxCount)
	}
	if _, err := env.store.ActivateBrandCloudUser(ctx, "wrong-tenant", "owner-activation-token-hash", "new-password-hash"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong tenant activation error = %v, want not found", err)
	}
	if owner.BrandCloud.TenantSlug == nil {
		t.Fatal("brand cloud tenant slug is missing")
	}
	activated, err := env.store.ActivateBrandCloudUser(ctx, *owner.BrandCloud.TenantSlug, "owner-activation-token-hash", "new-password-hash")
	if err != nil {
		t.Fatal(err)
	}
	if !activated.BrandCloudUser.EmailVerified || activated.BrandCloudUser.SignupPendingVerification || activated.PasswordHash != "new-password-hash" {
		t.Fatalf("activated state is inconsistent: %+v", activated)
	}
	if _, err := env.store.ActivateBrandCloudUser(ctx, *owner.BrandCloud.TenantSlug, "owner-activation-token-hash", "another-hash"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("replay activation error = %v, want not found", err)
	}
}

func integrationEmailCipher(t *testing.T) *emaildelivery.Cipher {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = 3
	}
	cipher, err := emaildelivery.NewCipher(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	return cipher
}
