package api

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// Product OTA access is resolved by Account Manager on every service request.
// The caller must authenticate as a trusted internal workload.
func (s *Server) getInternalProductOTAGrant(c *gin.Context) {
	if !s.requireInternalAuthToken(c) {
		return
	}
	grant, err := s.store.GetProductOTAGrant(
		c.Request.Context(), c.Param("brandCloudId"), c.Param("productId"),
	)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{
		"brand_cloud_id":           grant.BrandCloudID,
		"product_id":               grant.ProductID,
		"service_code":             "ota",
		"active":                   grant.Active,
		"enabled":                  grant.Enabled,
		"product_service_revision": grant.ProductServiceRevision,
		"service_grant_sha256":     grant.ServiceGrantSHA256,
	})
}

// Historical revisions let Billing check a receipt's original Product grant
// after the Product has been edited or disabled. The endpoint never derives
// historical eligibility from the Product's current service options.
func (s *Server) getInternalHistoricalProductOTAGrant(c *gin.Context) {
	if !s.requireInternalAuthToken(c) {
		return
	}
	revision, err := strconv.ParseInt(c.Param("revision"), 10, 64)
	if err != nil || revision < 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid Product service revision"})
		return
	}
	grant, err := s.store.GetHistoricalProductOTAGrant(
		c.Request.Context(), c.Param("brandCloudId"), c.Param("productId"), revision,
	)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, grant)
}
