package api

import (
	"github.com/gin-gonic/gin"
	"net/http"
)

func (s *Server) preflightDeveloperBrandCloudDeletion(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if !requireManagedCloudSession(c) {
		return
	}
	result, err := s.store.PreflightDeveloperBrandCloudDeletion(c.Request.Context(), currentUserID(c), c.Param("brandCloudId"))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}
