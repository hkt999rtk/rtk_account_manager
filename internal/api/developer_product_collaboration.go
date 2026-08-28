package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/store"
)

type productCollaboratorInvitationRequest struct {
	Email string `json:"email" binding:"required,email"`
	Role  string `json:"role" binding:"required"`
}

type productCollaboratorRoleRequest struct {
	Role string `json:"role" binding:"required"`
}

type productOwnerTransferRequest struct {
	TargetUserID string `json:"target_user_id" binding:"required"`
}

type productCollaboratorInvitationAcceptRequest struct {
	Token string `json:"token" binding:"required"`
}

func validProductCollaboratorRole(c *gin.Context, role string) bool {
	if role != store.ProductEditorRole && role != store.ProductViewerRole {
		writeError(c, http.StatusBadRequest, "invalid_role", "Product collaborator role must be product_editor or product_viewer")
		return false
	}
	return true
}

func (s *Server) requireProductCollaboratorManager(c *gin.Context) bool {
	allowed, err := s.store.CanManageProductCollaborators(c.Request.Context(), currentUserID(c), c.Param("brandCloudId"), c.Param("productId"))
	if err != nil || !allowed {
		writeError(c, http.StatusNotFound, "not_found", "Resource not found")
		return false
	}
	return true
}

func (s *Server) listProductCollaborators(c *gin.Context) {
	if !s.requireProductCollaboratorManager(c) {
		return
	}
	items, err := s.store.ListProductCollaborators(c.Request.Context(), c.Param("brandCloudId"), c.Param("productId"))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"collaborators": items})
}

func (s *Server) inviteProductCollaborator(c *gin.Context) {
	if !s.requireProductCollaboratorManager(c) {
		return
	}
	if !s.requireEmailOutbox(c) {
		return
	}
	var req productCollaboratorInvitationRequest
	if !bind(c, &req) || !validProductCollaboratorRole(c, strings.TrimSpace(req.Role)) {
		return
	}
	token, expiresAt, err := s.newAuthToken("product_collaborator_invitation")
	if err != nil {
		writeError(c, http.StatusInternalServerError, "token_issue_failed", "Could not issue invitation token")
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	outbox := authTokenEmailOutbox(email, "product_collaborator_invitation", token, expiresAt)
	invitation, created, err := s.store.CreateProductCollaboratorInvitation(c.Request.Context(), store.ProductCollaboratorInvitationInput{
		BrandCloudID: c.Param("brandCloudId"), ProductID: c.Param("productId"), InvitedByUserID: currentUserID(c),
		TargetEmail: email, Role: strings.TrimSpace(req.Role), TokenHash: auth.HashToken(token), ExpiresAt: expiresAt, Email: &outbox,
	}, time.Now().UTC())
	if err != nil {
		writeStoreError(c, err)
		return
	}
	if created {
		s.notifyAuthTokenQueued(AuthTokenDelivery{Purpose: "product_collaborator_invitation", Email: invitation.TargetEmail, Token: token, ExpiresAt: expiresAt})
	}
	c.JSON(http.StatusAccepted, gin.H{"invitation": invitation})
}

func (s *Server) listProductCollaboratorInvitations(c *gin.Context) {
	if !s.requireProductCollaboratorManager(c) {
		return
	}
	items, err := s.store.ListProductCollaboratorInvitations(c.Request.Context(), c.Param("brandCloudId"), c.Param("productId"), time.Now().UTC())
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"invitations": items})
}

func (s *Server) resendProductCollaboratorInvitation(c *gin.Context) {
	if !s.requireProductCollaboratorManager(c) {
		return
	}
	if !s.requireEmailOutbox(c) {
		return
	}
	token, expiresAt, err := s.newAuthToken("product_collaborator_invitation")
	if err != nil {
		writeError(c, http.StatusInternalServerError, "token_issue_failed", "Could not issue invitation token")
		return
	}
	outbox := authTokenEmailOutbox("pending@example.invalid", "product_collaborator_invitation", token, expiresAt)
	item, err := s.store.ResendProductCollaboratorInvitation(c.Request.Context(), store.ProductCollaboratorInvitationMutation{BrandCloudID: c.Param("brandCloudId"), ProductID: c.Param("productId"), InvitationID: c.Param("invitationId"), ActorUserID: currentUserID(c), TokenHash: auth.HashToken(token), ExpiresAt: expiresAt, Email: &outbox}, time.Now().UTC())
	if err != nil {
		writeStoreError(c, err)
		return
	}
	s.notifyAuthTokenQueued(AuthTokenDelivery{Purpose: "product_collaborator_invitation", Email: item.TargetEmail, Token: token, ExpiresAt: expiresAt})
	c.JSON(http.StatusAccepted, gin.H{"invitation": item})
}

func (s *Server) cancelProductCollaboratorInvitation(c *gin.Context) {
	if !s.requireProductCollaboratorManager(c) {
		return
	}
	item, err := s.store.CancelProductCollaboratorInvitation(c.Request.Context(), store.ProductCollaboratorInvitationMutation{BrandCloudID: c.Param("brandCloudId"), ProductID: c.Param("productId"), InvitationID: c.Param("invitationId"), ActorUserID: currentUserID(c)}, time.Now().UTC())
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"invitation": item})
}

func (s *Server) acceptProductCollaboratorInvitation(c *gin.Context) {
	var req productCollaboratorInvitationAcceptRequest
	if !bind(c, &req) {
		return
	}
	invitation, err := s.store.AcceptProductCollaboratorInvitation(c.Request.Context(), currentUserID(c), auth.HashToken(req.Token), time.Now().UTC())
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"invitation": invitation})
}

func (s *Server) updateProductCollaborator(c *gin.Context) {
	var req productCollaboratorRoleRequest
	if !bind(c, &req) || !validProductCollaboratorRole(c, strings.TrimSpace(req.Role)) {
		return
	}
	item, err := s.store.UpdateProductCollaborator(c.Request.Context(), currentUserID(c), c.Param("brandCloudId"), c.Param("productId"), c.Param("userId"), strings.TrimSpace(req.Role))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"collaborator": item})
}

func (s *Server) removeProductCollaborator(c *gin.Context) {
	if err := s.store.RemoveProductCollaborator(c.Request.Context(), currentUserID(c), c.Param("brandCloudId"), c.Param("productId"), c.Param("userId")); err != nil {
		writeStoreError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (s *Server) transferProductOwnership(c *gin.Context) {
	var req productOwnerTransferRequest
	if !bind(c, &req) || !requireNonBlank(c, "target_user_id", req.TargetUserID) {
		return
	}
	if err := s.store.TransferProductOwnership(c.Request.Context(), currentUserID(c), c.Param("brandCloudId"), c.Param("productId"), strings.TrimSpace(req.TargetUserID)); err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"product_id": c.Param("productId"), "owner_user_id": strings.TrimSpace(req.TargetUserID)})
}
