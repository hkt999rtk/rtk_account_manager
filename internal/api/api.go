package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
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
	store                      *store.Store
	auth                       *auth.Service
	authTokenSink              AuthTokenSink
	quotaRaiseNotificationSink QuotaRaiseNotificationSink
	signupLimiter              *signupLimiter
	signupPolicy               signupPolicy
}

var ErrAuthTokenSinkUnavailable = errors.New("auth token sink unavailable")

func newServer(store *store.Store, authService *auth.Service, sink AuthTokenSink) *Server {
	return &Server{
		store:         store,
		auth:          authService,
		authTokenSink: sink,
		signupLimiter: newSignupLimiter(5, time.Hour),
		signupPolicy:  loadSignupPolicy(),
	}
}

func New(store *store.Store, authService *auth.Service) *Server {
	return newServer(store, authService, nil)
}

type AuthTokenDelivery struct {
	Purpose   string
	Email     string
	Token     string
	ExpiresAt time.Time
}

type AuthTokenSink interface {
	DeliverAuthToken(context.Context, AuthTokenDelivery) error
}

type LogAuthTokenSink struct {
	logger *log.Logger
}

func NewLogAuthTokenSink(logger *log.Logger) LogAuthTokenSink {
	return LogAuthTokenSink{logger: logger}
}

func (s LogAuthTokenSink) DeliverAuthToken(_ context.Context, delivery AuthTokenDelivery) error {
	logger := s.logger
	if logger == nil {
		logger = log.Default()
	}
	logger.Printf(
		"auth token delivery purpose=%s email=%s token=%s expires_at=%s",
		delivery.Purpose,
		delivery.Email,
		delivery.Token,
		delivery.ExpiresAt.Format(time.RFC3339),
	)
	return nil
}

func NewWithAuthTokenSink(store *store.Store, authService *auth.Service, sink AuthTokenSink) *Server {
	return newServer(store, authService, sink)
}

type QuotaRaiseNotificationDelivery struct {
	RecipientEmail   string
	RecipientName    *string
	OrganizationID   string
	OrganizationName string
	RequestedQuota   int
	ApprovedQuota    *int
	DecisionReason   *string
	Decision         string
}

type QuotaRaiseNotificationSink interface {
	DeliverQuotaRaiseNotification(context.Context, QuotaRaiseNotificationDelivery) error
}

type LogQuotaRaiseNotificationSink struct {
	logger *log.Logger
}

func NewLogQuotaRaiseNotificationSink(logger *log.Logger) LogQuotaRaiseNotificationSink {
	return LogQuotaRaiseNotificationSink{logger: logger}
}

func (s LogQuotaRaiseNotificationSink) DeliverQuotaRaiseNotification(_ context.Context, delivery QuotaRaiseNotificationDelivery) error {
	logger := s.logger
	if logger == nil {
		logger = log.Default()
	}
	logger.Printf(
		"quota raise notification decision=%s email=%s org_id=%s org_name=%s requested_quota=%d approved_quota=%v",
		delivery.Decision,
		delivery.RecipientEmail,
		delivery.OrganizationID,
		delivery.OrganizationName,
		delivery.RequestedQuota,
		delivery.ApprovedQuota,
	)
	return nil
}

func NewWithAuthTokenAndNotificationSink(store *store.Store, authService *auth.Service, authSink AuthTokenSink, notificationSink QuotaRaiseNotificationSink) *Server {
	server := newServer(store, authService, authSink)
	server.quotaRaiseNotificationSink = notificationSink
	return server
}

func (s *Server) Router() *gin.Engine {
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())

	v1 := r.Group("/v1")
	v1.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	v1.POST("/auth/register", s.register)
	v1.POST("/auth/signup", s.signup)
	v1.POST("/auth/login", s.login)
	v1.POST("/auth/refresh", s.refresh)
	v1.POST("/auth/verify-email", s.verifyEmail)
	v1.POST("/auth/resend-verification", s.resendVerification)
	v1.POST("/auth/forgot-password", s.forgotPassword)
	v1.POST("/auth/reset-password", s.resetPassword)

	protected := v1.Group("")
	protected.Use(s.requireAuth())
	protected.POST("/auth/logout", s.logout)
	protected.GET("/me", s.me)
	protected.DELETE("/me", s.deleteCurrentUser)
	protected.PATCH("/me/password", s.changePassword)

	protected.GET("/orgs", s.listOrganizations)
	protected.POST("/orgs", s.createOrganization)
	protected.GET("/orgs/:orgId", s.getOrganization)
	protected.PATCH("/orgs/:orgId", s.requireOrgRole(model.RoleOwner), s.updateOrganization)
	protected.POST("/orgs/:orgId/quota-raise-requests", s.requireOrgRole(model.RoleOwner, model.RoleAdmin, model.RoleMember), s.createQuotaRaiseRequest)
	protected.GET("/orgs/:orgId/members", s.requireOrgRole(model.RoleOwner, model.RoleAdmin, model.RoleMember), s.listMembers)
	protected.POST("/orgs/:orgId/members", s.requireOrgRole(model.RoleOwner), s.addMember)
	protected.PATCH("/orgs/:orgId/members/:userId", s.requireOrgRole(model.RoleOwner), s.updateMemberRole)
	protected.PATCH("/orgs/:orgId/members/:userId/disable", s.requireOrgRole(model.RoleOwner), s.disableMemberUser)
	protected.PATCH("/orgs/:orgId/members/:userId/enable", s.requireOrgRole(model.RoleOwner), s.enableMemberUser)
	protected.DELETE("/orgs/:orgId/members/:userId", s.requireOrgRole(model.RoleOwner), s.removeMember)

	protected.GET("/orgs/:orgId/device-groups", s.requireOrgRole(model.RoleOwner, model.RoleAdmin, model.RoleMember), s.listDeviceGroups)
	protected.POST("/orgs/:orgId/device-groups", s.requireOrgRole(model.RoleOwner, model.RoleAdmin), s.createDeviceGroup)
	protected.GET("/orgs/:orgId/device-groups/:groupId", s.requireOrgRole(model.RoleOwner, model.RoleAdmin, model.RoleMember), s.getDeviceGroup)
	protected.PATCH("/orgs/:orgId/device-groups/:groupId", s.requireOrgRole(model.RoleOwner, model.RoleAdmin), s.updateDeviceGroup)
	protected.DELETE("/orgs/:orgId/device-groups/:groupId", s.requireOrgRole(model.RoleOwner, model.RoleAdmin), s.deleteDeviceGroup)
	protected.GET("/orgs/:orgId/device-groups/:groupId/devices", s.requireOrgRole(model.RoleOwner, model.RoleAdmin, model.RoleMember), s.listDeviceGroupDevices)
	protected.PUT("/orgs/:orgId/device-groups/:groupId/devices/:deviceId", s.requireOrgRole(model.RoleOwner, model.RoleAdmin), s.addDeviceToGroup)
	protected.DELETE("/orgs/:orgId/device-groups/:groupId/devices/:deviceId", s.requireOrgRole(model.RoleOwner, model.RoleAdmin), s.removeDeviceFromGroup)

	protected.POST("/orgs/:orgId/devices", s.requireOrgRole(model.RoleOwner, model.RoleAdmin), s.createDevice)
	protected.GET("/orgs/:orgId/devices", s.requireOrgRole(model.RoleOwner, model.RoleAdmin, model.RoleMember), s.listDevices)
	protected.GET("/orgs/:orgId/devices/:deviceId", s.requireOrgRole(model.RoleOwner, model.RoleAdmin, model.RoleMember), s.getDevice)
	protected.GET("/orgs/:orgId/devices/:deviceId/tags", s.requireOrgRole(model.RoleOwner, model.RoleAdmin, model.RoleMember), s.listDeviceTags)
	protected.PUT("/orgs/:orgId/devices/:deviceId/tags/:tag", s.requireOrgRole(model.RoleOwner, model.RoleAdmin), s.addDeviceTag)
	protected.DELETE("/orgs/:orgId/devices/:deviceId/tags/:tag", s.requireOrgRole(model.RoleOwner, model.RoleAdmin), s.deleteDeviceTag)
	protected.POST("/orgs/:orgId/devices/:deviceId/provision", s.requireOrgRole(model.RoleOwner, model.RoleAdmin), s.provisionDevice)
	protected.GET("/orgs/:orgId/devices/:deviceId/provisioning", s.requireOrgRole(model.RoleOwner, model.RoleAdmin, model.RoleMember), s.getProvisioningState)
	protected.POST("/orgs/:orgId/devices/:deviceId/deactivate", s.requireOrgRole(model.RoleOwner, model.RoleAdmin), s.deactivateDevice)
	protected.PATCH("/orgs/:orgId/devices/:deviceId", s.requireOrgRole(model.RoleOwner, model.RoleAdmin), s.updateDevice)
	protected.DELETE("/orgs/:orgId/devices/:deviceId", s.requireOrgRole(model.RoleOwner, model.RoleAdmin), s.deleteDevice)
	protected.PATCH("/orgs/:orgId/devices/:deviceId/status", s.requireOrgRole(model.RoleOwner, model.RoleAdmin), s.updateDeviceStatus)

	protected.POST("/admin/quota-raise-requests/:requestId/approve", s.requirePlatformAdmin(), s.approveQuotaRaiseRequest)
	protected.POST("/admin/quota-raise-requests/:requestId/decline", s.requirePlatformAdmin(), s.declineQuotaRaiseRequest)
	protected.GET("/admin/metrics", s.requirePlatformAdmin(), s.adminMetrics)

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
		Email:                     strings.ToLower(strings.TrimSpace(req.Email)),
		PasswordHash:              hash,
		DisplayName:               req.DisplayName,
		OrganizationName:          strings.TrimSpace(req.OrganizationName),
		OrganizationTier:          model.OrganizationTierCommercial,
		EvaluationDeviceQuota:     5,
		SignupPendingVerification: false,
	})
	if err != nil {
		writeStoreError(c, err)
		return
	}
	token, expiresAt, err := s.createAuthToken(c, result.User.ID, "email_verification")
	if err != nil {
		writeAuthTokenStoreError(c, err, "Could not issue verification token")
		return
	}
	if err := s.deliverAuthToken(c, result.User.Email, "email_verification", token, expiresAt); err != nil {
		writeError(c, http.StatusInternalServerError, "token_delivery_failed", "Could not deliver verification token")
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
	if user.SignupPendingVerification {
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

type authTokenRequest struct {
	Token string `json:"token" binding:"required"`
}

func (s *Server) verifyEmail(c *gin.Context) {
	var req authTokenRequest
	if !bind(c, &req) {
		return
	}
	if !requireNonBlank(c, "token", req.Token) {
		return
	}
	user, err := s.store.VerifyEmailToken(c.Request.Context(), auth.HashToken(req.Token))
	if err != nil {
		writeError(c, http.StatusBadRequest, "invalid_token", "Invalid or expired verification token")
		return
	}
	c.JSON(http.StatusOK, gin.H{"user": user})
}

type emailRequest struct {
	Email string `json:"email" binding:"required,email"`
}

func (s *Server) resendVerification(c *gin.Context) {
	var req emailRequest
	if !bind(c, &req) {
		return
	}
	token, expiresAt, err := s.newAuthToken()
	if err != nil {
		writeError(c, http.StatusInternalServerError, "token_issue_failed", "Could not issue verification token")
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	created, err := s.store.CreateEmailVerificationTokenForEmail(c.Request.Context(), email, auth.HashToken(token), expiresAt)
	if err != nil {
		if errors.Is(err, store.ErrRateLimited) {
			c.Status(http.StatusAccepted)
			return
		}
		writeAuthTokenStoreError(c, err, "Could not issue verification token")
		return
	}
	if created {
		_ = s.deliverAuthToken(c, email, "email_verification", token, expiresAt)
	}
	c.Status(http.StatusAccepted)
}

func (s *Server) forgotPassword(c *gin.Context) {
	var req emailRequest
	if !bind(c, &req) {
		return
	}
	token, expiresAt, err := s.newAuthToken()
	if err != nil {
		writeError(c, http.StatusInternalServerError, "token_issue_failed", "Could not issue reset token")
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	created, err := s.store.CreatePasswordResetTokenForEmail(c.Request.Context(), email, auth.HashToken(token), expiresAt)
	if err != nil {
		if errors.Is(err, store.ErrRateLimited) {
			c.Status(http.StatusAccepted)
			return
		}
		writeAuthTokenStoreError(c, err, "Could not issue reset token")
		return
	}
	if created {
		_ = s.deliverAuthToken(c, email, "password_reset", token, expiresAt)
	}
	c.Status(http.StatusAccepted)
}

type resetPasswordRequest struct {
	Token       string `json:"token" binding:"required"`
	NewPassword string `json:"new_password" binding:"required,min=8"`
}

func (s *Server) resetPassword(c *gin.Context) {
	var req resetPasswordRequest
	if !bind(c, &req) {
		return
	}
	if !requireNonBlank(c, "token", req.Token) {
		return
	}
	newHash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "password_hash_failed", "Could not hash password")
		return
	}
	if err := s.store.ResetPasswordWithToken(c.Request.Context(), auth.HashToken(req.Token), newHash); err != nil {
		writeError(c, http.StatusBadRequest, "invalid_token", "Invalid or expired reset token")
		return
	}
	c.Status(http.StatusNoContent)
}

func (s *Server) createAuthToken(c *gin.Context, userID, purpose string) (string, time.Time, error) {
	token, expiresAt, err := s.newAuthToken()
	if err != nil {
		return "", time.Time{}, err
	}
	tokenHash := auth.HashToken(token)
	switch purpose {
	case "email_verification":
		return token, expiresAt, s.store.CreateEmailVerificationToken(c.Request.Context(), userID, tokenHash, expiresAt)
	case "password_reset":
		return token, expiresAt, s.store.CreatePasswordResetToken(c.Request.Context(), userID, tokenHash, expiresAt)
	default:
		return "", time.Time{}, errors.New("unsupported token purpose")
	}
}

func (s *Server) deliverAuthToken(c *gin.Context, email, purpose, token string, expiresAt time.Time) error {
	if s.authTokenSink == nil {
		return ErrAuthTokenSinkUnavailable
	}
	return s.authTokenSink.DeliverAuthToken(c.Request.Context(), AuthTokenDelivery{
		Purpose:   purpose,
		Email:     email,
		Token:     token,
		ExpiresAt: expiresAt,
	})
}

func (s *Server) newAuthToken() (string, time.Time, error) {
	token, err := auth.RandomToken()
	if err != nil {
		return "", time.Time{}, err
	}
	return token, time.Now().UTC().Add(30 * time.Minute), nil
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

func (s *Server) deleteCurrentUser(c *gin.Context) {
	if err := s.store.DisableCurrentUser(c.Request.Context(), currentUserID(c)); err != nil {
		writeStoreError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password" binding:"required"`
	NewPassword     string `json:"new_password" binding:"required,min=8"`
}

func (s *Server) changePassword(c *gin.Context) {
	var req changePasswordRequest
	if !bind(c, &req) {
		return
	}

	userID := currentUserID(c)
	_, hash, err := s.store.GetUserPasswordByID(c.Request.Context(), userID)
	if err != nil || !auth.CheckPassword(hash, req.CurrentPassword) {
		writeError(c, http.StatusUnauthorized, "invalid_credentials", "Invalid current password")
		return
	}

	newHash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "password_hash_failed", "Could not hash password")
		return
	}
	if err := s.store.UpdateUserPassword(c.Request.Context(), userID, newHash); err != nil {
		writeStoreError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
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

func (s *Server) updateOrganization(c *gin.Context) {
	var req orgRequest
	if !bind(c, &req) {
		return
	}
	if !requireNonBlank(c, "name", req.Name) {
		return
	}
	org, err := s.store.UpdateOrganization(c.Request.Context(), c.Param("orgId"), currentUserID(c), strings.TrimSpace(req.Name))
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

type deviceGroupRequest struct {
	Name        string  `json:"name" binding:"required"`
	Description *string `json:"description"`
}

func (r deviceGroupRequest) input() store.DeviceGroupInput {
	return store.DeviceGroupInput{
		Name:        strings.TrimSpace(r.Name),
		Description: trimPtr(r.Description),
	}
}

func (s *Server) createDeviceGroup(c *gin.Context) {
	var req deviceGroupRequest
	if !bind(c, &req) {
		return
	}
	if !requireNonBlank(c, "name", req.Name) {
		return
	}
	group, err := s.store.CreateDeviceGroup(c.Request.Context(), c.Param("orgId"), req.input())
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"group": group})
}

func (s *Server) listDeviceGroups(c *gin.Context) {
	limit, offset := pagination(c)
	groupPage, err := s.store.ListDeviceGroups(c.Request.Context(), c.Param("orgId"), limit, offset)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"groups": groupPage.Groups, "pagination": groupPage.Page})
}

func (s *Server) getDeviceGroup(c *gin.Context) {
	group, err := s.store.GetDeviceGroup(c.Request.Context(), c.Param("orgId"), c.Param("groupId"))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"group": group})
}

func (s *Server) updateDeviceGroup(c *gin.Context) {
	var req deviceGroupRequest
	if !bind(c, &req) {
		return
	}
	if !requireNonBlank(c, "name", req.Name) {
		return
	}
	group, err := s.store.UpdateDeviceGroup(c.Request.Context(), c.Param("orgId"), c.Param("groupId"), req.input())
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"group": group})
}

func (s *Server) deleteDeviceGroup(c *gin.Context) {
	if err := s.store.DeleteDeviceGroup(c.Request.Context(), c.Param("orgId"), c.Param("groupId")); err != nil {
		writeStoreError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (s *Server) addDeviceToGroup(c *gin.Context) {
	if err := s.store.AddDeviceToGroup(c.Request.Context(), c.Param("orgId"), c.Param("groupId"), c.Param("deviceId")); err != nil {
		writeStoreError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (s *Server) removeDeviceFromGroup(c *gin.Context) {
	if err := s.store.RemoveDeviceFromGroup(c.Request.Context(), c.Param("orgId"), c.Param("groupId"), c.Param("deviceId")); err != nil {
		writeStoreError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (s *Server) listDeviceGroupDevices(c *gin.Context) {
	limit, offset := pagination(c)
	devicePage, err := s.store.ListDeviceGroupDevices(c.Request.Context(), c.Param("orgId"), c.Param("groupId"), limit, offset)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"devices": devicePage.Devices, "pagination": devicePage.Page})
}

func (s *Server) addDeviceTag(c *gin.Context) {
	tag := strings.TrimSpace(c.Param("tag"))
	if !requireNonBlank(c, "tag", tag) {
		return
	}
	deviceTag, err := s.store.AddDeviceTag(c.Request.Context(), c.Param("orgId"), c.Param("deviceId"), tag)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"tag": deviceTag})
}

func (s *Server) deleteDeviceTag(c *gin.Context) {
	tag := strings.TrimSpace(c.Param("tag"))
	if !requireNonBlank(c, "tag", tag) {
		return
	}
	if err := s.store.DeleteDeviceTag(c.Request.Context(), c.Param("orgId"), c.Param("deviceId"), tag); err != nil {
		writeStoreError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (s *Server) listDeviceTags(c *gin.Context) {
	limit, offset := pagination(c)
	tagPage, err := s.store.ListDeviceTags(c.Request.Context(), c.Param("orgId"), c.Param("deviceId"), limit, offset)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"tags": tagPage.Tags, "pagination": tagPage.Page})
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

func bindOptional(c *gin.Context, dst any) bool {
	if c.Request.ContentLength == 0 {
		return true
	}
	return bind(c, dst)
}

func bindStrict(c *gin.Context, dst any) bool {
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		writeError(c, http.StatusBadRequest, "invalid_request", err.Error())
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			writeError(c, http.StatusBadRequest, "invalid_request", "request body must contain a single JSON value")
		} else {
			writeError(c, http.StatusBadRequest, "invalid_request", err.Error())
		}
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
	case errors.Is(err, store.ErrNotProvisioned):
		writeError(c, http.StatusConflict, "device_not_provisioned", "Device is missing projected video metadata")
	case errors.Is(err, store.ErrConflict):
		writeError(c, http.StatusConflict, "conflict", "Resource already exists with conflicting data")
	case errors.Is(err, store.ErrRateLimited):
		writeError(c, http.StatusTooManyRequests, "rate_limited", "Too many token requests")
	case errors.Is(err, store.ErrEvaluationQuotaExceeded):
		writeError(c, http.StatusConflict, "EVALUATION_QUOTA_EXCEEDED", "Evaluation device quota exceeded")
	case errors.Is(err, errOperationStateInconsistent):
		writeError(c, http.StatusInternalServerError, "operation_state_inconsistent", err.Error())
	case strings.Contains(err.Error(), "duplicate key"):
		writeError(c, http.StatusConflict, "conflict", "Resource already exists")
	default:
		writeError(c, http.StatusInternalServerError, "internal_error", "Internal server error")
	}
}

func writeAuthTokenStoreError(c *gin.Context, err error, message string) {
	if errors.Is(err, store.ErrRateLimited) {
		writeError(c, http.StatusTooManyRequests, "rate_limited", "Too many token requests")
		return
	}
	writeError(c, http.StatusInternalServerError, "token_issue_failed", message)
}

func writeError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}
