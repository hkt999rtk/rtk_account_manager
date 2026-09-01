package api

import (
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"rtk_account_manager/internal/store"
)

func (s *Server) deleteDeveloperBrandCloud(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if !requireManagedCloudSession(c) {
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 1))
	if err != nil || len(raw) != 0 || len(c.Request.Header.Values("Idempotency-Key")) != 1 || !store.ValidManagedCloudKey(c.GetHeader("Idempotency-Key")) {
		writeError(c, 400, "invalid_request", "DELETE requires one Idempotency-Key and no request body")
		return
	}
	op, err := s.store.RequestDeveloperCloudDeletion(c.Request.Context(), currentUserID(c), c.Param("brandCloudId"), c.GetHeader("Idempotency-Key"))
	if err != nil {
		switch {
		case errors.Is(err, store.ErrCloudDeletionBlocked):
			writeError(c, 409, "cloud_deletion_blocked", "Refresh deletion preflight; empty resources and settled zero Billing are required")
		case errors.Is(err, store.ErrHandoffUnavailable):
			writeError(c, 503, "cloud_deletion_unavailable", "Verified resource producers and Billing closure are not configured")
		default:
			writeStoreError(c, err)
		}
		return
	}
	c.Header("Location", "/v1/developer/brand-clouds/"+op.CloudID+"/operations/"+op.ID)
	c.JSON(http.StatusAccepted, gin.H{"operation": op})
}
func (s *Server) getDeveloperCloudOperation(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if !requireManagedCloudSession(c) {
		return
	}
	op, err := s.store.GetDeveloperCloudDeletion(c.Request.Context(), currentUserID(c), c.Param("brandCloudId"), c.Param("operationId"))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"operation": op})
}
