package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	cloudlogger "github.com/hkt999rtk/rtk_cloud_logger"
	"go.uber.org/zap"

	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

type Server struct {
	store                      Store
	auth                       *auth.Service
	authTokenSink              AuthTokenSink
	quotaRaiseNotificationSink QuotaRaiseNotificationSink
	signupLimiter              *signupLimiter
	signupPolicy               signupPolicy
	oidcResolver               auth.ProviderResolver
	oidcClient                 auth.OIDCClient
	oidcStateTTL               time.Duration
	oidcEnvClientSecretRef     string
	appCertificateIssuer       AppCertificateIssuer
	internalAuthToken          string
	logger                     *zap.Logger
}

var ErrAuthTokenSinkUnavailable = errors.New("auth token sink unavailable")

func newServer(store Store, authService *auth.Service, sink AuthTokenSink) *Server {
	return &Server{
		store:         store,
		auth:          authService,
		authTokenSink: sink,
		signupLimiter: newSignupLimiter(5, time.Hour),
		signupPolicy:  loadSignupPolicy(),
		logger:        cloudlogger.Nop(),
	}
}

func New(store Store, authService *auth.Service) *Server {
	return newServer(store, authService, nil)
}

type OIDCOptions struct {
	Env             auth.OIDCEnvConfig
	HTTPClient      *http.Client
	Now             func() time.Time
	StateTTL        time.Duration
	ClientSecretRef string
}

func (s *Server) ConfigureOIDC(options OIDCOptions) {
	s.oidcResolver = auth.ProviderResolver{
		Store: s.store,
		Env:   options.Env,
		IsNotFound: func(err error) bool {
			return errors.Is(err, store.ErrNotFound)
		},
	}
	s.oidcClient = auth.OIDCClient{
		HTTPClient: options.HTTPClient,
		Now:        options.Now,
	}
	s.oidcStateTTL = options.StateTTL
	if s.oidcStateTTL <= 0 {
		s.oidcStateTTL = 10 * time.Minute
	}
	s.oidcEnvClientSecretRef = options.ClientSecretRef
	if s.oidcEnvClientSecretRef == "" {
		s.oidcEnvClientSecretRef = "env:OIDC_CLIENT_SECRET"
	}
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
	logger *zap.Logger
}

func NewLogAuthTokenSink(logger *zap.Logger) LogAuthTokenSink {
	return LogAuthTokenSink{logger: logger}
}

func (s LogAuthTokenSink) DeliverAuthToken(_ context.Context, delivery AuthTokenDelivery) error {
	logger := s.logger
	if logger == nil {
		logger = cloudlogger.Nop()
	}
	logger.Info(
		"auth token delivery",
		zap.String("purpose", delivery.Purpose),
		zap.String("email", delivery.Email),
		zap.Time("expires_at", delivery.ExpiresAt.UTC()),
		zap.Bool("token_redacted", strings.TrimSpace(delivery.Token) != ""),
	)
	return nil
}

func NewWithAuthTokenSink(store Store, authService *auth.Service, sink AuthTokenSink) *Server {
	return newServer(store, authService, sink)
}

func (s *Server) ConfigureAppCertificateIssuer(issuer AppCertificateIssuer) {
	s.appCertificateIssuer = issuer
}

func (s *Server) ConfigureInternalAuthToken(token string) {
	s.internalAuthToken = strings.TrimSpace(token)
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
	logger *zap.Logger
}

func NewLogQuotaRaiseNotificationSink(logger *zap.Logger) LogQuotaRaiseNotificationSink {
	return LogQuotaRaiseNotificationSink{logger: logger}
}

func (s LogQuotaRaiseNotificationSink) DeliverQuotaRaiseNotification(_ context.Context, delivery QuotaRaiseNotificationDelivery) error {
	logger := s.logger
	if logger == nil {
		logger = cloudlogger.Nop()
	}
	fields := []zap.Field{
		zap.String("decision", delivery.Decision),
		zap.String("email", delivery.RecipientEmail),
		zap.String("org_id", delivery.OrganizationID),
		zap.String("org_name", delivery.OrganizationName),
		zap.Int("requested_quota", delivery.RequestedQuota),
	}
	if delivery.ApprovedQuota != nil {
		fields = append(fields, zap.Int("approved_quota", *delivery.ApprovedQuota))
	}
	logger.Info(
		"quota raise notification",
		fields...,
	)
	return nil
}

type SMTPQuotaRaiseNotificationSink struct {
	host     string
	from     string
	auth     smtp.Auth
	sendMail func(string, smtp.Auth, string, []string, []byte) error
}

func NewSMTPQuotaRaiseNotificationSink(host, from string, auth smtp.Auth) SMTPQuotaRaiseNotificationSink {
	return SMTPQuotaRaiseNotificationSink{
		host:     host,
		from:     from,
		auth:     auth,
		sendMail: smtp.SendMail,
	}
}

func (s SMTPQuotaRaiseNotificationSink) DeliverQuotaRaiseNotification(_ context.Context, delivery QuotaRaiseNotificationDelivery) error {
	if s.sendMail == nil {
		return errors.New("smtp quota raise notification sink unavailable")
	}
	subject := fmt.Sprintf("Quota raise %s", delivery.Decision)
	body := buildQuotaRaiseNotificationBody(delivery)
	msg := buildSMTPMessage(s.from, delivery.RecipientEmail, subject, body)
	return s.sendMail(s.host, s.auth, s.from, []string{delivery.RecipientEmail}, msg)
}

func buildQuotaRaiseNotificationBody(delivery QuotaRaiseNotificationDelivery) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "Quota raise decision: %s\r\n", delivery.Decision)
	fmt.Fprintf(&b, "Organization: %s (%s)\r\n", delivery.OrganizationName, delivery.OrganizationID)
	if delivery.RecipientName != nil && strings.TrimSpace(*delivery.RecipientName) != "" {
		fmt.Fprintf(&b, "Requester: %s <%s>\r\n", strings.TrimSpace(*delivery.RecipientName), delivery.RecipientEmail)
	} else {
		fmt.Fprintf(&b, "Requester: <%s>\r\n", delivery.RecipientEmail)
	}
	fmt.Fprintf(&b, "Requested quota: %d\r\n", delivery.RequestedQuota)
	if delivery.ApprovedQuota != nil {
		fmt.Fprintf(&b, "Approved quota: %d\r\n", *delivery.ApprovedQuota)
	}
	if delivery.DecisionReason != nil && strings.TrimSpace(*delivery.DecisionReason) != "" {
		fmt.Fprintf(&b, "Decision reason: %s\r\n", strings.TrimSpace(*delivery.DecisionReason))
	}
	return b.String()
}

func buildSMTPMessage(from, to, subject, body string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(body)
	return b.Bytes()
}

func NewWithAuthTokenAndNotificationSink(store Store, authService *auth.Service, authSink AuthTokenSink, notificationSink QuotaRaiseNotificationSink) *Server {
	server := newServer(store, authService, authSink)
	server.quotaRaiseNotificationSink = notificationSink
	return server
}

func (s *Server) SetLogger(logger *zap.Logger) {
	if logger == nil {
		logger = cloudlogger.Nop()
	}
	s.logger = logger
}

func (s *Server) requestLogger() gin.HandlerFunc {
	logger := s.logger
	if logger == nil {
		logger = cloudlogger.Nop()
	}
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		status := c.Writer.Status()
		if status == 0 {
			status = http.StatusOK
		}
		fields := []zap.Field{
			zap.String("method", c.Request.Method),
			zap.String("path", cloudlogger.SanitizePath(c.Request.URL.RequestURI())),
			zap.Int("status", status),
			zap.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000.0),
			zap.String("remote_addr", c.ClientIP()),
		}
		if requestID := strings.TrimSpace(c.Request.Header.Get("X-Request-Id")); requestID != "" {
			fields = append(fields, zap.String("request_id", requestID))
		}
		logger.Info("http request", fields...)
	}
}

func (s *Server) recoveryLogger() gin.HandlerFunc {
	logger := s.logger
	if logger == nil {
		logger = cloudlogger.Nop()
	}
	return func(c *gin.Context) {
		defer func() {
			if recovered := recover(); recovered != nil {
				fields := []zap.Field{
					zap.String("method", c.Request.Method),
					zap.String("path", cloudlogger.SanitizePath(c.Request.URL.RequestURI())),
					zap.Any("panic", recovered),
				}
				if requestID := strings.TrimSpace(c.Request.Header.Get("X-Request-Id")); requestID != "" {
					fields = append(fields, zap.String("request_id", requestID))
				}
				logger.Error("panic recovered", fields...)
				c.AbortWithStatus(http.StatusInternalServerError)
			}
		}()
		c.Next()
	}
}

func (s *Server) Router() *gin.Engine {
	r := gin.New()
	r.Use(s.requestLogger(), s.recoveryLogger())
	r.GET("/metrics/prometheus", s.prometheusMetrics)

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
	v1.GET("/auth/oidc/providers", s.listOIDCProviders)
	v1.GET("/auth/oidc/:providerId/login", s.startOIDCLogin)
	v1.GET("/auth/oidc/:providerId/callback", s.handleOIDCCallback)
	v1.POST("/brand-clouds/:tenantSlug/auth/login", s.brandCloudLogin)
	v1.POST("/brand-clouds/:tenantSlug/auth/refresh", s.brandCloudRefresh)
	v1.POST("/internal/app-token-authorizations", s.handleInternalAppTokenAuthorization)
	v1.POST("/internal/device-provisioning-results", s.handleInternalDeviceProvisioningResult)

	protected := v1.Group("")
	protected.Use(s.requireAuth())
	protected.POST("/auth/logout", s.logout)
	protected.POST("/brand-clouds/:tenantSlug/auth/logout", s.brandCloudLogout)
	protected.GET("/brand-clouds/:tenantSlug/me", s.brandCloudMe)
	protected.GET("/me", s.me)
	protected.DELETE("/me", s.deleteCurrentUser)
	protected.PATCH("/me/password", s.changePassword)
	protected.GET("/me/identities", s.listCurrentUserIdentities)
	protected.DELETE("/me/identities/:identityId", s.deleteCurrentUserIdentity)

	protected.GET("/orgs", s.listOrganizations)
	protected.POST("/orgs", s.createOrganization)
	protected.GET("/orgs/:orgId", s.getOrganization)
	protected.PATCH("/orgs/:orgId", s.requirePermission("organization.update"), s.updateOrganization)
	protected.POST("/orgs/:orgId/quota-raise-requests", s.requirePermission("quota_request.create"), s.createQuotaRaiseRequest)
	protected.GET("/orgs/:orgId/members", s.requirePermission("membership.read"), s.listMembers)
	protected.POST("/orgs/:orgId/members", s.requirePermission("membership.manage"), s.addMember)
	protected.PATCH("/orgs/:orgId/members/:userId", s.requirePermission("membership.manage"), s.updateMemberRole)
	protected.PATCH("/orgs/:orgId/members/:userId/disable", s.requirePermission("membership.manage"), s.disableMemberUser)
	protected.PATCH("/orgs/:orgId/members/:userId/enable", s.requirePermission("membership.manage"), s.enableMemberUser)
	protected.DELETE("/orgs/:orgId/members/:userId", s.requirePermission("membership.manage"), s.removeMember)

	protected.GET("/orgs/:orgId/device-groups", s.requirePermission("device_group.read"), s.listDeviceGroups)
	protected.POST("/orgs/:orgId/device-groups", s.requirePermission("device_group.manage"), s.createDeviceGroup)
	protected.GET("/orgs/:orgId/device-groups/:groupId", s.requirePermission("device_group.read"), s.getDeviceGroup)
	protected.PATCH("/orgs/:orgId/device-groups/:groupId", s.requirePermission("device_group.manage"), s.updateDeviceGroup)
	protected.DELETE("/orgs/:orgId/device-groups/:groupId", s.requirePermission("device_group.manage"), s.deleteDeviceGroup)
	protected.GET("/orgs/:orgId/device-groups/:groupId/devices", s.requirePermission("device_group.read"), s.listDeviceGroupDevices)
	protected.PUT("/orgs/:orgId/device-groups/:groupId/devices/:deviceId", s.requirePermission("device_group.assign"), s.addDeviceToGroup)
	protected.DELETE("/orgs/:orgId/device-groups/:groupId/devices/:deviceId", s.requirePermission("device_group.assign"), s.removeDeviceFromGroup)

	protected.POST("/orgs/:orgId/devices", s.requirePermission("registry_device.manage"), s.createDevice)
	protected.GET("/orgs/:orgId/devices", s.requirePermission("registry_device.read"), s.listDevices)
	protected.POST("/orgs/:orgId/devices/claim/resolve", s.requirePermission("claim.resolve"), s.resolveDeviceClaim)
	protected.GET("/orgs/:orgId/devices/:deviceId", s.requirePermission("registry_device.read"), s.getDevice)
	protected.GET("/orgs/:orgId/devices/:deviceId/tags", s.requirePermission("device_tag.read"), s.listDeviceTags)
	protected.PUT("/orgs/:orgId/devices/:deviceId/tags/:tag", s.requirePermission("device_tag.assign"), s.addDeviceTag)
	protected.DELETE("/orgs/:orgId/devices/:deviceId/tags/:tag", s.requirePermission("device_tag.assign"), s.deleteDeviceTag)
	protected.POST("/orgs/:orgId/devices/:deviceId/provision", s.requirePermission("lifecycle_operation.provision"), s.provisionDevice)
	protected.GET("/orgs/:orgId/devices/:deviceId/provisioning", s.requirePermission("lifecycle_operation.inspect"), s.getProvisioningState)
	protected.POST("/orgs/:orgId/devices/:deviceId/deactivate", s.requirePermission("lifecycle_operation.deactivate"), s.deactivateDevice)
	protected.POST("/orgs/:orgId/devices/:deviceId/unprovision", s.requirePermission("device.unprovision"), s.unprovisionDevice)
	protected.PATCH("/orgs/:orgId/devices/:deviceId", s.requirePermission("registry_device.manage"), s.updateDevice)
	protected.DELETE("/orgs/:orgId/devices/:deviceId", s.requirePermission("registry_device.manage"), s.deleteDevice)
	protected.PATCH("/orgs/:orgId/devices/:deviceId/status", s.requirePermission("registry_device.manage"), s.updateDeviceStatus)

	protected.POST("/admin/quota-raise-requests/:requestId/approve", s.requirePlatformAdmin(), s.approveQuotaRaiseRequest)
	protected.POST("/admin/quota-raise-requests/:requestId/decline", s.requirePlatformAdmin(), s.declineQuotaRaiseRequest)
	protected.GET("/admin/metrics", s.requirePlatformAdmin(), s.adminMetrics)
	protected.POST("/admin/brand-clouds", s.requirePlatformAdmin(), s.createBrandCloud)
	protected.GET("/admin/brand-clouds", s.requirePlatformAdmin(), s.listBrandClouds)
	protected.GET("/admin/brand-clouds/:brandCloudId", s.requirePlatformAdmin(), s.getBrandCloud)
	protected.PATCH("/admin/brand-clouds/:brandCloudId", s.requirePlatformAdmin(), s.updateBrandCloud)
	protected.POST("/admin/brand-clouds/:brandCloudId/device-item-profiles", s.requirePlatformAdmin(), s.createDeviceItemProfile)
	protected.GET("/admin/brand-clouds/:brandCloudId/device-item-profiles", s.requirePlatformAdmin(), s.listDeviceItemProfiles)
	protected.GET("/admin/brand-clouds/:brandCloudId/device-item-profiles/:profileId", s.requirePlatformAdmin(), s.getDeviceItemProfile)
	protected.PATCH("/admin/brand-clouds/:brandCloudId/device-item-profiles/:profileId", s.requirePlatformAdmin(), s.updateDeviceItemProfile)
	protected.POST("/admin/brand-clouds/:brandCloudId/device-item-profiles/:profileId/disable", s.requirePlatformAdmin(), s.disableDeviceItemProfile)
	protected.POST("/admin/brand-clouds/:brandCloudId/members", s.requirePlatformAdmin(), s.assignBrandCloudMember)
	protected.POST("/admin/brand-clouds/:brandCloudId/users", s.requirePlatformAdmin(), s.createBrandCloudUser)
	protected.POST("/admin/device-claim-tokens", s.requirePlatformAdmin(), s.createDeviceClaimToken)
	protected.GET("/admin/device-claim-tokens", s.requirePlatformAdmin(), s.listDeviceClaimTokens)
	protected.GET("/admin/device-claim-tokens/:tokenId", s.requirePlatformAdmin(), s.getDeviceClaimToken)
	protected.POST("/admin/device-claim-tokens/:tokenId/revoke", s.requirePlatformAdmin(), s.revokeDeviceClaimToken)
	protected.POST("/admin/device-claim-tokens/:tokenId/reclaim", s.requirePlatformAdmin(), s.reclaimDeviceClaimToken)
	protected.POST("/admin/device-claims/:claimId/transfer", s.requirePlatformAdmin(), s.transferDeviceClaim)
	protected.POST("/admin/devices/:deviceId/unprovision", s.requirePlatformAdmin(), s.adminUnprovisionDevice)
	protected.POST("/admin/identity-providers", s.requirePlatformAdmin(), s.createIdentityProvider)
	protected.GET("/admin/identity-providers", s.requirePlatformAdmin(), s.listIdentityProviders)
	protected.GET("/admin/identity-providers/:providerId", s.requirePlatformAdmin(), s.getIdentityProvider)
	protected.PATCH("/admin/identity-providers/:providerId", s.requirePlatformAdmin(), s.updateIdentityProvider)
	protected.DELETE("/admin/identity-providers/:providerId", s.requirePlatformAdmin(), s.deleteIdentityProvider)
	protected.GET("/admin/quota-raise-requests", s.requirePlatformAdmin(), s.listAdminQuotaRaiseRequests)
	protected.GET("/admin/quota-raise-requests/:requestId", s.requirePlatformAdmin(), s.getAdminQuotaRaiseRequest)
	protected.GET("/admin/audit-events", s.requirePlatformAdmin(), s.listAdminAuditEvents)
	protected.GET("/admin/acl/permissions", s.requirePlatformAdmin(), s.listACLPermissions)
	protected.GET("/admin/acl/roles", s.requirePlatformAdmin(), s.listACLRoles)
	protected.POST("/admin/acl/roles", s.requirePlatformAdmin(), s.createACLRole)
	protected.GET("/admin/acl/roles/:roleName", s.requirePlatformAdmin(), s.getACLRole)
	protected.PATCH("/admin/acl/roles/:roleName", s.requirePlatformAdmin(), s.updateACLRole)
	protected.DELETE("/admin/acl/roles/:roleName", s.requirePlatformAdmin(), s.deleteACLRole)
	protected.POST("/admin/acl/roles/:roleName/permissions/:permissionName", s.requirePlatformAdmin(), s.bindACLRolePermission)
	protected.GET("/admin/acl/role-assignments", s.requirePlatformAdmin(), s.listACLRoleAssignments)
	protected.POST("/admin/acl/role-assignments", s.requirePlatformAdmin(), s.createACLRoleAssignment)
	protected.DELETE("/admin/acl/role-assignments/:assignmentId", s.requirePlatformAdmin(), s.deleteACLRoleAssignment)
	protected.GET("/admin/acl/external-group-mappings", s.requirePlatformAdmin(), s.listACLExternalGroupMappings)
	protected.POST("/admin/acl/external-group-mappings", s.requirePlatformAdmin(), s.createACLExternalGroupMapping)
	protected.DELETE("/admin/acl/external-group-mappings/:mappingId", s.requirePlatformAdmin(), s.deleteACLExternalGroupMapping)
	protected.GET("/admin/acl/audit-events", s.requirePlatformAdmin(), s.listACLAuditEvents)

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
	Email     string `json:"email" binding:"required,email"`
	Password  string `json:"password" binding:"required"`
	AppCSRPem string `json:"app_csr_pem,omitempty"`
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
	response, err := s.loginResponse(c.Request.Context(), user, tokens, req.AppCSRPem)
	if err != nil {
		writeAppCertificateError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (s *Server) listOIDCProviders(c *gin.Context) {
	provider, err := s.resolveOIDCProvider(c, "", false)
	if err != nil {
		if errors.Is(err, auth.ErrOIDCDisabled) || errors.Is(err, auth.ErrOIDCProviderNotFound) {
			c.JSON(http.StatusOK, gin.H{"providers": []any{}})
			return
		}
		writeOIDCError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"providers": []gin.H{publicOIDCProvider(provider)}})
}

func (s *Server) startOIDCLogin(c *gin.Context) {
	provider, err := s.resolveOIDCProvider(c, c.Param("providerId"), true)
	if err != nil {
		writeOIDCError(c, err)
		return
	}
	state, err := auth.RandomToken()
	if err != nil {
		writeError(c, http.StatusInternalServerError, "oidc_state_failed", "Could not create OIDC login state")
		return
	}
	nonce, err := auth.RandomToken()
	if err != nil {
		writeError(c, http.StatusInternalServerError, "oidc_state_failed", "Could not create OIDC login state")
		return
	}
	var postLoginRedirect *string
	if value := strings.TrimSpace(c.Query("redirect_uri")); value != "" {
		postLoginRedirect = &value
	}
	if _, err := s.store.CreateOIDCLoginState(c.Request.Context(), store.OIDCLoginStateCreateInput{
		ProviderID:           provider.ID,
		StateHash:            auth.HashToken(state),
		NonceHash:            auth.HashToken(nonce),
		RedirectURL:          provider.RedirectURL,
		PostLoginRedirectURL: postLoginRedirect,
		ExpiresAt:            s.now().Add(s.oidcStateTTL),
		Now:                  s.now(),
	}); err != nil {
		writeStoreError(c, err)
		return
	}
	location, err := s.oidcClient.AuthorizationURL(c.Request.Context(), provider, state, nonce)
	if err != nil {
		writeOIDCError(c, err)
		return
	}
	c.Redirect(http.StatusFound, location)
}

func (s *Server) handleOIDCCallback(c *gin.Context) {
	if providerErr := strings.TrimSpace(c.Query("error")); providerErr != "" {
		message := strings.TrimSpace(c.Query("error_description"))
		if message == "" {
			message = "OIDC provider returned an error"
		}
		writeError(c, http.StatusBadRequest, "invalid_oidc_token", message)
		return
	}
	code := strings.TrimSpace(c.Query("code"))
	state := strings.TrimSpace(c.Query("state"))
	if code == "" || state == "" {
		writeError(c, http.StatusBadRequest, "invalid_oidc_state", "Invalid OIDC login state")
		return
	}
	provider, err := s.resolveOIDCProvider(c, c.Param("providerId"), true)
	if err != nil {
		writeOIDCError(c, err)
		return
	}
	loginState, err := s.store.ConsumeOIDCLoginState(c.Request.Context(), auth.HashToken(state), s.now())
	if err != nil {
		writeOIDCError(c, err)
		return
	}
	if loginState.ProviderID != provider.ID {
		writeError(c, http.StatusBadRequest, "invalid_oidc_state", "Invalid OIDC login state")
		return
	}
	identity, _, err := s.oidcClient.ExchangeAndValidateNonceHash(c.Request.Context(), provider, code, loginState.NonceHash)
	if err != nil {
		writeOIDCError(c, err)
		return
	}
	user, err := s.resolveOIDCUser(c, provider, identity)
	if err != nil {
		writeOIDCError(c, err)
		return
	}
	if err := s.store.ApplyExternalGroupMappings(c.Request.Context(), user.ID, provider.ProviderID, oidcGroupsFromClaims(identity.Claims), s.now()); err != nil {
		writeStoreError(c, err)
		return
	}
	tokens, err := s.issueTokens(c, user.ID)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "token_issue_failed", "Could not issue tokens")
		return
	}
	response, err := s.loginResponse(c.Request.Context(), user, tokens, "")
	if err != nil {
		writeAppCertificateError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (s *Server) resolveOIDCUser(c *gin.Context, provider auth.OIDCProvider, oidcIdentity auth.OIDCIdentity) (model.User, error) {
	linked, err := s.store.GetUserIdentityByProviderSubject(c.Request.Context(), provider.ID, oidcIdentity.Subject)
	if err == nil {
		user, getErr := s.store.GetUser(c.Request.Context(), linked.UserID)
		if getErr != nil {
			return model.User{}, errOIDCUserNotProvisioned
		}
		if user.SignupPendingVerification {
			return model.User{}, errOIDCUserNotProvisioned
		}
		now := s.now()
		if _, updateErr := s.store.UpdateUserIdentityLastLogin(c.Request.Context(), linked.ID, now); updateErr != nil {
			return model.User{}, updateErr
		}
		return user, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return model.User{}, err
	}
	if !provider.AutoLinkEmail {
		return model.User{}, errOIDCUserNotProvisioned
	}
	user, err := s.store.GetUserByEmail(c.Request.Context(), oidcIdentity.Email)
	if err != nil || user.SignupPendingVerification {
		return model.User{}, errOIDCUserNotProvisioned
	}
	if _, err := s.store.CreateUserIdentity(c.Request.Context(), store.UserIdentityCreateInput{
		UserID:        user.ID,
		ProviderID:    provider.ID,
		IssuerURL:     oidcIdentity.Issuer,
		Subject:       oidcIdentity.Subject,
		Email:         oidcIdentity.Email,
		EmailVerified: oidcIdentity.EmailVerified,
		Claims:        oidcIdentity.Claims,
		Now:           s.now(),
	}); err != nil {
		return model.User{}, err
	}
	return user, nil
}

func oidcGroupsFromClaims(claims map[string]any) []string {
	if len(claims) == 0 {
		return nil
	}
	raw, ok := claims["groups"]
	if !ok {
		raw = claims["group"]
	}
	switch value := raw.(type) {
	case []any:
		groups := make([]string, 0, len(value))
		for _, item := range value {
			if group, ok := item.(string); ok && strings.TrimSpace(group) != "" {
				groups = append(groups, strings.TrimSpace(group))
			}
		}
		return groups
	case []string:
		groups := make([]string, 0, len(value))
		for _, group := range value {
			if strings.TrimSpace(group) != "" {
				groups = append(groups, strings.TrimSpace(group))
			}
		}
		return groups
	case string:
		if strings.TrimSpace(value) == "" {
			return nil
		}
		return []string{strings.TrimSpace(value)}
	default:
		return nil
	}
}

func (s *Server) resolveOIDCProvider(c *gin.Context, providerID string, persistEnvProvider bool) (auth.OIDCProvider, error) {
	if s.store == nil {
		return auth.OIDCProvider{}, auth.ErrOIDCProviderMisconfigured
	}
	provider, err := s.oidcResolver.Resolve(c.Request.Context())
	if err != nil {
		return auth.OIDCProvider{}, err
	}
	if providerID != "" && provider.ProviderID != providerID {
		return auth.OIDCProvider{}, auth.ErrOIDCProviderNotFound
	}
	if provider.ID == "" && persistEnvProvider {
		persisted, err := s.persistEnvOIDCProvider(c, provider)
		if err != nil {
			return auth.OIDCProvider{}, err
		}
		provider.ID = persisted.ID
	}
	if provider.ID == "" && persistEnvProvider {
		return auth.OIDCProvider{}, auth.ErrOIDCProviderMisconfigured
	}
	return provider, nil
}

func (s *Server) persistEnvOIDCProvider(c *gin.Context, provider auth.OIDCProvider) (model.IdentityProvider, error) {
	existing, err := s.store.GetIdentityProviderByProviderID(c.Request.Context(), provider.ProviderID)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return model.IdentityProvider{}, err
	}
	secretRef := s.oidcEnvClientSecretRef
	created, err := s.store.CreateIdentityProvider(c.Request.Context(), store.IdentityProviderCreateInput{
		ProviderID:      provider.ProviderID,
		Name:            provider.Name,
		Type:            model.IdentityProviderTypeOIDC,
		IssuerURL:       provider.IssuerURL,
		ClientID:        provider.ClientID,
		ClientSecretRef: &secretRef,
		Scopes:          provider.Scopes,
		Enabled:         true,
		Metadata:        map[string]any{"source": "env"},
		Now:             s.now(),
	})
	if err == nil {
		return created, nil
	}
	existing, getErr := s.store.GetIdentityProviderByProviderID(c.Request.Context(), provider.ProviderID)
	if getErr == nil {
		return existing, nil
	}
	return model.IdentityProvider{}, err
}

func publicOIDCProvider(provider auth.OIDCProvider) gin.H {
	return gin.H{
		"provider_id": provider.ProviderID,
		"name":        provider.Name,
		"type":        string(model.IdentityProviderTypeOIDC),
		"issuer_url":  provider.IssuerURL,
		"scopes":      provider.Scopes,
		"enabled":     provider.Enabled,
	}
}

var errOIDCUserNotProvisioned = errors.New("oidc user is not provisioned")

func writeOIDCError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, auth.ErrOIDCDisabled):
		writeError(c, http.StatusBadRequest, "oidc_disabled", "OIDC authentication is disabled")
	case errors.Is(err, auth.ErrOIDCProviderNotFound):
		writeError(c, http.StatusNotFound, "oidc_provider_not_found", "OIDC provider not found")
	case errors.Is(err, store.ErrOIDCStateInvalid), errors.Is(err, store.ErrOIDCStateExpired):
		writeError(c, http.StatusBadRequest, "invalid_oidc_state", "Invalid OIDC login state")
	case errors.Is(err, auth.ErrUnverifiedOIDCEmail):
		writeError(c, http.StatusBadRequest, "unverified_oidc_email", "OIDC email is not verified")
	case errors.Is(err, auth.ErrInvalidOIDCToken):
		writeError(c, http.StatusBadRequest, "invalid_oidc_token", "Invalid OIDC token")
	case errors.Is(err, auth.ErrOIDCProviderMisconfigured):
		writeError(c, http.StatusServiceUnavailable, "oidc_provider_misconfigured", "OIDC provider is misconfigured")
	case errors.Is(err, errOIDCUserNotProvisioned):
		writeError(c, http.StatusForbidden, "user_not_provisioned", "SSO user is not provisioned in account manager")
	default:
		writeStoreError(c, err)
	}
}

func (s *Server) now() time.Time {
	if s.oidcClient.Now != nil {
		return s.oidcClient.Now()
	}
	return time.Now().UTC()
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
	if claims.SubjectType != auth.SubjectTypePlatformUser || claims.UserID == "" {
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

func (s *Server) listCurrentUserIdentities(c *gin.Context) {
	identities, err := s.store.ListUserIdentities(c.Request.Context(), currentUserID(c))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"identities": identities})
}

func (s *Server) deleteCurrentUserIdentity(c *gin.Context) {
	if err := s.store.DeleteUserIdentity(c.Request.Context(), currentUserID(c), c.Param("identityId")); err != nil {
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
			switch claims.SubjectType {
			case "", auth.SubjectTypePlatformUser:
				if _, err := s.store.GetUser(c.Request.Context(), claims.UserID); err != nil {
					writeError(c, http.StatusUnauthorized, "invalid_token", "Invalid bearer token")
					c.Abort()
					return
				}
			case auth.SubjectTypeBrandCloudUser:
				if _, err := s.store.GetBrandCloudUser(c.Request.Context(), claims.BrandCloudUserID); err != nil {
					writeError(c, http.StatusUnauthorized, "invalid_token", "Invalid bearer token")
					c.Abort()
					return
				}
			default:
				writeError(c, http.StatusUnauthorized, "invalid_token", "Invalid bearer token")
				c.Abort()
				return
			}
		}
		c.Set("subjectType", claims.SubjectType)
		c.Set("userID", claims.UserID)
		c.Set("brandCloudUserID", claims.BrandCloudUserID)
		c.Set("brandCloudID", claims.BrandCloudID)
		c.Set("tenantSlug", claims.TenantSlug)
		c.Next()
	}
}

func (s *Server) requirePermission(permission string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if currentSubjectType(c) == auth.SubjectTypeBrandCloudUser {
			orgID := c.Param("orgId")
			if orgID == "" || orgID != currentBrandCloudID(c) {
				writeError(c, http.StatusNotFound, "not_found", "Resource not found")
				c.Abort()
				return
			}
			allowed, err := s.store.HasBrandCloudPermission(c.Request.Context(), currentBrandCloudUserID(c), orgID, permission)
			if err != nil {
				writeError(c, http.StatusNotFound, "not_found", "Resource not found")
				c.Abort()
				return
			}
			if !allowed {
				writeError(c, http.StatusForbidden, "forbidden", "Insufficient permissions")
				c.Abort()
				return
			}
			c.Set("permission", permission)
			c.Next()
			return
		}
		if orgID := c.Param("orgId"); orgID != "" {
			if _, err := s.store.GetRole(c.Request.Context(), orgID, currentUserID(c)); err != nil {
				writeError(c, http.StatusNotFound, "not_found", "Resource not found")
				c.Abort()
				return
			}
		}
		allowed, err := s.store.HasPermission(c.Request.Context(), currentUserID(c), c.Param("orgId"), permission)
		if err != nil {
			writeError(c, http.StatusNotFound, "not_found", "Resource not found")
			c.Abort()
			return
		}
		if !allowed {
			writeError(c, http.StatusForbidden, "forbidden", "Insufficient permissions")
			c.Abort()
			return
		}
		c.Set("permission", permission)
		c.Next()
	}
}

func currentUserID(c *gin.Context) string {
	value, _ := c.Get("userID")
	userID, _ := value.(string)
	return userID
}

func currentSubjectType(c *gin.Context) auth.SubjectType {
	value, _ := c.Get("subjectType")
	subjectType, _ := value.(auth.SubjectType)
	if subjectType == "" {
		return auth.SubjectTypePlatformUser
	}
	return subjectType
}

func currentBrandCloudUserID(c *gin.Context) string {
	value, _ := c.Get("brandCloudUserID")
	id, _ := value.(string)
	return id
}

func currentBrandCloudID(c *gin.Context) string {
	value, _ := c.Get("brandCloudID")
	id, _ := value.(string)
	return id
}

func currentTenantSlug(c *gin.Context) string {
	value, _ := c.Get("tenantSlug")
	slug, _ := value.(string)
	return slug
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
	case errors.Is(err, store.ErrClaimUnsupportedCategory):
		writeError(c, http.StatusBadRequest, "unsupported_device_category", "Device category is not supported")
	case errors.Is(err, store.ErrClaimUnsupportedService):
		writeError(c, http.StatusBadRequest, "unsupported_service_option", "service_options may contain only mqtt, video_streaming, or video_storage")
	case errors.Is(err, store.ErrClaimServiceOptionsMismatch):
		writeError(c, http.StatusBadRequest, "service_options_mismatch", "service_options must match the selected device item profile")
	case errors.Is(err, store.ErrDeviceItemProfileDisabled):
		writeError(c, http.StatusConflict, "device_item_profile_disabled", "Device item profile is disabled")
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

func writeClaimResolveError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeClaimError(c, http.StatusNotFound, "invalid_claim_token", "Invalid claim token", false, "scan_or_enter_a_valid_claim_token")
	case errors.Is(err, store.ErrClaimExpired):
		writeClaimError(c, http.StatusBadRequest, "expired_claim_token", "Claim token has expired", false, "request_new_claim_token")
	case errors.Is(err, store.ErrClaimAlreadyClaimed):
		writeClaimError(c, http.StatusConflict, "already_claimed", "Claim token has already been claimed", false, "use_existing_device_or_contact_support")
	case errors.Is(err, store.ErrClaimRevoked):
		writeClaimError(c, http.StatusNotFound, "invalid_claim_token", "Invalid claim token", false, "scan_or_enter_a_valid_claim_token")
	case errors.Is(err, store.ErrClaimCrossOrganization):
		writeClaimError(c, http.StatusForbidden, "forbidden", "Claim token does not belong to this organization", false, "switch_organization_or_contact_support")
	case errors.Is(err, store.ErrClaimUnsupportedCategory):
		writeClaimError(c, http.StatusBadRequest, "unsupported_device_category", "Claim token device category is not supported", false, "use_supported_device_category")
	case errors.Is(err, store.ErrClaimInvalidState):
		writeClaimError(c, http.StatusConflict, "invalid_claim_state", "Claim token state does not allow this operation", false, "contact_support")
	case errors.Is(err, store.ErrClaimEvidenceRequired):
		writeClaimError(c, http.StatusBadRequest, "operator_evidence_required", "Operator reason and evidence are required", false, "provide_operator_evidence")
	case errors.Is(err, store.ErrEvaluationQuotaExceeded):
		writeClaimError(c, http.StatusConflict, "EVALUATION_QUOTA_EXCEEDED", "Evaluation device quota exceeded", false, "request_quota_raise_or_contact_admin")
	case strings.Contains(err.Error(), "duplicate key"):
		writeClaimError(c, http.StatusConflict, "conflict", "Resource already exists", false, "retry_with_current_claim_state")
	default:
		writeClaimError(c, http.StatusServiceUnavailable, "service_unavailable", "Claim resolve is temporarily unavailable", true, "retry_later")
	}
}

func writeClaimError(c *gin.Context, status int, code, message string, retryable bool, resolutionAction string) {
	writeErrorWithFields(c, status, code, message, &retryable, &resolutionAction)
}

func writeAuthTokenStoreError(c *gin.Context, err error, message string) {
	if errors.Is(err, store.ErrRateLimited) {
		writeError(c, http.StatusTooManyRequests, "rate_limited", "Too many token requests")
		return
	}
	writeError(c, http.StatusInternalServerError, "token_issue_failed", message)
}

func writeError(c *gin.Context, status int, code, message string) {
	writeErrorWithFields(c, status, code, message, nil, nil)
}

func writeErrorWithFields(c *gin.Context, status int, code, message string, retryable *bool, resolutionAction *string) {
	errBody := gin.H{"code": code, "message": message}
	if retryable != nil {
		errBody["retryable"] = *retryable
	}
	if resolutionAction != nil && strings.TrimSpace(*resolutionAction) != "" {
		errBody["resolution_action"] = strings.TrimSpace(*resolutionAction)
	}
	c.JSON(status, gin.H{"error": errBody})
}
