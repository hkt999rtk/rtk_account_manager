package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/store"
)

type skuCollaboratorInvitationRequest struct {
	Email string `json:"email" binding:"required,email"`
	Role  string `json:"role" binding:"required"`
}

type skuCollaboratorRoleRequest struct {
	Role string `json:"role" binding:"required"`
}

type skuOwnerTransferRequest struct {
	TargetUserID string `json:"target_user_id" binding:"required"`
}

type skuCollaboratorInvitationAcceptRequest struct {
	Token string `json:"token" binding:"required"`
}

func validSKUCollaboratorRole(c *gin.Context, role string) bool {
	if role != store.SKUEditorRole && role != store.SKUViewerRole {
		writeError(c, http.StatusBadRequest, "invalid_role", "SKU collaborator role must be sku_editor or sku_viewer")
		return false
	}
	return true
}

func (s *Server) requireSKUCollaboratorManager(c *gin.Context) bool {
	allowed, err := s.store.CanManageSKUCollaborators(c.Request.Context(), currentUserID(c), c.Param("brandCloudId"), c.Param("skuId"))
	if err != nil || !allowed {
		writeError(c, http.StatusNotFound, "not_found", "Resource not found")
		return false
	}
	return true
}

func (s *Server) listSKUCollaborators(c *gin.Context) {
	if !s.requireSKUCollaboratorManager(c) {
		return
	}
	items, err := s.store.ListSKUCollaborators(c.Request.Context(), c.Param("brandCloudId"), c.Param("skuId"))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"collaborators": items})
}

func (s *Server) inviteSKUCollaborator(c *gin.Context) {
	if !s.requireSKUCollaboratorManager(c) {
		return
	}
	if !s.requireEmailOutbox(c) {
		return
	}
	var req skuCollaboratorInvitationRequest
	if !bind(c, &req) || !validSKUCollaboratorRole(c, strings.TrimSpace(req.Role)) {
		return
	}
	token, expiresAt, err := s.newAuthToken("sku_collaborator_invitation")
	if err != nil {
		writeError(c, http.StatusInternalServerError, "token_issue_failed", "Could not issue invitation token")
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	outbox := authTokenEmailOutbox(email, "sku_collaborator_invitation", token, expiresAt)
	invitation, created, err := s.store.CreateSKUCollaboratorInvitation(c.Request.Context(), store.SKUCollaboratorInvitationInput{
		BrandCloudID: c.Param("brandCloudId"), SKUID: c.Param("skuId"), InvitedByUserID: currentUserID(c),
		TargetEmail: email, Role: strings.TrimSpace(req.Role), TokenHash: auth.HashToken(token), ExpiresAt: expiresAt, Email: &outbox,
	}, time.Now().UTC())
	if err != nil {
		writeStoreError(c, err)
		return
	}
	if created {
		s.notifyAuthTokenQueued(AuthTokenDelivery{Purpose: "sku_collaborator_invitation", Email: invitation.TargetEmail, Token: token, ExpiresAt: expiresAt})
	}
	c.JSON(http.StatusAccepted, gin.H{"invitation": invitation})
}

func (s *Server) listSKUCollaboratorInvitations(c *gin.Context) {
	if !s.requireSKUCollaboratorManager(c) {
		return
	}
	items, err := s.store.ListSKUCollaboratorInvitations(c.Request.Context(), c.Param("brandCloudId"), c.Param("skuId"), time.Now().UTC())
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"invitations": items})
}

func (s *Server) resendSKUCollaboratorInvitation(c *gin.Context) {
	if !s.requireSKUCollaboratorManager(c) {
		return
	}
	if !s.requireEmailOutbox(c) {
		return
	}
	token, expiresAt, err := s.newAuthToken("sku_collaborator_invitation")
	if err != nil {
		writeError(c, http.StatusInternalServerError, "token_issue_failed", "Could not issue invitation token")
		return
	}
	outbox := authTokenEmailOutbox("pending@example.invalid", "sku_collaborator_invitation", token, expiresAt)
	item, err := s.store.ResendSKUCollaboratorInvitation(c.Request.Context(), store.SKUCollaboratorInvitationMutation{BrandCloudID: c.Param("brandCloudId"), SKUID: c.Param("skuId"), InvitationID: c.Param("invitationId"), ActorUserID: currentUserID(c), TokenHash: auth.HashToken(token), ExpiresAt: expiresAt, Email: &outbox}, time.Now().UTC())
	if err != nil {
		writeStoreError(c, err)
		return
	}
	s.notifyAuthTokenQueued(AuthTokenDelivery{Purpose: "sku_collaborator_invitation", Email: item.TargetEmail, Token: token, ExpiresAt: expiresAt})
	c.JSON(http.StatusAccepted, gin.H{"invitation": item})
}

func (s *Server) cancelSKUCollaboratorInvitation(c *gin.Context) {
	if !s.requireSKUCollaboratorManager(c) {
		return
	}
	item, err := s.store.CancelSKUCollaboratorInvitation(c.Request.Context(), store.SKUCollaboratorInvitationMutation{BrandCloudID: c.Param("brandCloudId"), SKUID: c.Param("skuId"), InvitationID: c.Param("invitationId"), ActorUserID: currentUserID(c)}, time.Now().UTC())
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"invitation": item})
}

func (s *Server) acceptSKUCollaboratorInvitation(c *gin.Context) {
	var req skuCollaboratorInvitationAcceptRequest
	if !bind(c, &req) {
		return
	}
	invitation, err := s.store.AcceptSKUCollaboratorInvitation(c.Request.Context(), currentUserID(c), auth.HashToken(req.Token), time.Now().UTC())
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"invitation": invitation})
}

func (s *Server) updateSKUCollaborator(c *gin.Context) {
	var req skuCollaboratorRoleRequest
	if !bind(c, &req) || !validSKUCollaboratorRole(c, strings.TrimSpace(req.Role)) {
		return
	}
	item, err := s.store.UpdateSKUCollaborator(c.Request.Context(), currentUserID(c), c.Param("brandCloudId"), c.Param("skuId"), c.Param("userId"), strings.TrimSpace(req.Role))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"collaborator": item})
}

func (s *Server) removeSKUCollaborator(c *gin.Context) {
	if err := s.store.RemoveSKUCollaborator(c.Request.Context(), currentUserID(c), c.Param("brandCloudId"), c.Param("skuId"), c.Param("userId")); err != nil {
		writeStoreError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (s *Server) transferSKUOwnership(c *gin.Context) {
	var req skuOwnerTransferRequest
	if !bind(c, &req) || !requireNonBlank(c, "target_user_id", req.TargetUserID) {
		return
	}
	if err := s.store.TransferSKUOwnership(c.Request.Context(), currentUserID(c), c.Param("brandCloudId"), c.Param("skuId"), strings.TrimSpace(req.TargetUserID)); err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"sku_id": c.Param("skuId"), "owner_user_id": strings.TrimSpace(req.TargetUserID)})
}
