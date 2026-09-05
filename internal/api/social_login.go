package api

import (
	"errors"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

type socialLoginStartRequest struct {
	ProviderID string `json:"provider_id"`
	Next       string `json:"next,omitempty"`
}

type socialLoginCallbackRequest struct {
	Code             string `json:"code"`
	State            string `json:"state"`
	Error            string `json:"error,omitempty"`
	ErrorDescription string `json:"error_description,omitempty"`
}

func (s *Server) listSocialProviders(c *gin.Context) {
	ids := make([]string, 0, len(s.socialProviders))
	for id := range s.socialProviders {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	providers := make([]gin.H, 0, len(ids))
	for _, id := range ids {
		provider := s.socialProviders[id]
		if provider.Validate() != nil {
			continue
		}
		providers = append(providers, gin.H{
			"id": provider.ID, "name": provider.Name, "protocol": provider.Protocol,
		})
	}
	c.JSON(http.StatusOK, gin.H{"providers": providers})
}

func (s *Server) startSocialLogin(c *gin.Context) {
	var req socialLoginStartRequest
	if !bind(c, &req) {
		return
	}
	provider, ok := s.socialProviders[strings.ToLower(strings.TrimSpace(req.ProviderID))]
	if !ok || provider.Validate() != nil {
		writeSocialLoginError(c, auth.ErrSocialProviderNotFound)
		return
	}
	providerRecord, err := s.ensureSocialProvider(c, provider)
	if err != nil {
		writeSocialLoginError(c, err)
		return
	}
	state, err := auth.RandomToken()
	if err != nil {
		writeError(c, http.StatusInternalServerError, "social_state_failed", "Could not create social login state")
		return
	}
	nonce, err := auth.RandomToken()
	if err != nil {
		writeError(c, http.StatusInternalServerError, "social_state_failed", "Could not create social login state")
		return
	}
	_, challenge, err := auth.DerivePKCE(state, s.socialStateSecret)
	if err != nil {
		writeSocialLoginError(c, err)
		return
	}
	next := safeSocialNext(req.Next)
	var nextPtr *string
	if next != "" {
		nextPtr = &next
	}
	if _, err := s.store.CreateOIDCLoginState(c.Request.Context(), store.OIDCLoginStateCreateInput{
		ProviderID: providerRecord.ID, StateHash: auth.HashToken(state), NonceHash: auth.HashToken(nonce),
		RedirectURL: provider.RedirectURL, PostLoginRedirectURL: nextPtr,
		ExpiresAt: s.now().Add(s.socialStateTTL), Now: s.now(),
	}); err != nil {
		writeStoreError(c, err)
		return
	}
	location, err := s.socialClient.AuthorizationURL(c.Request.Context(), provider, state, nonce, challenge)
	if err != nil {
		writeSocialLoginError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"redirect_url": location})
}

func (s *Server) handleSocialCallback(c *gin.Context) {
	var req socialLoginCallbackRequest
	if !bind(c, &req) {
		return
	}
	if strings.TrimSpace(req.Error) != "" {
		writeError(c, http.StatusBadRequest, "social_login_cancelled", "Social login was cancelled")
		return
	}
	state := strings.TrimSpace(req.State)
	code := strings.TrimSpace(req.Code)
	if state == "" || code == "" {
		writeError(c, http.StatusBadRequest, "invalid_social_state", "Invalid social login state")
		return
	}
	loginState, err := s.store.ConsumeOIDCLoginState(c.Request.Context(), auth.HashToken(state), s.now())
	if err != nil {
		writeSocialLoginError(c, err)
		return
	}
	providerRecord, err := s.store.GetIdentityProviderByID(c.Request.Context(), loginState.ProviderID)
	if err != nil {
		writeSocialLoginError(c, err)
		return
	}
	provider, ok := s.socialProviders[providerRecord.ProviderID]
	if !ok || provider.RedirectURL != loginState.RedirectURL {
		writeSocialLoginError(c, auth.ErrSocialProviderNotFound)
		return
	}
	verifier, _, err := auth.DerivePKCE(state, s.socialStateSecret)
	if err != nil {
		writeSocialLoginError(c, err)
		return
	}
	identity, err := s.socialClient.ExchangeAndIdentify(c.Request.Context(), provider, code, loginState.NonceHash, verifier)
	if err != nil {
		s.logger.Warn("social identity verification failed", zap.String("provider", provider.ID), zap.Error(err))
		writeSocialLoginError(c, err)
		return
	}
	user, err := s.resolveSocialUser(c, providerRecord, identity)
	if err != nil {
		writeSocialLoginError(c, err)
		return
	}
	tokens, err := s.issueTokens(c, user.ID)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "token_issue_failed", "Could not issue tokens")
		return
	}
	response, err := s.loginResponse(c.Request.Context(), user, tokens, "", false)
	if err != nil {
		writeAppCertificateError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"user": response.User, "tokens": response.Tokens, "app_certificate": response.AppCertificate,
		"return_path": safeSocialNext(stringValue(loginState.PostLoginRedirectURL)),
	})
}

func (s *Server) ensureSocialProvider(c *gin.Context, provider auth.SocialProvider) (model.IdentityProvider, error) {
	providerType := model.IdentityProviderTypeOIDC
	scopes := []string{"openid", "email", "profile"}
	secretName := "GOOGLE_OAUTH_CLIENT_SECRET"
	if provider.ID == "github" {
		providerType = model.IdentityProviderTypeOAuth2
		scopes = []string{"user:email"}
		secretName = "GITHUB_OAUTH_CLIENT_SECRET"
	}
	secretRef := "env:" + secretName
	existing, err := s.store.GetIdentityProviderByProviderID(c.Request.Context(), provider.ID)
	if err == nil {
		if existing.Type != providerType {
			return model.IdentityProvider{}, auth.ErrSocialProviderMisconfigured
		}
		name, issuer, clientID, enabled := provider.Name, provider.IssuerURL, provider.ClientID, true
		return s.store.UpdateIdentityProvider(c.Request.Context(), store.IdentityProviderUpdateInput{
			ProviderID: provider.ID, Name: &name, IssuerURL: &issuer, ClientID: &clientID,
			ClientSecretRef: &secretRef, Scopes: scopes, Enabled: &enabled,
			Metadata: map[string]any{"source": "environment", "social_login": true}, Now: s.now(),
		})
	}
	if !errors.Is(err, store.ErrNotFound) {
		return model.IdentityProvider{}, err
	}
	return s.store.CreateIdentityProvider(c.Request.Context(), store.IdentityProviderCreateInput{
		ProviderID: provider.ID, Name: provider.Name, Type: providerType, IssuerURL: provider.IssuerURL,
		ClientID: provider.ClientID, ClientSecretRef: &secretRef, Scopes: scopes, Enabled: true,
		Metadata: map[string]any{"source": "environment", "social_login": true}, Now: s.now(),
	})
}

func (s *Server) resolveSocialUser(c *gin.Context, provider model.IdentityProvider, identity auth.OIDCIdentity) (model.User, error) {
	if !identity.EmailVerified || strings.TrimSpace(identity.Email) == "" {
		return model.User{}, auth.ErrSocialEmailUnverified
	}
	linked, err := s.store.GetUserIdentityByProviderSubject(c.Request.Context(), provider.ID, identity.Subject)
	if err == nil {
		user, getErr := s.store.GetUser(c.Request.Context(), linked.UserID)
		if getErr != nil || user.DisabledAt != nil {
			return model.User{}, errOIDCUserNotProvisioned
		}
		if user.SignupPendingVerification || !user.EmailVerified {
			if !strings.EqualFold(strings.TrimSpace(user.Email), strings.TrimSpace(identity.Email)) {
				return model.User{}, errOIDCUserNotProvisioned
			}
			user, getErr = s.store.ActivateUserFromVerifiedSocialEmail(c.Request.Context(), user.ID, provider.ProviderID)
			if getErr != nil {
				return model.User{}, getErr
			}
		}
		_, err = s.store.UpdateUserIdentityLastLogin(c.Request.Context(), linked.ID, s.now())
		return user, err
	}
	if !errors.Is(err, store.ErrNotFound) {
		return model.User{}, err
	}
	email := strings.ToLower(strings.TrimSpace(identity.Email))
	user, err := s.store.GetUserByEmail(c.Request.Context(), email)
	if errors.Is(err, store.ErrNotFound) {
		passwordSeed, randomErr := auth.RandomToken()
		if randomErr != nil {
			return model.User{}, randomErr
		}
		passwordHash, hashErr := auth.HashPassword(passwordSeed)
		if hashErr != nil {
			return model.User{}, hashErr
		}
		displayName := socialDisplayName(identity.Claims)
		created, createErr := s.store.SignupDeveloper(c.Request.Context(), store.DeveloperSignupInput{
			Email: email, PasswordHash: passwordHash, DisplayName: displayName,
			OrganizationName: email, EmailVerified: true, SignupPendingVerification: false,
		})
		if createErr == nil {
			user = created.User
			err = nil
		} else if errors.Is(createErr, store.ErrConflict) {
			user, err = s.store.GetUserByEmail(c.Request.Context(), email)
		} else {
			return model.User{}, createErr
		}
	}
	if err != nil || user.DisabledAt != nil {
		return model.User{}, errOIDCUserNotProvisioned
	}
	if user.SignupPendingVerification || !user.EmailVerified {
		user, err = s.store.ActivateUserFromVerifiedSocialEmail(c.Request.Context(), user.ID, provider.ProviderID)
		if err != nil {
			return model.User{}, err
		}
	}
	if _, err := s.store.CreateUserIdentity(c.Request.Context(), store.UserIdentityCreateInput{
		UserID: user.ID, ProviderID: provider.ID, IssuerURL: identity.Issuer, Subject: identity.Subject,
		Email: strings.ToLower(strings.TrimSpace(identity.Email)), EmailVerified: true, Claims: identity.Claims, Now: s.now(),
	}); err != nil {
		return model.User{}, err
	}
	return user, nil
}

func socialDisplayName(claims map[string]any) *string {
	for _, key := range []string{"name", "login"} {
		if value, ok := claims[key].(string); ok && strings.TrimSpace(value) != "" {
			trimmed := strings.TrimSpace(value)
			return &trimmed
		}
	}
	return nil
}

func writeSocialLoginError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, auth.ErrSocialProviderNotFound):
		writeError(c, http.StatusNotFound, "social_provider_not_found", "Social login provider is unavailable")
	case errors.Is(err, auth.ErrSocialProviderMisconfigured):
		writeError(c, http.StatusServiceUnavailable, "social_provider_misconfigured", "Social login provider is misconfigured")
	case errors.Is(err, store.ErrOIDCStateInvalid), errors.Is(err, store.ErrOIDCStateExpired):
		writeError(c, http.StatusBadRequest, "invalid_social_state", "Invalid or expired social login state")
	case errors.Is(err, auth.ErrSocialEmailUnverified), errors.Is(err, auth.ErrUnverifiedOIDCEmail):
		writeError(c, http.StatusForbidden, "social_email_unverified", "A verified primary email is required")
	case errors.Is(err, errOIDCUserNotProvisioned):
		writeError(c, http.StatusForbidden, "user_not_provisioned", "No eligible Connect+ account matches this identity")
	case errors.Is(err, auth.ErrInvalidSocialIdentity), errors.Is(err, auth.ErrInvalidOIDCToken):
		writeError(c, http.StatusUnauthorized, "invalid_social_identity", "Social identity could not be verified")
	default:
		writeStoreError(c, err)
	}
}

func safeSocialNext(raw string) string {
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	if err != nil || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || strings.Contains(raw, `\`) || parsed.Scheme != "" || parsed.Host != "" {
		return ""
	}
	cleaned := path.Clean(parsed.EscapedPath())
	if cleaned != "/admin" && !strings.HasPrefix(cleaned, "/admin/") && cleaned != "/console" && !strings.HasPrefix(cleaned, "/console/") {
		return ""
	}
	return cleaned
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
