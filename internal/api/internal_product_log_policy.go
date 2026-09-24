package api

import (
	"net/http"
	"slices"

	"rtk_account_manager/internal/model"

	"github.com/gin-gonic/gin"
)

// Device log policy is read from the Product authority, never from a device payload.
func (s *Server) getInternalProductLogPolicy(c *gin.Context) {
	if !s.requireInternalAuthToken(c) {
		return
	}
	profile, err := s.store.GetDeviceItemProfile(c.Request.Context(), c.Param("brandCloudId"), c.Param("productId"))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	if profile.Status != model.DeviceItemProfileStatusActive || !slices.Contains(profile.ServiceOptions, "device_logging") || profile.LogRetentionDays == nil {
		c.JSON(http.StatusOK, gin.H{"active": profile.Status == model.DeviceItemProfileStatusActive, "enabled": false, "product_id": profile.ID})
		return
	}
	c.JSON(http.StatusOK, gin.H{"active": true, "enabled": true, "product_id": profile.ID, "log_retention_days": *profile.LogRetentionDays, "updated_at": profile.UpdatedAt})
}
