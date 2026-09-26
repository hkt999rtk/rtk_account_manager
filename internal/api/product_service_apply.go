package api

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

type admitProductServiceApplyRequest struct {
	JobID        string `json:"job_id" binding:"required"`
	PreviewToken string `json:"preview_token" binding:"required"`
}

func (s *Server) previewProductServiceApply(c *gin.Context) {
	preview, err := s.store.PreviewProductServiceApply(c.Request.Context(), currentUserID(c), c.Param("orgId"), c.Param("profileId"))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, preview)
}

func (s *Server) admitProductServiceApply(c *gin.Context) {
	var req admitProductServiceApplyRequest
	if !bindStrict(c, &req) {
		return
	}
	job, err := s.store.AdmitProductServiceApply(c.Request.Context(), currentUserID(c), c.Param("orgId"), c.Param("profileId"), strings.TrimSpace(req.JobID), strings.TrimSpace(req.PreviewToken))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"job": job})
}

func (s *Server) getProductServiceApplyJob(c *gin.Context) {
	job, err := s.store.GetProductServiceApplyJob(c.Request.Context(), currentUserID(c), c.Param("orgId"), c.Param("profileId"), c.Param("jobId"))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"job": job})
}

func (s *Server) listProductServiceApplyItems(c *gin.Context) {
	limit, offset := pagination(c)
	page, err := s.store.ListProductServiceApplyItems(c.Request.Context(), currentUserID(c), c.Param("orgId"), c.Param("profileId"), c.Param("jobId"), limit, offset)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, page)
}

func (s *Server) getProductServiceApplyItem(c *gin.Context) {
	item, err := s.store.GetProductServiceApplyItem(c.Request.Context(), currentUserID(c), c.Param("orgId"), c.Param("profileId"), c.Param("jobId"), c.Param("deviceId"))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"item": item})
}

func (s *Server) dispatchProductServiceApplyItem(c *gin.Context) {
	// Keep an explicit but empty body contract, allowing future additive fields.
	var req struct{}
	if !bindStrict(c, &req) {
		return
	}
	messageID, err := newOpaqueID()
	if err != nil {
		writeError(c, http.StatusInternalServerError, "id_generation_failed", "Could not generate message id")
		return
	}
	item, err := s.store.DispatchProductServiceApplyItem(c.Request.Context(), currentUserID(c), c.Param("orgId"), c.Param("profileId"), c.Param("jobId"), c.Param("deviceId"), messageID)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	status := http.StatusAccepted
	if item.Status == "applied" {
		status = http.StatusOK
	}
	c.JSON(status, gin.H{"item": item})
}

func (s *Server) cancelProductServiceApply(c *gin.Context) { s.finishProductServiceApply(c, "cancel") }
func (s *Server) completeProductServiceApply(c *gin.Context) {
	s.finishProductServiceApply(c, "complete")
}

func (s *Server) finishProductServiceApply(c *gin.Context, action string) {
	var req struct{}
	if !bindStrict(c, &req) {
		return
	}
	job, err := s.store.FinishProductServiceApply(c.Request.Context(), currentUserID(c), c.Param("orgId"), c.Param("profileId"), c.Param("jobId"), action)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"job": job})
}
