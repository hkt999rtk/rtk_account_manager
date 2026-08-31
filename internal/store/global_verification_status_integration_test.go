package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestGlobalVerificationTokenStatusHonorsAccountAndTokenState(t *testing.T) {
	for _, tc := range []struct {
		name, want                                             string
		pending, verified, disabled, expired, consumed         bool
		legacySubject, mismatchedSubject, scoped, wrongPurpose bool
	}{
		{name: "unverified-non-pending", want: "valid"},
		{name: "unverified-pending", pending: true, want: "valid"},
		{name: "already-verified", verified: true, want: "invalid"},
		{name: "disabled", disabled: true, want: "invalid"},
		{name: "expired", expired: true, want: "expired"},
		{name: "consumed", consumed: true, want: "invalid"},
		{name: "legacy-subject", legacySubject: true, want: "invalid"},
		{name: "mismatched-subject", mismatchedSubject: true, want: "invalid"},
		{name: "tenant-scope", scoped: true, want: "invalid"},
		{name: "wrong-purpose", wrongPurpose: true, want: "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newStoreIntegrationEnv(t)
			ctx := context.Background()
			_, userID, _ := createDeviceFixture(t, env)
			if err := env.store.CreateEmailVerificationToken(ctx, userID, "fixture-verification-status", time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if _, err := env.db.Exec(ctx, `UPDATE users SET signup_pending_verification=$2,email_verified=$3,
				disabled_at=CASE WHEN $4 THEN now() ELSE NULL END WHERE id=$1`, userID, tc.pending, tc.verified, tc.disabled); err != nil {
				t.Fatal(err)
			}
			if _, err := env.db.Exec(ctx, `UPDATE auth_tokens SET
				expires_at=CASE WHEN $1 THEN now()-interval '1 second' ELSE expires_at END,
				consumed_at=CASE WHEN $2 THEN now() ELSE NULL END,
				subject_type=CASE WHEN $3 THEN 'brand_cloud_user' ELSE 'user' END,
				subject_id=CASE WHEN $4 THEN gen_random_uuid() ELSE subject_id END,
				scope=CASE WHEN $5 THEN 'legacy-tenant' ELSE '' END,
				purpose=CASE WHEN $6 THEN 'password_reset' ELSE 'email_verification' END
				WHERE token_hash='fixture-verification-status'`, tc.expired, tc.consumed, tc.legacySubject, tc.mismatchedSubject, tc.scoped, tc.wrongPurpose); err != nil {
				t.Fatal(err)
			}
			if status, err := env.store.EmailVerificationTokenStatus(ctx, "fixture-verification-status"); err != nil || status != tc.want {
				t.Errorf("status=%s want=%s err=%v", status, tc.want, err)
			}
			if tc.legacySubject || tc.mismatchedSubject || tc.scoped || tc.wrongPurpose || tc.disabled || tc.expired || tc.consumed {
				if _, err := env.store.VerifyEmailToken(ctx, "fixture-verification-status", "fixture-new-hash"); !errors.Is(err, ErrNotFound) {
					t.Errorf("ineligible token must not activate a global user: %v", err)
				}
			}
		})
	}
}
