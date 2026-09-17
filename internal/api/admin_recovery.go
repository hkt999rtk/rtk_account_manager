package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/store"
)

type adminRecoveryStore interface {
	AdminRecovery(context.Context, store.RecoveryPrincipal, store.RecoveryCommand, time.Time) (store.AdminRecovery, error)
}

func (s *Server) adminRecovery(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	claims, err := s.auth.ParseAccessToken(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
	if err != nil || claims.SubjectType != auth.SubjectTypeUser {
		writeError(c, 403, "recovery_denied", "Verified account authentication is required")
		return
	}
	backend, ok := s.store.(adminRecoveryStore)
	if !ok {
		writeError(c, 503, "recovery_unavailable", "Recovery storage is unavailable")
		return
	}
	var cmd store.RecoveryCommand
	if c.Request.Method == http.MethodPost {
		decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 8192))
		decoder.DisallowUnknownFields()
		var extra any
		if decoder.Decode(&cmd) != nil || decoder.Decode(&extra) != io.EOF {
			writeError(c, 400, "invalid_request", "Invalid recovery request")
			return
		}
	}
	cmd.ID = c.Param("requestId")
	cmd.Key = c.GetHeader("Idempotency-Key")
	if c.Request.Method == "GET" {
		cmd.Action = "get"
		if cmd.ID == "" {
			cmd.Action = "access"
		}
	} else if cmd.ID == "" {
		cmd.Action = "create"
	} else {
		cmd.Action = c.Param("action")
	}
	requireUserMFA, err := pkiUserMFASetting(os.Getenv("PKI_REQUIRE_USER_MFA"))
	if err != nil {
		writeError(c, 503, "recovery_unavailable", "Invalid user authentication policy")
		return
	}
	result, err := backend.AdminRecovery(c.Request.Context(), store.RecoveryPrincipal{UserID: claims.UserID, MFA: claims.MFA, AuthTime: time.Unix(claims.AuthenticationTime, 0), AuthenticatedWithoutMFA: !requireUserMFA}, cmd, s.now())
	if errors.Is(err, store.ErrRecoveryDenied) {
		writeError(c, 403, "recovery_denied", "Authentication policy, independent approvals, and active recovery roles must be satisfied")
		return
	}
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(200, result)
}
