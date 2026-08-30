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
	Name       string         `json:"name,omitempty"`
	TenantSlug string         `json:"tenant_slug,omitempty"`
	Status     string         `json:"status,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

type brandCloudMemberRequest struct {
	BrandCloudUserID string `json:"brand_cloud_user_id,omitempty"`
	Role             string `json:"role" binding:"required"`
}

type brandCloudUserRequest struct {
	Email          string  `json:"email" binding:"required,email"`
	Password       string  `json:"password"`
	DisplayName    *string `json:"display_name"`
	Role           string  `json:"role" binding:"required"`
	RotatePassword bool    `json:"rotate_password"`
	ActivationMode string  `json:"activation_mode"`
}

type deviceItemProfileRequest struct {
	ProfileKey         string         `json:"profile_key,omitempty"`
	DisplayName        string         `json:"display_name,omitempty"`
	Status             string         `json:"status,omitempty"`
	Category           string         `json:"category,omitempty"`
	Manufacturer       *string        `json:"manufacturer"`
	Model              *string        `json:"model"`
	MetadataDefaults   map[string]any `json:"metadata_defaults"`
	MetadataSchema     map[string]any `json:"metadata_schema"`
	CAProfile          string         `json:"ca_profile,omitempty"`
	IssuerProfile      string         `json:"issuer_profile,omitempty"`
	ServiceOptions     []string       `json:"service_options"`
	ClaimPolicy        map[string]any `json:"claim_policy"`
	ProvisioningPolicy map[string]any `json:"provisioning_policy"`
}

type deviceItemProfileResponse struct {
	DeviceItemProfile model.DeviceItemProfile `json:"device_item_profile"`
}

type deviceItemProfilesResponse struct {
	DeviceItemProfiles []model.DeviceItemProfile `json:"device_item_profiles"`
	Pagination         store.Page                `json:"pagination"`
}

type brandCloudUsersResponse struct {
	Users      []model.Member `json:"users"`
	Pagination store.Page     `json:"pagination"`
}

type brandCloudUserResponse struct {
	Member         model.Member         `json:"member,omitempty"`
	BrandCloudUser model.BrandCloudUser `json:"brand_cloud_user,omitempty"`
}

func profileBrandCloudID(c *gin.Context) string {
	if id := currentBrandCloudID(c); id != "" {
		return id
	}
	if id := c.Param("brandCloudId"); id != "" {
		return id
	}
	return c.Param("orgId")
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
		Name:       strings.TrimSpace(req.Name),
		TenantSlug: strings.TrimSpace(req.TenantSlug),
		Status:     status,
		Metadata:   req.Metadata,
	})
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"brand_cloud": org})
}

func (s *Server) createDeviceItemProfile(c *gin.Context) {
	var req deviceItemProfileRequest
	if !bindStrict(c, &req) {
		return
	}
	if !requireNonBlank(c, "profile_key", req.ProfileKey) ||
		!requireNonBlank(c, "display_name", req.DisplayName) ||
		!requireNonBlank(c, "ca_profile", req.CAProfile) ||
		!requireNonBlank(c, "issuer_profile", req.IssuerProfile) {
		return
	}
	category, ok := parseDeviceClaimTokenCategory(c, req.Category)
	if !ok {
		return
	}
	serviceOptions, ok := canonicalServiceOptions(c, req.ServiceOptions)
	if !ok {
		return
	}
	profile, err := s.store.CreateDeviceItemProfile(c.Request.Context(), store.DeviceItemProfileCreateInput{
		ActorUserID:        stringPtr(currentUserID(c)),
		BrandCloudID:       profileBrandCloudID(c),
		ProfileKey:         req.ProfileKey,
		DisplayName:        req.DisplayName,
		Category:           category,
		Manufacturer:       trimPtr(req.Manufacturer),
		Model:              trimPtr(req.Model),
		MetadataDefaults:   req.MetadataDefaults,
		MetadataSchema:     req.MetadataSchema,
		CAProfile:          req.CAProfile,
		IssuerProfile:      req.IssuerProfile,
		ServiceOptions:     serviceOptions,
		ClaimPolicy:        req.ClaimPolicy,
		ProvisioningPolicy: req.ProvisioningPolicy,
	})
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusCreated, deviceItemProfileResponse{DeviceItemProfile: profile})
}

func (s *Server) listDeviceItemProfiles(c *gin.Context) {
	limit, offset := pagination(c)
	isPlatformAdmin := s.currentUserIsPlatformAdmin(c)
	status := model.DeviceItemProfileStatus(strings.TrimSpace(c.Query("status")))
	if status != "" && status != model.DeviceItemProfileStatusActive && status != model.DeviceItemProfileStatusDisabled {
		writeError(c, http.StatusBadRequest, "invalid_status", "status must be active or disabled")
		return
	}
	page, err := s.store.ListDeviceItemProfiles(c.Request.Context(), store.DeviceItemProfileListFilter{
		BrandCloudID: profileBrandCloudID(c),
		BrandCloudUserID: func() string {
			if currentSubjectType(c) == auth.SubjectTypeBrandCloudUser {
				return currentBrandCloudUserID(c)
			}
			return ""
		}(),
		UserID: func() string {
			if currentSubjectType(c) != auth.SubjectTypeBrandCloudUser && !isPlatformAdmin {
				return currentUserID(c)
			}
			return ""
		}(),
		Status: status,
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		writeStoreError(c, err)
		return
	}
	if currentSubjectType(c) == auth.SubjectTypeBrandCloudUser {
		for i := range page.Profiles {
			page.Profiles[i].CurrentUserRole, _ = s.store.GetProductCollaboratorRole(c.Request.Context(), currentBrandCloudUserID(c), profileBrandCloudID(c), page.Profiles[i].ID)
		}
	}
	if currentSubjectType(c) != auth.SubjectTypeBrandCloudUser {
		for i := range page.Profiles {
			page.Profiles[i].CurrentUserRole, _ = s.store.GetUserProductCollaboratorRole(c.Request.Context(), currentUserID(c), profileBrandCloudID(c), page.Profiles[i].ID)
		}
	}
	c.JSON(http.StatusOK, deviceItemProfilesResponse{DeviceItemProfiles: page.Profiles, Pagination: page.Page})
}

func (s *Server) getDeviceItemProfile(c *gin.Context) {
	profile, err := s.store.GetDeviceItemProfile(c.Request.Context(), profileBrandCloudID(c), c.Param("profileId"))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	if currentSubjectType(c) == auth.SubjectTypeBrandCloudUser {
		profile.CurrentUserRole, _ = s.store.GetProductCollaboratorRole(c.Request.Context(), currentBrandCloudUserID(c), profileBrandCloudID(c), profile.ID)
	}
	if currentSubjectType(c) != auth.SubjectTypeBrandCloudUser {
		profile.CurrentUserRole, _ = s.store.GetUserProductCollaboratorRole(c.Request.Context(), currentUserID(c), profileBrandCloudID(c), profile.ID)
	}
	c.JSON(http.StatusOK, deviceItemProfileResponse{DeviceItemProfile: profile})
}

func (s *Server) updateDeviceItemProfile(c *gin.Context) {
	var req deviceItemProfileRequest
	if !bindStrict(c, &req) {
		return
	}
	var status *model.DeviceItemProfileStatus
	if strings.TrimSpace(req.Status) != "" {
		parsed := model.DeviceItemProfileStatus(strings.TrimSpace(req.Status))
		if parsed != model.DeviceItemProfileStatusActive && parsed != model.DeviceItemProfileStatusDisabled {
			writeError(c, http.StatusBadRequest, "invalid_status", "status must be active or disabled")
			return
		}
		status = &parsed
	}
	var category *model.DeviceCategory
	if strings.TrimSpace(req.Category) != "" {
		parsed, ok := parseDeviceClaimTokenCategory(c, req.Category)
		if !ok {
			return
		}
		category = &parsed
	}
	var serviceOptions []string
	if req.ServiceOptions != nil {
		parsed, ok := canonicalServiceOptions(c, req.ServiceOptions)
		if !ok {
			return
		}
		serviceOptions = parsed
	}
	var displayName *string
	if strings.TrimSpace(req.DisplayName) != "" {
		displayName = &req.DisplayName
	}
	var caProfile *string
	if strings.TrimSpace(req.CAProfile) != "" {
		caProfile = &req.CAProfile
	}
	var issuerProfile *string
	if strings.TrimSpace(req.IssuerProfile) != "" {
		issuerProfile = &req.IssuerProfile
	}
	profile, err := s.store.UpdateDeviceItemProfile(c.Request.Context(), store.DeviceItemProfileUpdateInput{
		ActorUserID:        stringPtr(currentUserID(c)),
		BrandCloudID:       profileBrandCloudID(c),
		ProfileID:          c.Param("profileId"),
		DisplayName:        displayName,
		Status:             status,
		Category:           category,
		Manufacturer:       trimPtr(req.Manufacturer),
		Model:              trimPtr(req.Model),
		MetadataDefaults:   req.MetadataDefaults,
		MetadataSchema:     req.MetadataSchema,
		CAProfile:          caProfile,
		IssuerProfile:      issuerProfile,
		ServiceOptions:     serviceOptions,
		ClaimPolicy:        req.ClaimPolicy,
		ProvisioningPolicy: req.ProvisioningPolicy,
	})
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, deviceItemProfileResponse{DeviceItemProfile: profile})
}

func (s *Server) disableDeviceItemProfile(c *gin.Context) {
	profile, err := s.store.DisableDeviceItemProfile(c.Request.Context(), profileBrandCloudID(c), c.Param("profileId"), stringPtr(currentUserID(c)))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, deviceItemProfileResponse{DeviceItemProfile: profile})
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
	brandCloudUserID := strings.TrimSpace(req.BrandCloudUserID)
	if brandCloudUserID == "" {
		writeError(c, http.StatusBadRequest, "missing_brand_cloud_user", "brand_cloud_user_id is required")
		return
	}
	member, err := s.store.AssignBrandCloudMember(c.Request.Context(), currentUserID(c), c.Param("brandCloudId"), brandCloudUserID, role)
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
	activationMode := strings.ToLower(strings.TrimSpace(req.ActivationMode))
	if activationMode == "" {
		activationMode = "email"
	}
	if activationMode != "immediate" && activationMode != "email" {
		writeError(c, http.StatusBadRequest, "invalid_activation_mode", "activation_mode must be immediate or email")
		return
	}
	if activationMode == "immediate" && len(req.Password) < 8 {
		writeError(c, http.StatusBadRequest, "invalid_password", "password must be at least 8 characters")
		return
	}
	if activationMode == "immediate" && (!s.allowImmediateBrandAccounts || role == model.RoleOwner) {
		writeError(c, http.StatusForbidden, "immediate_provisioning_forbidden", "immediate provisioning is restricted to audited staging member/admin creation")
		return
	}
	if activationMode == "email" && strings.TrimSpace(req.Password) != "" {
		writeError(c, http.StatusBadRequest, "password_not_allowed", "password must not be supplied for email activation")
		return
	}
	password := req.Password
	if activationMode == "email" {
		var tokenErr error
		password, tokenErr = auth.RandomToken()
		if tokenErr != nil {
			writeError(c, http.StatusInternalServerError, "password_hash_failed", "Could not create pending account credential")
			return
		}
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "password_hash_failed", "Could not hash password")
		return
	}
	input := store.BrandCloudAccountInput{
		Email:          strings.ToLower(strings.TrimSpace(req.Email)),
		PasswordHash:   hash,
		DisplayName:    trimStringPtr(req.DisplayName),
		Role:           role,
		RotatePassword: req.RotatePassword,
		ActivationMode: activationMode,
	}
	if activationMode == "email" {
		token, expiresAt, tokenErr := s.newAuthToken("email_verification")
		if tokenErr != nil {
			writeError(c, http.StatusInternalServerError, "token_issue_failed", "Could not issue activation token")
			return
		}
		input.ActivationTokenHash = auth.HashToken(token)
		input.ActivationExpiresAt = expiresAt
		outbox := authTokenEmailOutbox(input.Email, "email_verification", token, expiresAt)
		input.ActivationEmail = &outbox
	}
	result, err := s.store.ProvisionBrandCloudAccount(c.Request.Context(), currentUserID(c), c.Param("brandCloudId"), input)
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

func (s *Server) listBrandCloudUsers(c *gin.Context) {
	limit, offset := pagination(c)
	status := strings.TrimSpace(c.Query("status"))
	if status != "" && status != "active" && status != "pending_verification" && status != "disabled" {
		writeError(c, http.StatusBadRequest, "invalid_status", "status must be active, pending_verification, or disabled")
		return
	}
	page, err := s.store.ListBrandCloudAccounts(c.Request.Context(), store.BrandCloudAccountListFilter{
		BrandCloudID: c.Param("brandCloudId"),
		Status:       status,
		Query:        c.Query("q"),
		Limit:        limit,
		Offset:       offset,
	})
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, brandCloudUsersResponse{Users: page.Members, Pagination: page.Page})
}

func (s *Server) disableBrandCloudUser(c *gin.Context) {
	member, err := s.store.DisableDeveloperBrandCloudMember(c.Request.Context(), c.Param("brandCloudId"), c.Param("userId"))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, brandCloudUserResponse{Member: member})
}

func (s *Server) enableBrandCloudUser(c *gin.Context) {
	member, err := s.store.EnableDeveloperBrandCloudMember(c.Request.Context(), c.Param("brandCloudId"), c.Param("userId"))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, brandCloudUserResponse{Member: member})
}

func (s *Server) approveBrandCloudUser(c *gin.Context) {
	user, err := s.store.ApproveBrandCloudUser(c.Request.Context(), currentUserID(c), c.Param("brandCloudId"), c.Param("brandCloudUserId"))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, brandCloudUserResponse{BrandCloudUser: user})
}

func (s *Server) revokeBrandCloudUserAppCertificate(c *gin.Context) {
	revoked, err := s.store.RevokeValidAppCertificatesForBrandCloudUser(c.Request.Context(), c.Param("brandCloudId"), c.Param("brandCloudUserId"))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"revoked": revoked})
}

func (s *Server) deleteBrandCloudUser(c *gin.Context) {
	if err := s.store.RemoveMember(c.Request.Context(), c.Param("brandCloudId"), c.Param("userId")); err != nil {
		writeStoreError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
