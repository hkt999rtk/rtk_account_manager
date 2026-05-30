package api

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

type brandCloudRequest struct {
	Name     string         `json:"name,omitempty"`
	Status   string         `json:"status,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

type brandCloudMemberRequest struct {
	UserID string `json:"user_id" binding:"required"`
	Role   string `json:"role" binding:"required"`
}

type brandCloudUserRequest struct {
	Email          string  `json:"email" binding:"required,email"`
	Password       string  `json:"password" binding:"required,min=8"`
	DisplayName    *string `json:"display_name"`
	Role           string  `json:"role" binding:"required"`
	RotatePassword bool    `json:"rotate_password"`
}

func (s *Server) createBrandCloud(c *gin.Context) {
	var req brandCloudRequest
	if !bind(c, &req) {
		return
	}
	if !requireNonBlank(c, "name", req.Name) {
		return
	}
	org, err := s.store.CreateBrandCloud(c.Request.Context(), currentUserID(c), store.BrandCloudInput{
		Name:     strings.TrimSpace(req.Name),
		Metadata: req.Metadata,
	})
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"brand_cloud": org})
}

func (s *Server) listBrandClouds(c *gin.Context) {
	limit, offset := pagination(c)
	page, err := s.store.ListBrandClouds(c.Request.Context(), limit, offset)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"brand_clouds": page.Organizations, "pagination": page.Page})
}

func (s *Server) getBrandCloud(c *gin.Context) {
	org, err := s.store.GetBrandCloud(c.Request.Context(), c.Param("brandCloudId"))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"brand_cloud": org})
}

func (s *Server) updateBrandCloud(c *gin.Context) {
	var req brandCloudRequest
	if !bind(c, &req) {
		return
	}
	status := model.OrganizationStatus(strings.TrimSpace(req.Status))
	if status != "" && status != model.OrganizationStatusActive && status != model.OrganizationStatusDisabled {
		writeError(c, http.StatusBadRequest, "invalid_status", "status must be active or disabled")
		return
	}
	org, err := s.store.UpdateBrandCloud(c.Request.Context(), currentUserID(c), c.Param("brandCloudId"), store.BrandCloudInput{
		Name:     strings.TrimSpace(req.Name),
		Status:   status,
		Metadata: req.Metadata,
	})
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"brand_cloud": org})
}

func (s *Server) assignBrandCloudMember(c *gin.Context) {
	var req brandCloudMemberRequest
	if !bind(c, &req) {
		return
	}
	role := model.Role(strings.TrimSpace(req.Role))
	if role != model.RoleOwner && role != model.RoleAdmin && role != model.RoleMember {
		writeError(c, http.StatusBadRequest, "invalid_role", "Invalid role")
		return
	}
	member, err := s.store.AssignBrandCloudMember(c.Request.Context(), currentUserID(c), c.Param("brandCloudId"), strings.TrimSpace(req.UserID), role)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"member": member})
}

func (s *Server) createBrandCloudUser(c *gin.Context) {
	var req brandCloudUserRequest
	if !bind(c, &req) {
		return
	}
	role := model.Role(strings.TrimSpace(req.Role))
	if role != model.RoleOwner && role != model.RoleAdmin && role != model.RoleMember {
		writeError(c, http.StatusBadRequest, "invalid_role", "Invalid role")
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "password_hash_failed", "Could not hash password")
		return
	}
	result, err := s.store.CreateBrandCloudUser(c.Request.Context(), currentUserID(c), c.Param("brandCloudId"), store.BrandCloudUserInput{
		Email:          strings.ToLower(strings.TrimSpace(req.Email)),
		PasswordHash:   hash,
		DisplayName:    trimStringPtr(req.DisplayName),
		Role:           role,
		RotatePassword: req.RotatePassword,
	})
	if err != nil {
		writeStoreError(c, err)
		return
	}
	status := http.StatusOK
	if result.Action == "created" {
		status = http.StatusCreated
	}
	c.JSON(status, result)
}
