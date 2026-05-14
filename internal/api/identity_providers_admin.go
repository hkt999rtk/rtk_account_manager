package api

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"

	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

var clientSecretRefPattern = regexp.MustCompile(`^env:[A-Za-z_][A-Za-z0-9_]*$`)

type identityProviderCreateRequest struct {
	ProviderID      string         `json:"provider_id" binding:"required"`
	Name            string         `json:"name" binding:"required"`
	IssuerURL       string         `json:"issuer_url" binding:"required"`
	ClientID        string         `json:"client_id" binding:"required"`
	ClientSecretRef string         `json:"client_secret_ref" binding:"required"`
	Scopes          []string       `json:"scopes"`
	Enabled         bool           `json:"enabled"`
	Metadata        map[string]any `json:"metadata"`
}

type identityProviderUpdateRequest struct {
	Name            *string        `json:"name"`
	IssuerURL       *string        `json:"issuer_url"`
	ClientID        *string        `json:"client_id"`
	ClientSecretRef *string        `json:"client_secret_ref"`
	Scopes          []string       `json:"scopes"`
	Enabled         *bool          `json:"enabled"`
	Metadata        map[string]any `json:"metadata"`
}

type identityProviderResponse struct {
	IdentityProvider model.IdentityProvider `json:"identity_provider"`
}

type identityProvidersResponse struct {
	IdentityProviders []model.IdentityProvider `json:"identity_providers"`
	Pagination        store.Page               `json:"pagination"`
}

func (s *Server) createIdentityProvider(c *gin.Context) {
	var req identityProviderCreateRequest
	if !bindStrict(c, &req) {
		return
	}
	if !validateIdentityProviderCreateRequest(c, req) {
		return
	}
	secretRef := strings.TrimSpace(req.ClientSecretRef)
	provider, err := s.store.CreateIdentityProvider(c.Request.Context(), store.IdentityProviderCreateInput{
		ProviderID:      strings.TrimSpace(req.ProviderID),
		Name:            strings.TrimSpace(req.Name),
		Type:            model.IdentityProviderTypeOIDC,
		IssuerURL:       strings.TrimSpace(req.IssuerURL),
		ClientID:        strings.TrimSpace(req.ClientID),
		ClientSecretRef: &secretRef,
		Scopes:          cleanScopes(req.Scopes),
		Enabled:         req.Enabled,
		Metadata:        req.Metadata,
		Now:             time.Now().UTC(),
	})
	if err != nil {
		writeIdentityProviderStoreError(c, err)
		return
	}
	if err := s.auditIdentityProvider(c, "identity_provider_created", provider); err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusCreated, identityProviderResponse{IdentityProvider: provider})
}

func (s *Server) listIdentityProviders(c *gin.Context) {
	limit, offset := pagination(c)
	page, err := s.store.ListIdentityProviders(c.Request.Context(), store.IdentityProviderListFilter{Limit: limit, Offset: offset})
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, identityProvidersResponse{IdentityProviders: page.Providers, Pagination: page.Page})
}

func (s *Server) getIdentityProvider(c *gin.Context) {
	provider, err := s.store.GetIdentityProviderByProviderID(c.Request.Context(), c.Param("providerId"))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, identityProviderResponse{IdentityProvider: provider})
}

func (s *Server) updateIdentityProvider(c *gin.Context) {
	var req identityProviderUpdateRequest
	if !bindStrict(c, &req) {
		return
	}
	if !validateIdentityProviderUpdateRequest(c, req) {
		return
	}
	in := store.IdentityProviderUpdateInput{
		ProviderID: strings.TrimSpace(c.Param("providerId")),
		Scopes:     cleanScopes(req.Scopes),
		Enabled:    req.Enabled,
		Metadata:   req.Metadata,
		Now:        time.Now().UTC(),
	}
	if req.Name != nil {
		value := strings.TrimSpace(*req.Name)
		in.Name = &value
	}
	if req.IssuerURL != nil {
		value := strings.TrimSpace(*req.IssuerURL)
		in.IssuerURL = &value
	}
	if req.ClientID != nil {
		value := strings.TrimSpace(*req.ClientID)
		in.ClientID = &value
	}
	if req.ClientSecretRef != nil {
		value := strings.TrimSpace(*req.ClientSecretRef)
		in.ClientSecretRef = &value
	}
	provider, err := s.store.UpdateIdentityProvider(c.Request.Context(), in)
	if err != nil {
		writeIdentityProviderStoreError(c, err)
		return
	}
	if err := s.auditIdentityProvider(c, "identity_provider_updated", provider); err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, identityProviderResponse{IdentityProvider: provider})
}

func (s *Server) deleteIdentityProvider(c *gin.Context) {
	provider, err := s.store.DisableIdentityProvider(c.Request.Context(), c.Param("providerId"), time.Now().UTC())
	if err != nil {
		writeStoreError(c, err)
		return
	}
	if err := s.auditIdentityProvider(c, "identity_provider_disabled", provider); err != nil {
		writeStoreError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func validateIdentityProviderCreateRequest(c *gin.Context, req identityProviderCreateRequest) bool {
	if !requireNonBlank(c, "provider_id", req.ProviderID) ||
		!requireNonBlank(c, "name", req.Name) ||
		!requireNonBlank(c, "issuer_url", req.IssuerURL) ||
		!requireNonBlank(c, "client_id", req.ClientID) {
		return false
	}
	if !validateClientSecretRef(c, req.ClientSecretRef) {
		return false
	}
	return validateScopes(c, req.Scopes)
}

func validateIdentityProviderUpdateRequest(c *gin.Context, req identityProviderUpdateRequest) bool {
	if req.Name == nil && req.IssuerURL == nil && req.ClientID == nil && req.ClientSecretRef == nil && req.Enabled == nil && req.Metadata == nil && req.Scopes == nil {
		writeError(c, http.StatusBadRequest, "invalid_request", "request must include at least one field")
		return false
	}
	if req.Name != nil && !requireNonBlank(c, "name", *req.Name) {
		return false
	}
	if req.IssuerURL != nil && !requireNonBlank(c, "issuer_url", *req.IssuerURL) {
		return false
	}
	if req.ClientID != nil && !requireNonBlank(c, "client_id", *req.ClientID) {
		return false
	}
	if req.ClientSecretRef != nil && !validateClientSecretRef(c, *req.ClientSecretRef) {
		return false
	}
	return validateScopes(c, req.Scopes)
}

func validateClientSecretRef(c *gin.Context, ref string) bool {
	if !clientSecretRefPattern.MatchString(strings.TrimSpace(ref)) {
		writeError(c, http.StatusBadRequest, "invalid_client_secret_ref", "client_secret_ref must use env:VAR_NAME format")
		return false
	}
	return true
}

func validateScopes(c *gin.Context, scopes []string) bool {
	for _, scope := range scopes {
		if strings.TrimSpace(scope) == "" {
			writeError(c, http.StatusBadRequest, "invalid_request", "scopes must not include blank values")
			return false
		}
	}
	return true
}

func cleanScopes(scopes []string) []string {
	if scopes == nil {
		return nil
	}
	out := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		trimmed := strings.TrimSpace(scope)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func (s *Server) auditIdentityProvider(c *gin.Context, eventType string, provider model.IdentityProvider) error {
	return s.store.CreateAuditEvent(c.Request.Context(), store.AuditEventInput{
		EventType:   eventType,
		ActorUserID: stringPtr(currentUserID(c)),
		SubjectType: "identity_provider",
		SubjectID:   provider.ID,
		Payload: map[string]any{
			"provider_id": provider.ProviderID,
			"name":        provider.Name,
			"type":        provider.Type,
			"issuer_url":  provider.IssuerURL,
			"client_id":   provider.ClientID,
			"scopes":      provider.Scopes,
			"enabled":     provider.Enabled,
		},
	})
}

func writeIdentityProviderStoreError(c *gin.Context, err error) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		writeError(c, http.StatusConflict, "conflict", "Identity provider conflict")
		return
	}
	writeStoreError(c, err)
}
