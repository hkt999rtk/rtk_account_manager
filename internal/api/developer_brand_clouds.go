package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"

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
	Role        json.RawMessage `json:"role"`
	AccessScope json.RawMessage `json:"access_scope,omitempty"`
}

type developerBrandCloudInvitationRequest struct {
	AccessScope json.RawMessage `json:"access_scope,omitempty"`
	Email       string          `json:"email" binding:"required,email"`
	Role        model.Role      `json:"role" binding:"required"`
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
	limit, limitErr := strconv.Atoi(c.DefaultQuery("limit", "25"))
	offset, offsetErr := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if limitErr != nil || offsetErr != nil || limit < 1 || limit > 100 || offset < 0 {
		writeError(c, http.StatusBadRequest, "invalid_pagination", "limit must be 1..100 and offset nonnegative")
		return
	}
	view := strings.TrimSpace(c.DefaultQuery("view", "all"))
	if view != "all" && view != "owned" && view != "shared" {
		writeError(c, http.StatusBadRequest, "invalid_cloud_view", "view must be all, owned or shared")
		return
	}
	page, err := s.store.ListManagedBrandClouds(c.Request.Context(), currentUserID(c), view, limit, offset)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	for i := range page.BrandClouds {
		if !page.BrandClouds[i].Operational {
			continue
		}
		page.BrandClouds[i].Capabilities, err = s.developerCapabilitiesForUser(c.Request.Context(), currentUserID(c), page.BrandClouds[i].ID, page.BrandClouds[i].Role)
		if err != nil {
			writeStoreError(c, err)
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"brand_clouds": page.BrandClouds, "total": page.Total, "limit": page.Limit, "offset": page.Offset,
		"owned_count": page.OwnedCount, "owned_limit": page.OwnedLimit,
		"reserved_count": page.ReservedCount,
		"pagination":     page.Page, "developer_cloud_limit": page.OwnedLimit})
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
	org.Capabilities, err = s.developerCapabilitiesForUser(c.Request.Context(), currentUserID(c), org.ID, member.Role)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"brand_cloud": org, "membership": member})
}

func developerCapabilitiesForRole(role model.Role) []string {
	if role == model.RoleViewer {
		return []string{"fleet.read", "product.read", "firmware.release.read", "ota.plan.read", "reports.read"}
	}
	read := []string{"fleet.read", "product.read", "firmware.release.read", "ota.plan.read", "reports.read", "team.read", "provisioning.read"}
	if role == model.RoleMember {
		return read
	}
	capabilities := append(read, "fleet.device.manage", "fleet.batch.manage", "product.manage", "product.policy.manage", "firmware.release.manage", "ota.plan.manage", "reports.create", "provisioning.create", "pki.test.issue")
	if role == model.RoleOwner {
		capabilities = append(capabilities, "team.manage")
	}
	return capabilities
}

func (s *Server) developerCapabilitiesForUser(ctx context.Context, userID, brandCloudID string, role model.Role) ([]string, error) {
	capabilities := developerCapabilitiesForRole(role)
	if role == model.RoleViewer {
		return capabilities, nil
	}
	permissions, err := s.store.ListUserOrganizationPermissions(ctx, userID, brandCloudID)
	if err != nil {
		return nil, err
	}
	capabilities = appendUniqueCapabilities(capabilities, permissions...)
	if role != model.RoleMember {
		return capabilities, nil
	}
	if hasCapability(permissions, "registry_device.manage") {
		capabilities = appendUniqueCapabilities(capabilities, "fleet.device.manage", "fleet.batch.manage", "product.manage", "product.policy.manage", "firmware.release.manage", "ota.plan.manage", "reports.create", "provisioning.create")
	}
	return capabilities, nil
}

func appendUniqueCapabilities(capabilities []string, additions ...string) []string {
	seen := make(map[string]struct{}, len(capabilities)+len(additions))
	for _, capability := range capabilities {
		seen[capability] = struct{}{}
	}
	for _, capability := range additions {
		if _, ok := seen[capability]; ok {
			continue
		}
		seen[capability] = struct{}{}
		capabilities = append(capabilities, capability)
	}
	return capabilities
}

func hasCapability(capabilities []string, target string) bool {
	for _, capability := range capabilities {
		if capability == target {
			return true
		}
	}
	return false
}

func (s *Server) listDeveloperBrandCloudMembers(c *gin.Context) {
	if _, ok := developerBrandCloudManager(c, s); !ok {
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
	if !s.requireEmailOutbox(c) {
		return
	}
	var req developerBrandCloudInvitationRequest
	if !bindStrict(c, &req) || !validDeveloperInvitationRole(c, req.Role) {
		return
	}
	if err := binding.Validator.ValidateStruct(&req); err != nil {
		writeError(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	scope, err := parseViewerScope(req.AccessScope)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	token, expiresAt, err := s.newAuthToken("brand_cloud_membership_invitation")
	if err != nil {
		writeError(c, http.StatusInternalServerError, "token_issue_failed", "Could not issue invitation token")
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	outbox := authTokenEmailOutbox(email, "brand_cloud_membership_invitation", token, expiresAt)
	invitation, created, err := s.store.CreateBrandCloudMemberInvitation(c.Request.Context(), store.BrandCloudMemberInvitationInput{
		BrandCloudID: c.Param("brandCloudId"), InvitedByUserID: currentUserID(c), TargetEmail: email,
		Role: req.Role, AccessScope: scope, TokenHash: auth.HashToken(token), ExpiresAt: expiresAt, Email: &outbox,
	}, time.Now().UTC())
	if err != nil {
		writeStoreError(c, err)
		return
	}
	if created {
		s.notifyAuthTokenQueued(AuthTokenDelivery{Purpose: "brand_cloud_membership_invitation", Email: invitation.TargetEmail, Token: token, ExpiresAt: expiresAt})
	}
	c.JSON(http.StatusAccepted, developerBrandCloudInvitationResponse{Invitation: invitation})
}

func validDeveloperInvitationRole(c *gin.Context, role model.Role) bool {
	if role != model.RoleAdmin && role != model.RoleMember && role != model.RoleViewer {
		writeError(c, http.StatusBadRequest, "invalid_role", "Invitation role must be admin, member or viewer")
		return false
	}
	return true
}

func parseViewerScope(raw json.RawMessage) (*model.CloudViewerScope, error) {
	if raw == nil {
		return nil, nil
	}
	var scope model.CloudViewerScope
	if err := json.Unmarshal(raw, &scope); err != nil {
		return nil, model.ErrInvalidCloudViewerScope
	}
	return &scope, nil
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
	if !s.requireEmailOutbox(c) {
		return
	}
	token, expiresAt, err := s.newAuthToken("brand_cloud_membership_invitation")
	if err != nil {
		writeError(c, http.StatusInternalServerError, "token_issue_failed", "Could not issue invitation token")
		return
	}
	outbox := authTokenEmailOutbox("pending@example.invalid", "brand_cloud_membership_invitation", token, expiresAt)
	invitation, err := s.store.ResendBrandCloudMemberInvitation(c.Request.Context(), store.BrandCloudMemberInvitationMutation{
		BrandCloudID: c.Param("brandCloudId"), InvitationID: c.Param("invitationId"), ActorUserID: currentUserID(c),
		TokenHash: auth.HashToken(token), ExpiresAt: expiresAt, Email: &outbox,
	}, time.Now().UTC())
	if err != nil {
		writeStoreError(c, err)
		return
	}
	s.notifyAuthTokenQueued(AuthTokenDelivery{Purpose: "brand_cloud_membership_invitation", Email: invitation.TargetEmail, Token: token, ExpiresAt: expiresAt})
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
	if !bindStrict(c, &req) {
		return
	}
	role := model.Role("")
	if req.Role != nil {
		if err := json.Unmarshal(req.Role, &role); err != nil {
			writeError(c, http.StatusBadRequest, "invalid_role", "Invalid role")
			return
		}
		if !validDeveloperInvitationRole(c, role) {
			return
		}
	}
	scope, err := parseViewerScope(req.AccessScope)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	member, err := s.store.UpdateDeveloperBrandCloudMember(c.Request.Context(), store.CloudMemberUpdateInput{BrandCloudID: c.Param("brandCloudId"), ActorUserID: currentUserID(c), TargetUserID: c.Param("userId"), Role: role, AccessScope: scope})
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
	if !s.requireEmailOutbox(c) {
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
	emailOutbox := authTokenEmailOutbox(targetEmail, "brand_cloud_owner_transfer", token, expiresAt)
	transfer, err := s.store.CreateBrandCloudOwnerTransfer(c.Request.Context(), store.BrandCloudOwnerTransferInput{
		BrandCloudID:      c.Param("brandCloudId"),
		RequestedByUserID: currentUserID(c),
		TargetEmail:       targetEmail,
		TokenHash:         auth.HashToken(token),
		ExpiresAt:         expiresAt,
		Email:             &emailOutbox,
	})
	if err != nil {
		writeStoreError(c, err)
		return
	}
	s.notifyAuthTokenQueued(AuthTokenDelivery{Purpose: "brand_cloud_owner_transfer", Email: targetEmail, Token: token, ExpiresAt: expiresAt})
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
	c.JSON(http.StatusAccepted, brandCloudOwnerTransferResponse{OwnerTransfer: transfer})
}

func (s *Server) getBrandCloudOwnerTransfer(c *gin.Context) {
	// The target is a participant, not a member. The store checks this global
	// session against the exact operation's source/target even while cloud-fenced.
	transfer, err := s.store.GetBrandCloudOwnerTransfer(c.Request.Context(), store.BrandCloudOwnerTransferQuery{BrandCloudID: c.Param("brandCloudId"), TransferID: c.Param("transferId"), RequesterID: currentUserID(c)}, time.Now().UTC())
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, brandCloudOwnerTransferResponse{OwnerTransfer: transfer})
}

func (s *Server) cancelBrandCloudOwnerTransfer(c *gin.Context) {
	transfer, err := s.store.CancelBrandCloudOwnerTransfer(c.Request.Context(), store.BrandCloudOwnerTransferQuery{BrandCloudID: c.Param("brandCloudId"), TransferID: c.Param("transferId"), RequesterID: currentUserID(c)}, time.Now().UTC())
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, brandCloudOwnerTransferResponse{OwnerTransfer: transfer})
}
