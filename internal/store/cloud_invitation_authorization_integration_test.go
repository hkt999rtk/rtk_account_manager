package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"rtk_account_manager/internal/model"
)

func TestMultiCloudInvitationRevalidatesOwnerIntegration(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	user := func(email string) DeveloperSignupResult {
		u, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{Email: email, PasswordHash: "hash"})
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	owner, target, next := user("inviter-owner@test.invalid"), user("inviter-target@test.invalid"), user("inviter-next@test.invalid")
	if _, err := env.db.Exec(ctx, `UPDATE users SET email_verified=true,signup_pending_verification=false`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	in := BrandCloudMemberInvitationInput{BrandCloudID: owner.BrandCloud.ID, InvitedByUserID: owner.User.ID, TargetEmail: target.User.Email, Role: model.RoleMember, TokenHash: "revalidate-inviter", ExpiresAt: now.Add(time.Hour)}
	if _, _, err := env.store.CreateBrandCloudMemberInvitation(ctx, in, now); err != nil {
		t.Fatal(err)
	}
	for _, state := range []struct{ name, disable, restore string }{
		{"unverified", `UPDATE users SET email_verified=false WHERE id=$1`, `UPDATE users SET email_verified=true WHERE id=$1`},
		{"disabled", `UPDATE users SET disabled_at=now() WHERE id=$1`, `UPDATE users SET disabled_at=NULL WHERE id=$1`},
		{"pending", `UPDATE users SET signup_pending_verification=true WHERE id=$1`, `UPDATE users SET signup_pending_verification=false WHERE id=$1`},
	} {
		t.Run(state.name, func(t *testing.T) {
			if _, err := env.db.Exec(ctx, state.disable, owner.User.ID); err != nil {
				t.Fatal(err)
			}
			if _, _, err := env.store.AcceptBrandCloudMemberInvitation(ctx, target.User.ID, in.TokenHash, now); !errors.Is(err, ErrNotFound) {
				t.Fatalf("inactive owner invite accepted: %v", err)
			}
			if _, err := env.db.Exec(ctx, state.restore, owner.User.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
	// Fixture for a committed ownership change: leave the predecessor as admin
	// deliberately, proving acceptance checks ownership, not membership alone.
	// Production transfer must additionally remove all predecessor membership.
	tx, err := env.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE organization_members SET role='admin' WHERE organization_id=$1 AND user_id=$2`, owner.BrandCloud.ID, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role) VALUES($1,$2,'owner')`, owner.BrandCloud.ID, next.User.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.store.AcceptBrandCloudMemberInvitation(ctx, target.User.ID, in.TokenHash, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("former owner invite accepted: %v", err)
	}
	var count int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM organization_members WHERE organization_id=$1 AND user_id=$2`, owner.BrandCloud.ID, target.User.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed acceptance changed membership: %d %v", count, err)
	}
}

func TestMultiCloudInvitationConcurrentAcceptanceIntegration(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	owner, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{Email: "concurrent-inviter@test.invalid", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	target, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{Email: "concurrent-invitee@test.invalid", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE users SET email_verified=true,signup_pending_verification=false`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	in := BrandCloudMemberInvitationInput{BrandCloudID: owner.BrandCloud.ID, InvitedByUserID: owner.User.ID, TargetEmail: target.User.Email, Role: model.RoleMember, TokenHash: "concurrent-invite", ExpiresAt: now.Add(time.Hour)}
	if _, _, err := env.store.CreateBrandCloudMemberInvitation(ctx, in, now); err != nil {
		t.Fatal(err)
	}
	start, results := make(chan struct{}), make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			_, _, err := env.store.AcceptBrandCloudMemberInvitation(ctx, target.User.ID, in.TokenHash, now)
			results <- err
		}()
	}
	close(start)
	accepted, rejected := 0, 0
	for i := 0; i < 2; i++ {
		select {
		case err := <-results:
			if err == nil {
				accepted++
			} else if errors.Is(err, ErrNotFound) {
				rejected++
			} else {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if accepted != 1 || rejected != 1 {
		t.Fatalf("accepted=%d rejected=%d", accepted, rejected)
	}
}
