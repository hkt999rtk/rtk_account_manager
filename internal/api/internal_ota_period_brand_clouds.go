package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// The producer seal must include Clouds with no OTA rows, including disabled
// Clouds whose earlier tasks or stored objects may remain billable.
func (s *Server) listInternalOTAPeriodBrandClouds(c *gin.Context) {
	if !s.requireInternalAuthToken(c) {
		return
	}
	month := c.Query("month")
	start, err := time.Parse("2006-01", month)
	if err != nil || start.Format("2006-01") != month {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid UTC month"})
		return
	}
	end := start.AddDate(0, 1, 0)
	if end.After(time.Now().UTC()) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "OTA period has not ended"})
		return
	}
	ids, err := s.store.ListOTAPeriodSealBrandCloudIDs(c.Request.Context(), end)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"month": month, "brand_cloud_ids": ids})
}
