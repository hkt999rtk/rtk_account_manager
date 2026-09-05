package api

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

func TestSocialLoginActivatesExistingPendingUser(t *testing.T) {
	for _, linkedBeforeLogin := range []bool{false, true} {
		name := "matched_by_email"
		if linkedBeforeLogin {
			name = "matched_by_existing_identity"
		}
		t.Run(name, func(t *testing.T) {
			env := newIntegrationEnv(t)
			ctx := context.Background()
			email := name + "@example.test"
			signup, err := env.store.SignupDeveloper(ctx, store.DeveloperSignupInput{
				Email: email, PasswordHash: "pending-password-hash", OrganizationName: "Pending Cloud",
				SignupPendingVerification: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := env.store.CreateEmailVerificationToken(ctx, signup.User.ID, "pending-verification-token", time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}

			secretRef := "env:GOOGLE_CLIENT_SECRET"
			provider, err := env.store.CreateIdentityProvider(ctx, store.IdentityProviderCreateInput{
				ProviderID: "google", Name: "Google", Type: model.IdentityProviderTypeOIDC,
				IssuerURL: "https://accounts.google.com", ClientID: "client", ClientSecretRef: &secretRef,
				Scopes: []string{"openid", "email", "profile"}, Enabled: true, Now: time.Now().UTC(),
			})
			if err != nil {
				t.Fatal(err)
			}
			identity := auth.OIDCIdentity{
				Issuer: provider.IssuerURL, Subject: "subject-" + name, Email: email, EmailVerified: true,
				Claims: map[string]any{"sub": "subject-" + name, "email": email, "email_verified": true},
			}
			if linkedBeforeLogin {
				if _, err := env.store.CreateUserIdentity(ctx, store.UserIdentityCreateInput{
					UserID: signup.User.ID, ProviderID: provider.ID, IssuerURL: identity.Issuer,
					Subject: identity.Subject, Email: email, EmailVerified: true, Claims: identity.Claims, Now: time.Now().UTC(),
				}); err != nil {
					t.Fatal(err)
				}
			}

			ginContext, _ := gin.CreateTestContext(httptest.NewRecorder())
			ginContext.Request = httptest.NewRequest("GET", "/v1/auth/social/callback", nil)
			user, err := env.server.resolveSocialUser(ginContext, provider, identity)
			if err != nil {
				t.Fatal(err)
			}
			if !user.EmailVerified || user.EmailVerifiedAt == nil || user.SignupPendingVerification {
				t.Fatalf("expected social login to activate pending user, got %+v", user)
			}
			linked, err := env.store.GetUserIdentityByProviderSubject(ctx, provider.ID, identity.Subject)
			if err != nil || linked.UserID != signup.User.ID {
				t.Fatalf("expected identity linked to existing user: identity=%+v err=%v", linked, err)
			}
			var activeVerificationTokens int
			if err := env.db.QueryRow(ctx, `SELECT count(*) FROM auth_tokens WHERE user_id=$1 AND purpose='email_verification' AND consumed_at IS NULL`, signup.User.ID).Scan(&activeVerificationTokens); err != nil {
				t.Fatal(err)
			}
			if activeVerificationTokens != 0 {
				t.Fatalf("expected old verification tokens to be invalidated, got %d active", activeVerificationTokens)
			}
		})
	}
}

func TestSocialLoginDoesNotReactivateDisabledUser(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()
	signup, err := env.store.SignupDeveloper(ctx, store.DeveloperSignupInput{
		Email: "disabled-social@example.test", PasswordHash: "hash", OrganizationName: "Disabled Cloud",
		EmailVerified: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	secretRef := "env:GITHUB_CLIENT_SECRET"
	provider, err := env.store.CreateIdentityProvider(ctx, store.IdentityProviderCreateInput{
		ProviderID: "github", Name: "GitHub", Type: model.IdentityProviderTypeOAuth2,
		IssuerURL: "https://github.com", ClientID: "client", ClientSecretRef: &secretRef,
		Enabled: true, Now: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	identity := auth.OIDCIdentity{Issuer: provider.IssuerURL, Subject: "disabled-subject", Email: signup.User.Email, EmailVerified: true}
	if _, err := env.store.CreateUserIdentity(ctx, store.UserIdentityCreateInput{
		UserID: signup.User.ID, ProviderID: provider.ID, IssuerURL: identity.Issuer, Subject: identity.Subject,
		Email: identity.Email, EmailVerified: true, Now: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE users SET disabled_at=now() WHERE id=$1`, signup.User.ID); err != nil {
		t.Fatal(err)
	}
	ginContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginContext.Request = httptest.NewRequest("GET", "/v1/auth/social/callback", nil)
	if _, err := env.server.resolveSocialUser(ginContext, provider, identity); !errors.Is(err, errOIDCUserNotProvisioned) {
		t.Fatalf("expected disabled account to remain blocked, got %v", err)
	}
}
