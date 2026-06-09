package api

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/store"
)

type internalAppTokenAuthorizationRequest struct {
	UserID      string `json:"user_id,omitempty"`
	EndUserID   string `json:"end_user_id,omitempty"`
	SubjectType string `json:"subject_type,omitempty"`
	Devid       string `json:"devid" binding:"required"`
}

func (s *Server) handleInternalAppTokenAuthorization(c *gin.Context) {
	if !s.requireInternalAuthToken(c) {
		return
	}
	var req internalAppTokenAuthorizationRequest
	if !bind(c, &req) {
		return
	}
	subjectType := auth.SubjectType(strings.TrimSpace(req.SubjectType))
	var err error
	switch subjectType {
	case "", auth.SubjectTypePlatformUser:
		userID := strings.TrimSpace(req.UserID)
		if userID == "" {
			writeError(c, http.StatusBadRequest, "missing_user_id", "user_id is required")
			return
		}
		err = s.store.AuthorizeUserForVideoDevice(c.Request.Context(), userID, strings.TrimSpace(req.Devid))
	case auth.SubjectTypeEndUser:
		endUserID := strings.TrimSpace(req.EndUserID)
		if endUserID == "" {
			writeError(c, http.StatusBadRequest, "missing_end_user_id", "end_user_id is required")
			return
		}
		err = s.store.AuthorizeEndUserForVideoDevice(c.Request.Context(), endUserID, strings.TrimSpace(req.Devid))
	default:
		writeError(c, http.StatusBadRequest, "unsupported_subject_type", "Unsupported subject_type")
		return
	}
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(c, http.StatusForbidden, "app_token_not_authorized", "User is not authorized for device")
			return
		}
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"authorized": true})
}

func (s *Server) requireInternalAuthToken(c *gin.Context) bool {
	if s.internalAuthToken == "" {
		writeError(c, http.StatusServiceUnavailable, "internal_auth_unconfigured", "Internal authorization is not configured")
		return false
	}
	header := strings.TrimSpace(c.GetHeader("Authorization"))
	expected := "Bearer " + s.internalAuthToken
	if subtle.ConstantTimeCompare([]byte(header), []byte(expected)) != 1 {
		writeError(c, http.StatusUnauthorized, "unauthorized", "Unauthorized")
		return false
	}
	return true
}
