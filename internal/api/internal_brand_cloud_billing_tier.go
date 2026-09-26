package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// A trusted Billing workload uses this evidence for a complete UTC month.
// A current commercial tier alone cannot prove an earlier paid period.
func (s *Server) getInternalBrandCloudBillingTier(c *gin.Context) {
	if !s.requireInternalAuthToken(c) {
		return
	}
	start, startErr := time.Parse(time.RFC3339Nano, c.Query("period_start"))
	end, endErr := time.Parse(time.RFC3339Nano, c.Query("period_end"))
	if startErr != nil || endErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid UTC billing period"})
		return
	}
	start, end = start.UTC(), end.UTC()
	monthStart := time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, time.UTC)
	if !start.Equal(monthStart) || !end.Equal(monthStart.AddDate(0, 1, 0)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "billing period must be one complete UTC month"})
		return
	}
	result, err := s.store.GetBrandCloudBillingTierPeriod(c.Request.Context(), c.Param("brandCloudId"), start, end)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, result)
}
