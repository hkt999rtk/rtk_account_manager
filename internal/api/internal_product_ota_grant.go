package api

import (
	"net/http"

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
