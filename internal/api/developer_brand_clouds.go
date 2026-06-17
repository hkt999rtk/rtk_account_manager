package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

type brandCloudOwnerTransferRequest struct {
	TargetEmail string `json:"target_email" binding:"required,email"`
}

type brandCloudOwnerTransferAcceptRequest struct {
	Token string `json:"token" binding:"required"`
}

type brandCloudOwnerTransferResponse struct {
	OwnerTransfer model.BrandCloudOwnerTransfer `json:"owner_transfer"`
}

func (s *Server) listDeveloperBrandClouds(c *gin.Context) {
	limit, offset := pagination(c)
	page, err := s.store.ListDeveloperBrandClouds(c.Request.Context(), currentUserID(c), limit, offset)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"brand_clouds": page.Organizations, "pagination": page.Page})
}

func (s *Server) createDeveloperBrandCloud(c *gin.Context) {
	var req brandCloudRequest
	if !bind(c, &req) {
		return
	}
	if !requireNonBlank(c, "name", req.Name) {
		return
	}
	org, err := s.store.CreateDeveloperBrandCloud(c.Request.Context(), currentUserID(c), store.BrandCloudInput{
		Name:       strings.TrimSpace(req.Name),
		TenantSlug: strings.TrimSpace(req.TenantSlug),
		Metadata:   req.Metadata,
	})
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"brand_cloud": org})
}

func (s *Server) createBrandCloudOwnerTransfer(c *gin.Context) {
	var req brandCloudOwnerTransferRequest
	if !bind(c, &req) {
		return
	}
	token, expiresAt, err := s.newAuthToken()
	if err != nil {
		writeError(c, http.StatusInternalServerError, "token_issue_failed", "Could not issue owner transfer token")
		return
	}
	targetEmail := strings.ToLower(strings.TrimSpace(req.TargetEmail))
	transfer, err := s.store.CreateBrandCloudOwnerTransfer(c.Request.Context(), store.BrandCloudOwnerTransferInput{
		BrandCloudID:      c.Param("brandCloudId"),
		RequestedByUserID: currentUserID(c),
		TargetEmail:       targetEmail,
		TokenHash:         auth.HashToken(token),
		ExpiresAt:         expiresAt,
	})
	if err != nil {
		writeStoreError(c, err)
		return
	}
	if err := s.deliverAuthToken(c, targetEmail, "brand_cloud_owner_transfer", token, expiresAt); err != nil {
		writeError(c, http.StatusInternalServerError, "token_delivery_failed", "Could not deliver owner transfer token")
		return
	}
	c.JSON(http.StatusAccepted, brandCloudOwnerTransferResponse{OwnerTransfer: transfer})
}

func (s *Server) acceptBrandCloudOwnerTransfer(c *gin.Context) {
	var req brandCloudOwnerTransferAcceptRequest
	if !bind(c, &req) {
		return
	}
	transfer, err := s.store.AcceptBrandCloudOwnerTransfer(c.Request.Context(), currentUserID(c), auth.HashToken(req.Token), time.Now().UTC())
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, brandCloudOwnerTransferResponse{OwnerTransfer: transfer})
}
