package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/store"
)

type jobAuthorizationPersistence interface {
	CreateJobAuthorization(context.Context, store.JobAuthorizationInput, time.Time) (store.JobAuthorization, error)
	ValidateJobAuthorization(context.Context, string, time.Time) (store.JobAuthorization, error)
	RevokeJobAuthorization(context.Context, string, time.Time) (store.JobAuthorization, error)
}

type createJobAuthorizationRequest struct {
	JobID      string    `json:"job_id" binding:"required"`
	ScopeHash  string    `json:"scope_hash" binding:"required"`
	Capability string    `json:"capability" binding:"required"`
	ProductIDs []string  `json:"product_ids" binding:"required"`
	ExpiresAt  time.Time `json:"expires_at" binding:"required"`
}

func (s *Server) createDeveloperJobAuthorization(c *gin.Context) {
	if currentSubjectType(c) != auth.SubjectTypeUser || s.jobAuthorizations == nil {
		writeError(c, http.StatusServiceUnavailable, "job_authorization_unavailable", "Job authorization is unavailable")
		return
	}
	var req createJobAuthorizationRequest
	if !bindStrict(c, &req) {
		return
	}
	scopeHash := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(req.ScopeHash)), "sha256:")
	grant, err := s.jobAuthorizations.CreateJobAuthorization(c.Request.Context(), store.JobAuthorizationInput{
		JobID: req.JobID, BrandCloudID: c.Param("brandCloudId"), ActorUserID: currentUserID(c), ScopeHash: scopeHash,
		Capability: req.Capability, ProductIDs: req.ProductIDs, ExpiresAt: req.ExpiresAt,
	}, time.Now().UTC())
	if err != nil {
		writeJobAuthorizationError(c, err)
		return
	}
	grant.ScopeHash = "sha256:" + grant.ScopeHash
	c.JSON(http.StatusCreated, grant)
}

type exchangeJobAuthorizationRequest struct {
	JobID     string `json:"job_id" binding:"required"`
	ScopeHash string `json:"scope_hash" binding:"required"`
}

func (s *Server) exchangeJobAuthorization(c *gin.Context) {
	if !s.requireJobAuthorizationToken(c) {
		return
	}
	var req exchangeJobAuthorizationRequest
	if !bindStrict(c, &req) {
		return
	}
	grant, err := s.jobAuthorizations.ValidateJobAuthorization(c.Request.Context(), c.Param("authorizationId"), time.Now().UTC())
	if err != nil {
		writeJobAuthorizationError(c, err)
		return
	}
	if grant.JobID != strings.TrimSpace(req.JobID) || grant.ScopeHash != strings.TrimPrefix(strings.ToLower(strings.TrimSpace(req.ScopeHash)), "sha256:") {
		writeError(c, http.StatusConflict, "job_authorization_scope_mismatch", "Job authorization scope does not match")
		return
	}
	token, expiresAt, err := s.auth.IssueDelegatedJobAccessToken(auth.Claims{
		UserID: grant.ActorUserID, BrandCloudID: grant.BrandCloudID, JobAuthorizationID: grant.ID, JobID: grant.JobID,
		ScopeHash: grant.ScopeHash, Capability: grant.Capability, ProductIDs: grant.ProductIDs,
		AuthorizationVersion: grant.AuthorizationVersion, OwnershipVersion: grant.OwnershipVersion,
	})
	if err != nil {
		writeError(c, http.StatusInternalServerError, "token_issue_failed", "Could not issue job token")
		return
	}
	c.JSON(http.StatusOK, gin.H{"access_token": token, "token_type": "Bearer", "expires_in": int(time.Until(expiresAt).Seconds()), "authorization_version": grant.AuthorizationVersion, "ownership_version": grant.OwnershipVersion})
}

func (s *Server) revokeJobAuthorization(c *gin.Context) {
	if !s.requireJobAuthorizationToken(c) {
		return
	}
	if _, err := s.jobAuthorizations.RevokeJobAuthorization(c.Request.Context(), c.Param("authorizationId"), time.Now().UTC()); err != nil {
		writeJobAuthorizationError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (s *Server) requireJobAuthorizationToken(c *gin.Context) bool {
	if s.jobAuthorizations == nil || s.jobAuthorizationToken == "" {
		writeError(c, http.StatusServiceUnavailable, "job_authorization_unconfigured", "Job authorization is not configured")
		return false
	}
	expected := "Bearer " + s.jobAuthorizationToken
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(c.GetHeader("Authorization"))), []byte(expected)) != 1 {
		writeError(c, http.StatusUnauthorized, "unauthorized", "Unauthorized")
		return false
	}
	return true
}

func (s *Server) validateDelegatedJobRequest(c *gin.Context, claims *auth.Claims) bool {
	if s.jobAuthorizations == nil {
		writeError(c, http.StatusUnauthorized, "invalid_token", "Invalid bearer token")
		return false
	}
	grant, err := s.jobAuthorizations.ValidateJobAuthorization(c.Request.Context(), claims.JobAuthorizationID, time.Now().UTC())
	if err != nil || grant.JobID != claims.JobID || grant.BrandCloudID != claims.BrandCloudID || grant.ActorUserID != claims.UserID || grant.ScopeHash != claims.ScopeHash || grant.Capability != claims.Capability || grant.AuthorizationVersion != claims.AuthorizationVersion || grant.OwnershipVersion != claims.OwnershipVersion {
		writeError(c, http.StatusUnauthorized, "job_authorization_revoked", "Job authorization is no longer valid")
		return false
	}
	path := c.FullPath()
	allowedPath := path == "/v1/orgs/:orgId/devices/:deviceId/provision" || path == "/v1/orgs/:orgId/devices/:deviceId/provisioning" || path == "/v1/orgs/:orgId/devices/:deviceId" || path == "/v1/orgs/:orgId/device-item-profiles/:profileId"
	if claims.Capability != "provisioning.create" || c.Param("orgId") != claims.BrandCloudID || !allowedPath {
		writeError(c, http.StatusForbidden, "job_scope_forbidden", "Job token cannot access this operation")
		return false
	}
	resourceProductID := c.Param("profileId")
	if deviceID := c.Param("deviceId"); deviceID != "" {
		device, err := s.store.GetDevice(c.Request.Context(), claims.BrandCloudID, deviceID)
		if err != nil || device.DeviceItemProfileID == nil {
			writeError(c, http.StatusNotFound, "not_found", "Resource not found")
			return false
		}
		resourceProductID = *device.DeviceItemProfileID
	}
	if resourceProductID == "" || !slices.Contains(claims.ProductIDs, resourceProductID) {
		writeError(c, http.StatusNotFound, "not_found", "Resource not found")
		return false
	}
	return true
}

func writeJobAuthorizationError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrJobAuthorizationRevoked):
		writeError(c, http.StatusGone, "job_authorization_revoked", "Job authorization is no longer valid")
	case errors.Is(err, store.ErrNotFound):
		writeError(c, http.StatusNotFound, "not_found", "Resource not found")
	case errors.Is(err, store.ErrConflict):
		writeError(c, http.StatusConflict, "job_authorization_conflict", "Job authorization request conflicts with its immutable scope")
	default:
		writeStoreError(c, err)
	}
}
