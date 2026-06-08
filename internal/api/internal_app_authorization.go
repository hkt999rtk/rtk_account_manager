package api

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/store"
)

type internalAppTokenAuthorizationRequest struct {
	UserID string `json:"user_id" binding:"required"`
	Devid  string `json:"devid" binding:"required"`
}

func (s *Server) handleInternalAppTokenAuthorization(c *gin.Context) {
	if !s.requireInternalAuthToken(c) {
		return
	}
	var req internalAppTokenAuthorizationRequest
	if !bind(c, &req) {
		return
	}
	err := s.store.AuthorizeUserForVideoDevice(c.Request.Context(), strings.TrimSpace(req.UserID), strings.TrimSpace(req.Devid))
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
