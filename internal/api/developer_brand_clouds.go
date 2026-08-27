package api

import (
	"errors"
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

type developerBrandCloudMemberRequest struct {
	Role model.Role `json:"role" binding:"required"`
}

type developerBrandCloudInvitationRequest struct {
	Email string     `json:"email" binding:"required,email"`
	Role  model.Role `json:"role" binding:"required"`
}

type developerBrandCloudInvitationAcceptRequest struct {
	Token string `json:"token" binding:"required"`
}

type developerBrandCloudInvitationResponse struct {
	Invitation model.BrandCloudMemberInvitation `json:"invitation"`
}

func developerBrandCloudManager(c *gin.Context, s *Server) (model.Member, bool) {
	member, err := s.store.GetDeveloperBrandCloudMember(c.Request.Context(), c.Param("brandCloudId"), currentUserID(c))
	if err != nil {
		writeStoreError(c, err)
		return model.Member{}, false
	}
	if member.Role != model.RoleOwner {
		writeError(c, http.StatusForbidden, "developer_brand_cloud_owner_required", "Brand Cloud membership management requires owner role")
		return model.Member{}, false
	}
	return member, true
}

func (s *Server) listDeveloperBrandClouds(c *gin.Context) {
	limit, offset := pagination(c)
	page, err := s.store.ListDeveloperBrandClouds(c.Request.Context(), currentUserID(c), limit, offset)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	for i := range page.Organizations {
		page.Organizations[i].Capabilities = developerCapabilitiesForRole(page.Organizations[i].Role)
	}
	user, err := s.store.GetUser(c.Request.Context(), currentUserID(c))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"brand_clouds": page.Organizations, "pagination": page.Page, "developer_cloud_limit": user.DeveloperCloudLimit})
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

func (s *Server) getDeveloperBrandCloud(c *gin.Context) {
	org, err := s.store.GetOrganization(c.Request.Context(), c.Param("brandCloudId"), currentUserID(c))
	if err != nil || org.OrganizationKind != model.OrganizationKindBrandCloud {
		if err == nil {
			err = store.ErrNotFound
		}
		writeStoreError(c, err)
		return
	}
	member, err := s.store.GetDeveloperBrandCloudMember(c.Request.Context(), org.ID, currentUserID(c))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	org.Capabilities = developerCapabilitiesForRole(member.Role)
	c.JSON(http.StatusOK, gin.H{"brand_cloud": org, "membership": member})
}

func developerCapabilitiesForRole(role model.Role) []string {
	read := []string{"fleet.read", "sku.read", "firmware.release.read", "ota.plan.read", "reports.read", "team.read", "provisioning.read"}
	if role == model.RoleMember {
		return read
	}
	capabilities := append(read, "fleet.device.manage", "fleet.batch.manage", "sku.manage", "sku.policy.manage", "firmware.release.manage", "ota.plan.manage", "reports.create", "provisioning.create", "pki.test.issue")
	if role == model.RoleOwner {
		capabilities = append(capabilities, "team.manage")
	}
	return capabilities
}

func (s *Server) listDeveloperBrandCloudMembers(c *gin.Context) {
	if _, err := s.store.GetDeveloperBrandCloudMember(c.Request.Context(), c.Param("brandCloudId"), currentUserID(c)); err != nil {
		writeStoreError(c, err)
		return
	}
	limit, offset := pagination(c)
	page, err := s.store.ListDeveloperBrandCloudMembers(c.Request.Context(), c.Param("brandCloudId"), limit, offset)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	for i := range page.Members {
		page.Members[i].Capabilities = developerCapabilitiesForRole(page.Members[i].Role)
	}
	c.JSON(http.StatusOK, gin.H{"members": page.Members, "pagination": page.Page})
}

func (s *Server) inviteDeveloperBrandCloudMember(c *gin.Context) {
	if _, ok := developerBrandCloudManager(c, s); !ok {
		return
	}
	var req developerBrandCloudInvitationRequest
	if !bind(c, &req) || !validDeveloperInvitationRole(c, req.Role) {
		return
	}
	token, expiresAt, err := s.newAuthToken("brand_cloud_membership_invitation")
	if err != nil {
		writeError(c, http.StatusInternalServerError, "token_issue_failed", "Could not issue invitation token")
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	var outbox *store.EmailOutboxInput
	if s.emailOutboxStore != nil {
		value := authTokenEmailOutbox(email, "brand_cloud_membership_invitation", token, expiresAt)
		outbox = &value
	}
	invitation, created, err := s.store.CreateBrandCloudMemberInvitation(c.Request.Context(), store.BrandCloudMemberInvitationInput{
		BrandCloudID: c.Param("brandCloudId"), InvitedByUserID: currentUserID(c), TargetEmail: email,
		Role: req.Role, TokenHash: auth.HashToken(token), ExpiresAt: expiresAt, Email: outbox,
	}, time.Now().UTC())
	if err != nil {
		writeStoreError(c, err)
		return
	}
	if created && s.emailOutboxStore == nil {
		if err := s.deliverAuthToken(c, invitation.TargetEmail, "brand_cloud_membership_invitation", token, expiresAt); err != nil {
			writeError(c, http.StatusInternalServerError, "token_delivery_failed", "Could not deliver invitation token")
			return
		}
	}
	c.JSON(http.StatusAccepted, developerBrandCloudInvitationResponse{Invitation: invitation})
}

func validDeveloperInvitationRole(c *gin.Context, role model.Role) bool {
	if role != model.RoleAdmin && role != model.RoleMember {
		writeError(c, http.StatusBadRequest, "invalid_role", "Invitation role must be admin or member")
		return false
	}
	return true
}

func (s *Server) listDeveloperBrandCloudMemberInvitations(c *gin.Context) {
	if _, ok := developerBrandCloudManager(c, s); !ok {
		return
	}
	invitations, err := s.store.ListBrandCloudMemberInvitations(c.Request.Context(), c.Param("brandCloudId"), currentUserID(c), time.Now().UTC())
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"invitations": invitations})
}

func (s *Server) resendDeveloperBrandCloudMemberInvitation(c *gin.Context) {
	if _, ok := developerBrandCloudManager(c, s); !ok {
		return
	}
	token, expiresAt, err := s.newAuthToken("brand_cloud_membership_invitation")
	if err != nil {
		writeError(c, http.StatusInternalServerError, "token_issue_failed", "Could not issue invitation token")
		return
	}
	var outbox *store.EmailOutboxInput
	if s.emailOutboxStore != nil {
		value := authTokenEmailOutbox("pending@example.invalid", "brand_cloud_membership_invitation", token, expiresAt)
		outbox = &value
	}
	invitation, err := s.store.ResendBrandCloudMemberInvitation(c.Request.Context(), store.BrandCloudMemberInvitationMutation{
		BrandCloudID: c.Param("brandCloudId"), InvitationID: c.Param("invitationId"), ActorUserID: currentUserID(c),
		TokenHash: auth.HashToken(token), ExpiresAt: expiresAt, Email: outbox,
	}, time.Now().UTC())
	if err != nil {
		writeStoreError(c, err)
		return
	}
	if s.emailOutboxStore == nil {
		if err := s.deliverAuthToken(c, invitation.TargetEmail, "brand_cloud_membership_invitation", token, expiresAt); err != nil {
			writeError(c, http.StatusInternalServerError, "token_delivery_failed", "Could not deliver invitation token")
			return
		}
	}
	c.JSON(http.StatusAccepted, developerBrandCloudInvitationResponse{Invitation: invitation})
}

func (s *Server) cancelDeveloperBrandCloudMemberInvitation(c *gin.Context) {
	if _, ok := developerBrandCloudManager(c, s); !ok {
		return
	}
	invitation, err := s.store.CancelBrandCloudMemberInvitation(c.Request.Context(), store.BrandCloudMemberInvitationMutation{
		BrandCloudID: c.Param("brandCloudId"), InvitationID: c.Param("invitationId"), ActorUserID: currentUserID(c),
	}, time.Now().UTC())
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, developerBrandCloudInvitationResponse{Invitation: invitation})
}

func (s *Server) acceptDeveloperBrandCloudMemberInvitation(c *gin.Context) {
	var req developerBrandCloudInvitationAcceptRequest
	if !bind(c, &req) {
		return
	}
	invitation, member, err := s.store.AcceptBrandCloudMemberInvitation(c.Request.Context(), currentUserID(c), auth.HashToken(req.Token), time.Now().UTC())
	if err != nil {
		writeStoreError(c, err)
		return
	}
	member.Capabilities = developerCapabilitiesForRole(member.Role)
	c.JSON(http.StatusOK, gin.H{"invitation": invitation, "member": member})
}

func (s *Server) updateDeveloperBrandCloudMember(c *gin.Context) {
	if _, ok := developerBrandCloudManager(c, s); !ok {
		return
	}
	var req developerBrandCloudMemberRequest
	if !bind(c, &req) || !validDeveloperInvitationRole(c, req.Role) {
		return
	}
	member, err := s.store.UpdateMemberRole(c.Request.Context(), c.Param("brandCloudId"), c.Param("userId"), req.Role)
	if err != nil {
		if errors.Is(err, store.ErrLastOwner) {
			writeError(c, http.StatusConflict, "last_owner", err.Error())
			return
		}
		writeStoreError(c, err)
		return
	}
	member.Capabilities = developerCapabilitiesForRole(member.Role)
	c.JSON(http.StatusOK, gin.H{"member": member})
}

func (s *Server) removeDeveloperBrandCloudMember(c *gin.Context) {
	if _, ok := developerBrandCloudManager(c, s); !ok {
		return
	}
	if err := s.store.RemoveMember(c.Request.Context(), c.Param("brandCloudId"), c.Param("userId")); err != nil {
		if errors.Is(err, store.ErrLastOwner) {
			writeError(c, http.StatusConflict, "last_owner", err.Error())
			return
		}
		writeStoreError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (s *Server) disableDeveloperBrandCloudMember(c *gin.Context) {
	if _, ok := developerBrandCloudManager(c, s); !ok {
		return
	}
	member, err := s.store.DisableDeveloperBrandCloudMember(c.Request.Context(), c.Param("brandCloudId"), c.Param("userId"))
	if err != nil {
		if errors.Is(err, store.ErrLastOwner) {
			writeError(c, http.StatusConflict, "last_owner", err.Error())
			return
		}
		writeStoreError(c, err)
		return
	}
	member.Capabilities = developerCapabilitiesForRole(member.Role)
	c.JSON(http.StatusOK, gin.H{"member": member})
}

func (s *Server) enableDeveloperBrandCloudMember(c *gin.Context) {
	if _, ok := developerBrandCloudManager(c, s); !ok {
		return
	}
	member, err := s.store.EnableDeveloperBrandCloudMember(c.Request.Context(), c.Param("brandCloudId"), c.Param("userId"))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	member.Capabilities = developerCapabilitiesForRole(member.Role)
	c.JSON(http.StatusOK, gin.H{"member": member})
}

func (s *Server) createBrandCloudOwnerTransfer(c *gin.Context) {
	if _, ok := developerBrandCloudManager(c, s); !ok {
		return
	}
	var req brandCloudOwnerTransferRequest
	if !bind(c, &req) {
		return
	}
	token, expiresAt, err := s.newAuthToken("brand_cloud_owner_transfer")
	if err != nil {
		writeError(c, http.StatusInternalServerError, "token_issue_failed", "Could not issue owner transfer token")
		return
	}
	targetEmail := strings.ToLower(strings.TrimSpace(req.TargetEmail))
	var emailOutbox *store.EmailOutboxInput
	if s.emailOutboxStore != nil {
		value := authTokenEmailOutbox(targetEmail, "brand_cloud_owner_transfer", token, expiresAt)
		emailOutbox = &value
	}
	transfer, err := s.store.CreateBrandCloudOwnerTransfer(c.Request.Context(), store.BrandCloudOwnerTransferInput{
		BrandCloudID:      c.Param("brandCloudId"),
		RequestedByUserID: currentUserID(c),
		TargetEmail:       targetEmail,
		TokenHash:         auth.HashToken(token),
		ExpiresAt:         expiresAt,
		Email:             emailOutbox,
	})
	if err != nil {
		writeStoreError(c, err)
		return
	}
	if s.emailOutboxStore == nil {
		if err := s.deliverAuthToken(c, targetEmail, "brand_cloud_owner_transfer", token, expiresAt); err != nil {
			writeError(c, http.StatusInternalServerError, "token_delivery_failed", "Could not deliver owner transfer token")
			return
		}
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

func (s *Server) getBrandCloudOwnerTransfer(c *gin.Context) {
	if _, ok := developerBrandCloudManager(c, s); !ok {
		return
	}
	transfer, err := s.store.GetBrandCloudOwnerTransfer(c.Request.Context(), store.BrandCloudOwnerTransferQuery{BrandCloudID: c.Param("brandCloudId"), TransferID: c.Param("transferId"), RequesterID: currentUserID(c)}, time.Now().UTC())
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, brandCloudOwnerTransferResponse{OwnerTransfer: transfer})
}

func (s *Server) cancelBrandCloudOwnerTransfer(c *gin.Context) {
	if _, ok := developerBrandCloudManager(c, s); !ok {
		return
	}
	transfer, err := s.store.CancelBrandCloudOwnerTransfer(c.Request.Context(), store.BrandCloudOwnerTransferQuery{BrandCloudID: c.Param("brandCloudId"), TransferID: c.Param("transferId"), RequesterID: currentUserID(c)}, time.Now().UTC())
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, brandCloudOwnerTransferResponse{OwnerTransfer: transfer})
}
