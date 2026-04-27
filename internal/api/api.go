package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

type Server struct {
	store *store.Store
	auth  *auth.Service
}

func New(store *store.Store, authService *auth.Service) *Server {
	return &Server{store: store, auth: authService}
}

func (s *Server) Router() *gin.Engine {
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())

	v1 := r.Group("/v1")
	v1.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	v1.POST("/auth/register", s.register)
	v1.POST("/auth/login", s.login)
	v1.POST("/auth/refresh", s.refresh)

	protected := v1.Group("")
	protected.Use(s.requireAuth())
	protected.POST("/auth/logout", s.logout)
	protected.GET("/me", s.me)

	protected.GET("/orgs", s.listOrganizations)
	protected.POST("/orgs", s.createOrganization)
	protected.GET("/orgs/:orgId", s.getOrganization)
	protected.GET("/orgs/:orgId/members", s.requireOrgRole(model.RoleOwner, model.RoleAdmin, model.RoleMember), s.listMembers)
	protected.POST("/orgs/:orgId/members", s.requireOrgRole(model.RoleOwner), s.addMember)
	protected.PATCH("/orgs/:orgId/members/:userId", s.requireOrgRole(model.RoleOwner), s.updateMemberRole)
	protected.PATCH("/orgs/:orgId/members/:userId/disable", s.requireOrgRole(model.RoleOwner), s.disableMemberUser)
	protected.PATCH("/orgs/:orgId/members/:userId/enable", s.requireOrgRole(model.RoleOwner), s.enableMemberUser)
	protected.DELETE("/orgs/:orgId/members/:userId", s.requireOrgRole(model.RoleOwner), s.removeMember)

	protected.POST("/orgs/:orgId/devices", s.requireOrgRole(model.RoleOwner, model.RoleAdmin), s.createDevice)
	protected.GET("/orgs/:orgId/devices", s.requireOrgRole(model.RoleOwner, model.RoleAdmin, model.RoleMember), s.listDevices)
	protected.GET("/orgs/:orgId/devices/:deviceId", s.requireOrgRole(model.RoleOwner, model.RoleAdmin, model.RoleMember), s.getDevice)
	protected.PATCH("/orgs/:orgId/devices/:deviceId", s.requireOrgRole(model.RoleOwner, model.RoleAdmin), s.updateDevice)
	protected.DELETE("/orgs/:orgId/devices/:deviceId", s.requireOrgRole(model.RoleOwner, model.RoleAdmin), s.deleteDevice)
	protected.PATCH("/orgs/:orgId/devices/:deviceId/status", s.requireOrgRole(model.RoleOwner, model.RoleAdmin), s.updateDeviceStatus)

	return r
}

type registerRequest struct {
	Email            string  `json:"email" binding:"required,email"`
	Password         string  `json:"password" binding:"required,min=8"`
	DisplayName      *string `json:"display_name"`
	OrganizationName string  `json:"organization_name" binding:"required"`
}

func (s *Server) register(c *gin.Context) {
	var req registerRequest
	if !bind(c, &req) {
		return
	}
	if !requireNonBlank(c, "organization_name", req.OrganizationName) {
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "password_hash_failed", "Could not hash password")
		return
	}
	result, err := s.store.Register(c.Request.Context(), store.RegisterInput{
		Email:            strings.ToLower(strings.TrimSpace(req.Email)),
		PasswordHash:     hash,
		DisplayName:      req.DisplayName,
		OrganizationName: strings.TrimSpace(req.OrganizationName),
	})
	if err != nil {
		writeStoreError(c, err)
		return
	}
	tokens, err := s.issueTokens(c, result.User.ID)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "token_issue_failed", "Could not issue tokens")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"user": result.User, "organization": result.Organization, "tokens": tokens})
}

type loginRequest struct {
	Email    string `json:"email" binding:"required,email"`
	Password string `json:"password" binding:"required"`
}

func (s *Server) login(c *gin.Context) {
	var req loginRequest
	if !bind(c, &req) {
		return
	}
	user, hash, err := s.store.GetUserPassword(c.Request.Context(), strings.ToLower(strings.TrimSpace(req.Email)))
	if err != nil || !auth.CheckPassword(hash, req.Password) {
		writeError(c, http.StatusUnauthorized, "invalid_credentials", "Invalid email or password")
		return
	}
	tokens, err := s.issueTokens(c, user.ID)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "token_issue_failed", "Could not issue tokens")
		return
	}
	c.JSON(http.StatusOK, gin.H{"user": user, "tokens": tokens})
}

type tokenResponse struct {
	AccessToken           string    `json:"access_token"`
	AccessTokenExpiresAt  time.Time `json:"access_token_expires_at"`
	RefreshToken          string    `json:"refresh_token"`
	RefreshTokenExpiresAt time.Time `json:"refresh_token_expires_at"`
}

func (s *Server) issueTokens(c *gin.Context, userID string) (tokenResponse, error) {
	accessToken, accessExpiresAt, err := s.auth.IssueAccessToken(userID)
	if err != nil {
		return tokenResponse{}, err
	}
	refreshToken, refreshExpiresAt, err := s.auth.IssueRefreshToken(userID)
	if err != nil {
		return tokenResponse{}, err
	}
	if err := s.store.SaveRefreshToken(c.Request.Context(), userID, auth.HashToken(refreshToken), refreshExpiresAt); err != nil {
		return tokenResponse{}, err
	}
	return tokenResponse{
		AccessToken:           accessToken,
		AccessTokenExpiresAt:  accessExpiresAt,
		RefreshToken:          refreshToken,
		RefreshTokenExpiresAt: refreshExpiresAt,
	}, nil
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token" binding:"required"`
}

func (s *Server) refresh(c *gin.Context) {
	var req refreshRequest
	if !bind(c, &req) {
		return
	}
	if !requireNonBlank(c, "refresh_token", req.RefreshToken) {
		return
	}
	claims, err := s.auth.ParseRefreshToken(req.RefreshToken)
	if err != nil {
		writeError(c, http.StatusUnauthorized, "invalid_refresh_token", "Invalid refresh token")
		return
	}
	accessToken, accessExpiresAt, err := s.auth.IssueAccessToken(claims.UserID)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "token_issue_failed", "Could not issue tokens")
		return
	}
	refreshToken, refreshExpiresAt, err := s.auth.IssueRefreshToken(claims.UserID)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "token_issue_failed", "Could not issue tokens")
		return
	}
	tokens := tokenResponse{
		AccessToken:           accessToken,
		AccessTokenExpiresAt:  accessExpiresAt,
		RefreshToken:          refreshToken,
		RefreshTokenExpiresAt: refreshExpiresAt,
	}
	err = s.store.RotateRefreshToken(c.Request.Context(), auth.HashToken(req.RefreshToken), auth.HashToken(refreshToken), claims.UserID, refreshExpiresAt)
	if err != nil {
		writeError(c, http.StatusUnauthorized, "invalid_refresh_token", "Invalid refresh token")
		return
	}
	c.JSON(http.StatusOK, gin.H{"tokens": tokens})
}

func (s *Server) logout(c *gin.Context) {
	var req refreshRequest
	if !bind(c, &req) {
		return
	}
	if !requireNonBlank(c, "refresh_token", req.RefreshToken) {
		return
	}
	if err := s.store.RevokeRefreshToken(c.Request.Context(), auth.HashToken(req.RefreshToken)); err != nil {
		writeStoreError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (s *Server) me(c *gin.Context) {
	userID := currentUserID(c)
	user, err := s.store.GetUser(c.Request.Context(), userID)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	orgPage, err := s.store.ListOrganizations(c.Request.Context(), userID, 200, 0)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"user": user, "organizations": orgPage.Organizations})
}

func (s *Server) listOrganizations(c *gin.Context) {
	limit, offset := pagination(c)
	orgPage, err := s.store.ListOrganizations(c.Request.Context(), currentUserID(c), limit, offset)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"organizations": orgPage.Organizations, "pagination": orgPage.Page})
}

type orgRequest struct {
	Name string `json:"name" binding:"required"`
}

func (s *Server) createOrganization(c *gin.Context) {
	var req orgRequest
	if !bind(c, &req) {
		return
	}
	if !requireNonBlank(c, "name", req.Name) {
		return
	}
	org, err := s.store.CreateOrganization(c.Request.Context(), currentUserID(c), strings.TrimSpace(req.Name))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"organization": org})
}

func (s *Server) getOrganization(c *gin.Context) {
	org, err := s.store.GetOrganization(c.Request.Context(), c.Param("orgId"), currentUserID(c))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"organization": org})
}

func (s *Server) listMembers(c *gin.Context) {
	limit, offset := pagination(c)
	memberPage, err := s.store.ListMembers(c.Request.Context(), c.Param("orgId"), limit, offset)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"members": memberPage.Members, "pagination": memberPage.Page})
}

type addMemberRequest struct {
	Email string     `json:"email" binding:"required,email"`
	Role  model.Role `json:"role" binding:"required"`
}

func (s *Server) addMember(c *gin.Context) {
	var req addMemberRequest
	if !bind(c, &req) || !validRole(c, req.Role) {
		return
	}
	member, err := s.store.AddMember(c.Request.Context(), c.Param("orgId"), strings.ToLower(strings.TrimSpace(req.Email)), req.Role)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"member": member})
}

type updateMemberRequest struct {
	Role model.Role `json:"role" binding:"required"`
}

func (s *Server) updateMemberRole(c *gin.Context) {
	var req updateMemberRequest
	if !bind(c, &req) || !validRole(c, req.Role) {
		return
	}
	member, err := s.store.UpdateMemberRole(c.Request.Context(), c.Param("orgId"), c.Param("userId"), req.Role)
	if err != nil {
		if errors.Is(err, store.ErrLastOwner) {
			writeError(c, http.StatusConflict, "last_owner", err.Error())
			return
		}
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"member": member})
}

func (s *Server) removeMember(c *gin.Context) {
	err := s.store.RemoveMember(c.Request.Context(), c.Param("orgId"), c.Param("userId"))
	if err != nil {
		if errors.Is(err, store.ErrLastOwner) {
			writeError(c, http.StatusConflict, "last_owner", err.Error())
			return
		}
		writeStoreError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (s *Server) disableMemberUser(c *gin.Context) {
	member, err := s.store.DisableMemberUser(c.Request.Context(), c.Param("orgId"), c.Param("userId"))
	if err != nil {
		if errors.Is(err, store.ErrLastOwner) {
			writeError(c, http.StatusConflict, "last_owner", err.Error())
			return
		}
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"member": member})
}

func (s *Server) enableMemberUser(c *gin.Context) {
	member, err := s.store.EnableMemberUser(c.Request.Context(), c.Param("orgId"), c.Param("userId"))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"member": member})
}

type deviceRequest struct {
	Name         string               `json:"name" binding:"required"`
	Category     model.DeviceCategory `json:"category" binding:"required"`
	SerialNumber *string              `json:"serial_number"`
	MACAddress   *string              `json:"mac_address"`
	Manufacturer *string              `json:"manufacturer"`
	Model        *string              `json:"model"`
	Metadata     map[string]any       `json:"metadata"`
}

func (r deviceRequest) input() store.DeviceInput {
	return store.DeviceInput{
		Name:         strings.TrimSpace(r.Name),
		Category:     r.Category,
		SerialNumber: trimPtr(r.SerialNumber),
		MACAddress:   trimPtr(r.MACAddress),
		Manufacturer: trimPtr(r.Manufacturer),
		Model:        trimPtr(r.Model),
		Metadata:     r.Metadata,
	}
}

func (s *Server) createDevice(c *gin.Context) {
	var req deviceRequest
	if !bind(c, &req) || !validCategory(c, req.Category) {
		return
	}
	if !requireNonBlank(c, "name", req.Name) {
		return
	}
	device, err := s.store.CreateDevice(c.Request.Context(), c.Param("orgId"), req.input())
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"device": device})
}

func (s *Server) listDevices(c *gin.Context) {
	limit, offset := pagination(c)
	devicePage, err := s.store.ListDevices(c.Request.Context(), c.Param("orgId"), limit, offset)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"devices": devicePage.Devices, "pagination": devicePage.Page})
}

func (s *Server) getDevice(c *gin.Context) {
	device, err := s.store.GetDevice(c.Request.Context(), c.Param("orgId"), c.Param("deviceId"))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"device": device})
}

func (s *Server) updateDevice(c *gin.Context) {
	var req deviceRequest
	if !bind(c, &req) || !validCategory(c, req.Category) {
		return
	}
	if !requireNonBlank(c, "name", req.Name) {
		return
	}
	device, err := s.store.UpdateDevice(c.Request.Context(), c.Param("orgId"), c.Param("deviceId"), req.input())
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"device": device})
}

func (s *Server) deleteDevice(c *gin.Context) {
	if err := s.store.DeleteDevice(c.Request.Context(), c.Param("orgId"), c.Param("deviceId")); err != nil {
		writeStoreError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

type statusRequest struct {
	Status     model.DeviceStatus `json:"status" binding:"required"`
	LastSeenAt *time.Time         `json:"last_seen_at"`
}

func (s *Server) updateDeviceStatus(c *gin.Context) {
	var req statusRequest
	if !bind(c, &req) || !validStatus(c, req.Status) {
		return
	}
	device, err := s.store.UpdateDeviceStatus(c.Request.Context(), c.Param("orgId"), c.Param("deviceId"), req.Status, req.LastSeenAt)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"device": device})
}

func (s *Server) requireAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			writeError(c, http.StatusUnauthorized, "missing_token", "Missing bearer token")
			c.Abort()
			return
		}
		claims, err := s.auth.ParseAccessToken(strings.TrimPrefix(header, "Bearer "))
		if err != nil {
			writeError(c, http.StatusUnauthorized, "invalid_token", "Invalid bearer token")
			c.Abort()
			return
		}
		if s.store != nil {
			if _, err := s.store.GetUser(c.Request.Context(), claims.UserID); err != nil {
				writeError(c, http.StatusUnauthorized, "invalid_token", "Invalid bearer token")
				c.Abort()
				return
			}
		}
		c.Set("userID", claims.UserID)
		c.Next()
	}
}

func (s *Server) requireOrgRole(allowed ...model.Role) gin.HandlerFunc {
	allowedSet := map[model.Role]bool{}
	for _, role := range allowed {
		allowedSet[role] = true
	}
	return func(c *gin.Context) {
		role, err := s.store.GetRole(c.Request.Context(), c.Param("orgId"), currentUserID(c))
		if err != nil {
			writeError(c, http.StatusNotFound, "not_found", "Resource not found")
			c.Abort()
			return
		}
		if !allowedSet[role] {
			writeError(c, http.StatusForbidden, "forbidden", "Insufficient role permissions")
			c.Abort()
			return
		}
		c.Set("role", role)
		c.Next()
	}
}

func currentUserID(c *gin.Context) string {
	value, _ := c.Get("userID")
	userID, _ := value.(string)
	return userID
}

func bind(c *gin.Context, dst any) bool {
	if err := c.ShouldBindJSON(dst); err != nil {
		writeError(c, http.StatusBadRequest, "invalid_request", err.Error())
		return false
	}
	return true
}

func validRole(c *gin.Context, role model.Role) bool {
	if role == model.RoleOwner || role == model.RoleAdmin || role == model.RoleMember {
		return true
	}
	writeError(c, http.StatusBadRequest, "invalid_role", "Invalid role")
	return false
}

func validCategory(c *gin.Context, category model.DeviceCategory) bool {
	if category == model.DeviceCategoryIPCamera || category == model.DeviceCategoryMQTT || category == model.DeviceCategoryGeneric {
		return true
	}
	writeError(c, http.StatusBadRequest, "invalid_device_category", "Invalid device category")
	return false
}

func validStatus(c *gin.Context, status model.DeviceStatus) bool {
	if status == model.DeviceStatusUnknown || status == model.DeviceStatusOnline || status == model.DeviceStatusOffline || status == model.DeviceStatusDisabled {
		return true
	}
	writeError(c, http.StatusBadRequest, "invalid_device_status", "Invalid device status")
	return false
}

func requireNonBlank(c *gin.Context, field, value string) bool {
	if strings.TrimSpace(value) != "" {
		return true
	}
	writeError(c, http.StatusBadRequest, "invalid_request", field+" must not be blank")
	return false
}

func queryInt(c *gin.Context, name string, fallback int) int {
	value := c.Query(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return fallback
	}
	return parsed
}

func pagination(c *gin.Context) (int, int) {
	limit := queryInt(c, "limit", 50)
	if limit == 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	return limit, queryInt(c, "offset", 0)
}

func trimPtr(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

func writeStoreError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(c, http.StatusNotFound, "not_found", "Resource not found")
	case errors.Is(err, store.ErrLastOwner):
		writeError(c, http.StatusConflict, "last_owner", err.Error())
	case errors.Is(err, store.ErrDisabled):
		writeError(c, http.StatusConflict, "disabled_resource", err.Error())
	case strings.Contains(err.Error(), "duplicate key"):
		writeError(c, http.StatusConflict, "conflict", "Resource already exists")
	default:
		writeError(c, http.StatusInternalServerError, "internal_error", "Internal server error")
	}
}

func writeError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}
