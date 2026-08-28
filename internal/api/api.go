package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	cloudlogger "github.com/hkt999rtk/rtk_cloud_logger"
	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"

	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/emaildelivery"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

type Server struct {
	store                  Store
	auth                   *auth.Service
	authTokenQueuedHook    func(AuthTokenDelivery)
	signupLimiter          *signupLimiter
	signupPolicy           signupPolicy
	oidcResolver           auth.ProviderResolver
	oidcClient             auth.OIDCClient
	oidcStateTTL           time.Duration
	oidcEnvClientSecretRef string
	appCertificateIssuer   AppCertificateIssuer
	internalAuthToken      string
	productionJWTSecret    string
	productionJWTAudience  string
	logger                 *zap.Logger
	chipsetManifestFetcher ChipsetManifestFetcher
	emailOutboxStore       emailOutboxPersistence
	emailVerificationTTL   time.Duration
	passwordResetTTL       time.Duration
}

type emailOutboxPersistence interface {
	CreateAuthTokenAndEmail(context.Context, string, string, string, time.Time, store.EmailOutboxInput) error
	CreateLoginActivationTokenForEmailAndEmail(context.Context, string, string, time.Time, store.EmailOutboxInput) (bool, error)
	CreatePasswordResetTokenForEmailAndEmail(context.Context, string, string, time.Time, store.EmailOutboxInput) (bool, error)
	CreateEmailVerificationTokenForEmailAndEmail(context.Context, string, string, time.Time, store.EmailOutboxInput) (bool, error)
	CreateBrandCloudLoginActivationTokenForEmailAndEmail(context.Context, string, string, string, time.Time, store.EmailOutboxInput) (bool, error)
	GetEmailOutboxCounts(context.Context, time.Time) (store.EmailOutboxCounts, error)
}

var ErrEmailOutboxUnavailable = errors.New("email outbox unavailable")

func newServer(store Store, authService *auth.Service) *Server {
	return &Server{
		store:                store,
		auth:                 authService,
		signupLimiter:        newSignupLimiter(5, time.Hour),
		signupPolicy:         loadSignupPolicy(),
		logger:               cloudlogger.Nop(),
		emailVerificationTTL: 30 * time.Minute,
		passwordResetTTL:     30 * time.Minute,
	}
}

func New(store Store, authService *auth.Service) *Server {
	return newServer(store, authService)
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

func (s *Server) ConfigureAppCertificateIssuer(issuer AppCertificateIssuer) {
	s.appCertificateIssuer = issuer
}

func (s *Server) ConfigureInternalAuthToken(token string) {
	s.internalAuthToken = strings.TrimSpace(token)
}

func (s *Server) ConfigureProductionJWT(secret, audience string) {
	s.productionJWTSecret = strings.TrimSpace(secret)
	s.productionJWTAudience = strings.TrimSpace(audience)
	if s.productionJWTAudience == "" {
		s.productionJWTAudience = "factory-enroll"
	}
}

func (s *Server) SetLogger(logger *zap.Logger) {
	if logger == nil {
		logger = cloudlogger.Nop()
	}
	s.logger = logger
}

func (s *Server) ConfigureEmailOutbox(repository emailOutboxPersistence) {
	s.emailOutboxStore = repository
}

func (s *Server) requireEmailOutbox(c *gin.Context) bool {
	if s.emailOutboxStore != nil {
		return true
	}
	writeError(c, http.StatusInternalServerError, "email_outbox_unavailable", "Email delivery is unavailable")
	return false
}

func (s *Server) notifyAuthTokenQueued(delivery AuthTokenDelivery) {
	if s.authTokenQueuedHook != nil {
		s.authTokenQueuedHook(delivery)
	}
}

func (s *Server) ConfigureAuthTokenTTLs(emailVerificationTTL, passwordResetTTL time.Duration) {
	if emailVerificationTTL > 0 {
		s.emailVerificationTTL = emailVerificationTTL
	}
	if passwordResetTTL > 0 {
		s.passwordResetTTL = passwordResetTTL
	}
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
	r.GET("/", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"service": "account-manager",
			"status":  "ok",
			"health":  "/v1/health",
		})
	})
	r.GET("/metrics/prometheus", s.prometheusMetrics)

	v1 := r.Group("/v1")
	v1.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	v1.POST("/auth/register", s.register)
	v1.POST("/auth/signup", s.signup)
	v1.POST("/auth/login", s.login)
	v1.POST("/auth/sign-in", s.signIn)
	v1.POST("/auth/login/activate", s.activateLogin)
	v1.POST("/auth/refresh", s.refresh)
	v1.POST("/auth/verify-email", s.verifyEmail)
	v1.POST("/auth/verify-email/status", s.verificationEmailStatus)
	v1.POST("/auth/resend-verification", s.resendVerification)
	v1.POST("/auth/forgot-password", s.forgotPassword)
	v1.POST("/auth/reset-password", s.resetPassword)
	v1.GET("/auth/oidc/providers", s.listOIDCProviders)
	v1.GET("/auth/oidc/:providerId/login", s.startOIDCLogin)
	v1.GET("/auth/oidc/:providerId/callback", s.handleOIDCCallback)
	v1.POST("/brand-clouds/:tenantSlug/auth/login", s.brandCloudLogin)
	v1.POST("/brand-clouds/:tenantSlug/auth/sign-in", s.brandCloudSignIn)
	v1.POST("/brand-clouds/:tenantSlug/auth/login/activate", s.brandCloudActivateLogin)
	v1.POST("/brand-clouds/:tenantSlug/auth/activate", s.brandCloudActivateUser)
	v1.POST("/brand-clouds/:tenantSlug/auth/refresh", s.brandCloudRefresh)
	v1.POST("/app/end-users/auth/login", s.appEndUserLogin)
	v1.POST("/app/end-users/auth/refresh", s.appEndUserRefresh)
	v1.POST("/internal/app-token-authorizations", s.handleInternalAppTokenAuthorization)
	v1.POST("/internal/device-provisioning-results", s.handleInternalDeviceProvisioningResult)

	protected := v1.Group("")
	protected.Use(s.requireAuth())
	protected.POST("/auth/logout", s.logout)
	protected.POST("/brand-clouds/:tenantSlug/auth/logout", s.brandCloudLogout)
	protected.GET("/brand-clouds/:tenantSlug/me", s.brandCloudMe)
	protected.POST("/app/end-users/auth/logout", s.appEndUserLogout)
	protected.GET("/app/end-users/me", s.appEndUserMe)
	protected.POST("/app/devices/claim/resolve", s.appEndUserResolveDeviceClaim)
	protected.GET("/me", s.me)
	protected.DELETE("/me", s.deleteCurrentUser)
	protected.PATCH("/me/password", s.changePassword)
	protected.GET("/me/identities", s.listCurrentUserIdentities)
	protected.DELETE("/me/identities/:identityId", s.deleteCurrentUserIdentity)

	protected.GET("/developer/brand-clouds", s.listDeveloperBrandClouds)
	protected.GET("/developer/brand-clouds/:brandCloudId", s.getDeveloperBrandCloud)
	protected.POST("/developer/brand-clouds", s.createDeveloperBrandCloud)
	protected.GET("/developer/brand-clouds/:brandCloudId/members", s.listDeveloperBrandCloudMembers)
	protected.GET("/developer/brand-clouds/:brandCloudId/members/invitations", s.listDeveloperBrandCloudMemberInvitations)
	protected.POST("/developer/brand-clouds/:brandCloudId/members/invitations", s.inviteDeveloperBrandCloudMember)
	protected.POST("/developer/brand-clouds/:brandCloudId/members/invitations/:invitationId/resend", s.resendDeveloperBrandCloudMemberInvitation)
	protected.POST("/developer/brand-clouds/:brandCloudId/members/invitations/:invitationId/cancel", s.cancelDeveloperBrandCloudMemberInvitation)
	protected.PATCH("/developer/brand-clouds/:brandCloudId/members/:userId/disable", s.disableDeveloperBrandCloudMember)
	protected.PATCH("/developer/brand-clouds/:brandCloudId/members/:userId/enable", s.enableDeveloperBrandCloudMember)
	protected.PATCH("/developer/brand-clouds/:brandCloudId/members/:userId", s.updateDeveloperBrandCloudMember)
	protected.DELETE("/developer/brand-clouds/:brandCloudId/members/:userId", s.removeDeveloperBrandCloudMember)
	protected.POST("/developer/brand-clouds/:brandCloudId/owner-transfer", s.createBrandCloudOwnerTransfer)
	protected.GET("/developer/brand-clouds/:brandCloudId/owner-transfer/:transferId", s.getBrandCloudOwnerTransfer)
	protected.POST("/developer/brand-clouds/:brandCloudId/owner-transfer/:transferId/cancel", s.cancelBrandCloudOwnerTransfer)
	protected.POST("/developer/brand-clouds/:brandCloudId/pki/test-app-certificates", s.issueDeveloperPKITestAppCertificate)
	protected.POST("/developer/brand-cloud-owner-transfers/accept", s.acceptBrandCloudOwnerTransfer)
	protected.POST("/developer/brand-cloud-member-invitations/accept", s.acceptDeveloperBrandCloudMemberInvitation)
	protected.GET("/developer/brand-clouds/:brandCloudId/skus/:skuId/collaborators", s.listSKUCollaborators)
	protected.PATCH("/developer/brand-clouds/:brandCloudId/skus/:skuId/collaborators/:userId", s.updateSKUCollaborator)
	protected.DELETE("/developer/brand-clouds/:brandCloudId/skus/:skuId/collaborators/:userId", s.removeSKUCollaborator)
	protected.GET("/developer/brand-clouds/:brandCloudId/skus/:skuId/collaborator-invitations", s.listSKUCollaboratorInvitations)
	protected.POST("/developer/brand-clouds/:brandCloudId/skus/:skuId/collaborator-invitations", s.inviteSKUCollaborator)
	protected.POST("/developer/brand-clouds/:brandCloudId/skus/:skuId/collaborator-invitations/:invitationId/resend", s.resendSKUCollaboratorInvitation)
	protected.POST("/developer/brand-clouds/:brandCloudId/skus/:skuId/collaborator-invitations/:invitationId/cancel", s.cancelSKUCollaboratorInvitation)
	protected.POST("/developer/sku-collaborator-invitations/accept", s.acceptSKUCollaboratorInvitation)
	protected.POST("/developer/brand-clouds/:brandCloudId/skus/:skuId/owner-transfer", s.transferSKUOwnership)
	protected.GET("/developer/chipsets", s.listDeveloperChipsets)
	protected.GET("/developer/chipsets/:chipsetId", s.getDeveloperChipset)

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
	protected.GET("/orgs/:orgId/tags", s.requirePermission("device_tag.read"), s.listOrganizationTags)
	protected.GET("/orgs/:orgId/access/check", s.checkOrganizationAccess)
	protected.GET("/orgs/:orgId/roles", s.requirePermission("role_assignment.read"), s.listCustomerACLRoles)
	protected.GET("/orgs/:orgId/permissions", s.requirePermission("role_assignment.read"), s.listCustomerACLPermissions)
	protected.GET("/orgs/:orgId/role-assignments", s.requirePermission("role_assignment.read"), s.listCustomerACLAssignments)
	protected.POST("/orgs/:orgId/role-assignments", s.requirePermission("role_assignment.manage"), s.createCustomerACLAssignment)
	protected.DELETE("/orgs/:orgId/role-assignments/:assignmentId", s.requirePermission("role_assignment.manage"), s.deleteCustomerACLAssignment)

	protected.GET("/orgs/:orgId/device-groups", s.requirePermission("device_group.read"), s.listDeviceGroups)
	protected.POST("/orgs/:orgId/device-groups", s.requirePermission("device_group.manage"), s.createDeviceGroup)
	protected.GET("/orgs/:orgId/device-groups/:groupId", s.requirePermission("device_group.read"), s.getDeviceGroup)
	protected.PATCH("/orgs/:orgId/device-groups/:groupId", s.requirePermission("device_group.manage"), s.updateDeviceGroup)
	protected.DELETE("/orgs/:orgId/device-groups/:groupId", s.requirePermission("device_group.manage"), s.deleteDeviceGroup)
	protected.GET("/orgs/:orgId/device-groups/:groupId/devices", s.requirePermission("device_group.read"), s.listDeviceGroupDevices)
	protected.PUT("/orgs/:orgId/device-groups/:groupId/devices/:deviceId", s.requirePermission("device_group.assign"), s.addDeviceToGroup)
	protected.DELETE("/orgs/:orgId/device-groups/:groupId/devices/:deviceId", s.requirePermission("device_group.assign"), s.removeDeviceFromGroup)
	protected.GET("/orgs/:orgId/device-item-profiles", s.requirePermission("registry_device.read"), s.listDeviceItemProfiles)
	protected.GET("/orgs/:orgId/device-item-profiles/:profileId", s.requirePermission("registry_device.read"), s.getDeviceItemProfile)
	protected.POST("/orgs/:orgId/device-item-profiles", s.requirePermission("registry_device.manage"), s.createDeviceItemProfile)
	protected.PATCH("/orgs/:orgId/device-item-profiles/:profileId", s.requirePermission("registry_device.manage"), s.updateDeviceItemProfile)
	protected.POST("/orgs/:orgId/device-item-profiles/:profileId/disable", s.requirePermission("registry_device.manage"), s.disableDeviceItemProfile)

	protected.POST("/orgs/:orgId/devices", s.requirePermission("registry_device.manage"), s.createDevice)
	protected.GET("/orgs/:orgId/devices", s.requirePermission("registry_device.read"), s.listDevices)
	protected.GET("/orgs/:orgId/fleet/devices", s.requirePermission("registry_device.read"), s.listFleetDevices)
	protected.GET("/orgs/:orgId/fleet/summary", s.requirePermission("registry_device.read"), s.fleetSummary)
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
	protected.POST("/admin/brand-clouds/:brandCloudId/device-item-profiles/:profileId/production-runs", s.requirePlatformAdmin(), s.createProductionRun)
	protected.GET("/orgs/:orgId/device-item-profiles/:profileId/production-runs", s.requirePermission("registry_device.read"), s.listOrganizationProductionRuns)
	protected.POST("/orgs/:orgId/device-item-profiles/:profileId/production-runs", s.requirePermission("registry_device.manage"), s.createProductionRun)
	protected.POST("/admin/brand-clouds/:brandCloudId/members", s.requirePlatformAdmin(), s.assignBrandCloudMember)
	protected.POST("/admin/brand-clouds/:brandCloudId/users", s.requirePlatformAdmin(), s.createBrandCloudUser)
	protected.GET("/admin/brand-clouds/:brandCloudId/users", s.requirePlatformAdmin(), s.listBrandCloudUsers)
	protected.POST("/admin/brand-clouds/:brandCloudId/users/:brandCloudUserId/disable", s.requirePlatformAdmin(), s.disableBrandCloudUser)
	protected.POST("/admin/brand-clouds/:brandCloudId/users/:brandCloudUserId/enable", s.requirePlatformAdmin(), s.enableBrandCloudUser)
	protected.POST("/admin/brand-clouds/:brandCloudId/users/:brandCloudUserId/approve", s.requirePlatformAdmin(), s.approveBrandCloudUser)
	protected.POST("/admin/brand-clouds/:brandCloudId/users/:brandCloudUserId/app-certificate/revoke", s.requirePlatformAdmin(), s.revokeBrandCloudUserAppCertificate)
	protected.DELETE("/admin/brand-clouds/:brandCloudId/users/:brandCloudUserId", s.requirePlatformAdmin(), s.deleteBrandCloudUser)
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
	protected.GET("/admin/chipset-providers", s.requirePermission(permissionChipsetProviderRead), s.listChipsetProviders)
	protected.POST("/admin/chipset-providers", s.requirePermission(permissionChipsetProviderEdit), s.createChipsetProvider)
	protected.GET("/admin/chipset-providers/:providerId", s.requirePermission(permissionChipsetProviderRead), s.getChipsetProvider)
	protected.PATCH("/admin/chipset-providers/:providerId", s.requirePermission(permissionChipsetProviderEdit), s.updateChipsetProvider)
	protected.POST("/admin/chipset-providers/:providerId/:action", s.requirePermission(permissionChipsetProviderPublish), s.actOnChipsetProvider)
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
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if !s.allowSignup(c, email) {
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
		Email:                     email,
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
	if _, _, err := s.issueAuthToken(c, result.User.ID, result.User.Email, "email_verification"); err != nil {
		writeError(c, http.StatusInternalServerError, "email_enqueue_failed", "Could not queue verification email")
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
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			writeStoreError(c, err)
			return
		}
		writeError(c, http.StatusUnauthorized, "invalid_credentials", "Invalid email or password")
		return
	}
	if !auth.CheckPassword(hash, req.Password) || user.SignupPendingVerification {
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

func (s *Server) signIn(c *gin.Context) {
	var req emailRequest
	if !bind(c, &req) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	_, err := s.issueAuthTokenForEmail(c, email, "login_activation")
	if err != nil {
		if enumerationSafeEmailIssueError(err) {
			c.Status(http.StatusAccepted)
			return
		}
		writeAuthTokenStoreError(c, err, "Could not issue login token")
		return
	}
	c.Status(http.StatusAccepted)
}

func (s *Server) activateLogin(c *gin.Context) {
	var req authTokenRequest
	if !bind(c, &req) {
		return
	}
	if !requireNonBlank(c, "token", req.Token) {
		return
	}
	user, err := s.store.ActivateLoginToken(c.Request.Context(), auth.HashToken(req.Token))
	if err != nil {
		writeError(c, http.StatusBadRequest, "invalid_token", "Invalid or expired login token")
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
	Token     string `json:"token" binding:"required"`
	AppCSRPem string `json:"app_csr_pem,omitempty"`
}

type verifyEmailRequest struct {
	Token       string `json:"token"`
	NewPassword string `json:"new_password"`
}

type verificationEmailStatusRequest struct {
	Token string `json:"token"`
}

func (s *Server) verificationEmailStatus(c *gin.Context) {
	var req verificationEmailStatusRequest
	if !bindStrict(c, &req) {
		return
	}
	if !requireNonBlank(c, "token", req.Token) {
		return
	}
	status, err := s.store.EmailVerificationTokenStatus(c.Request.Context(), auth.HashToken(req.Token))
	if err != nil {
		writeError(c, http.StatusInternalServerError, "token_status_failed", "Could not check verification token")
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": status})
}

func (s *Server) verifyEmail(c *gin.Context) {
	var req verifyEmailRequest
	if !bindStrict(c, &req) {
		return
	}
	if !requireNonBlank(c, "token", req.Token) {
		return
	}
	if len(req.NewPassword) < 8 {
		writeError(c, http.StatusBadRequest, "invalid_request", "new_password must be at least 8 characters")
		return
	}
	passwordHash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "password_hash_failed", "Could not hash password")
		return
	}
	user, err := s.store.VerifyEmailToken(c.Request.Context(), auth.HashToken(req.Token), passwordHash)
	if err != nil {
		writeError(c, http.StatusBadRequest, "invalid_token", "Invalid or expired verification token")
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

type emailRequest struct {
	Email string `json:"email" binding:"required,email"`
}

func (s *Server) resendVerification(c *gin.Context) {
	var req emailRequest
	if !bind(c, &req) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	_, err := s.issueAuthTokenForEmail(c, email, "email_verification")
	if err != nil {
		if enumerationSafeEmailIssueError(err) {
			c.Status(http.StatusAccepted)
			return
		}
		writeAuthTokenStoreError(c, err, "Could not issue verification token")
		return
	}
	c.Status(http.StatusAccepted)
}

func (s *Server) forgotPassword(c *gin.Context) {
	var req emailRequest
	if !bind(c, &req) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	_, err := s.issueAuthTokenForEmail(c, email, "password_reset")
	if err != nil {
		if enumerationSafeEmailIssueError(err) {
			c.Status(http.StatusAccepted)
			return
		}
		writeAuthTokenStoreError(c, err, "Could not issue reset token")
		return
	}
	c.Status(http.StatusAccepted)
}

func enumerationSafeEmailIssueError(err error) bool {
	return errors.Is(err, store.ErrRateLimited) ||
		errors.Is(err, ErrEmailOutboxUnavailable) ||
		errors.Is(err, store.ErrEmailOutboxEncryptionUnavailable)
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
	email, err := s.store.ResetPasswordWithToken(c.Request.Context(), auth.HashToken(req.Token), newHash)
	if err != nil {
		writeError(c, http.StatusBadRequest, "invalid_token", "Invalid or expired reset token")
		return
	}
	c.JSON(http.StatusOK, gin.H{"email": email})
}

func (s *Server) issueAuthToken(c *gin.Context, userID, email, purpose string) (string, time.Time, error) {
	if s.emailOutboxStore == nil {
		return "", time.Time{}, ErrEmailOutboxUnavailable
	}
	token, expiresAt, err := s.newAuthToken(purpose)
	if err != nil {
		return "", time.Time{}, err
	}
	err = s.emailOutboxStore.CreateAuthTokenAndEmail(
		c.Request.Context(),
		userID,
		purpose,
		auth.HashToken(token),
		expiresAt,
		authTokenEmailOutbox(email, purpose, token, expiresAt),
	)
	if err != nil {
		return "", time.Time{}, err
	}
	s.notifyAuthTokenQueued(AuthTokenDelivery{Purpose: purpose, Email: email, Token: token, ExpiresAt: expiresAt})
	return token, expiresAt, nil
}

func authTokenEmailOutbox(email, purpose, token string, expiresAt time.Time) store.EmailOutboxInput {
	return store.EmailOutboxInput{
		IdempotencyKey:  "auth-token:" + auth.HashToken(token),
		MessageType:     purpose,
		TemplateVersion: 1,
		Payload: emaildelivery.Payload{
			RecipientEmail: strings.ToLower(strings.TrimSpace(email)),
			Token:          token,
			ExpiresAt:      expiresAt.UTC().Format(time.RFC3339),
		},
		ExpiresAt: &expiresAt,
	}
}

func (s *Server) issueAuthTokenForEmail(c *gin.Context, email, purpose string) (bool, error) {
	if s.emailOutboxStore == nil {
		return false, ErrEmailOutboxUnavailable
	}
	token, expiresAt, err := s.newAuthToken(purpose)
	if err != nil {
		return false, err
	}
	outbox := authTokenEmailOutbox(email, purpose, token, expiresAt)
	var created bool
	switch purpose {
	case "login_activation":
		created, err = s.emailOutboxStore.CreateLoginActivationTokenForEmailAndEmail(c.Request.Context(), email, auth.HashToken(token), expiresAt, outbox)
	case "password_reset":
		created, err = s.emailOutboxStore.CreatePasswordResetTokenForEmailAndEmail(c.Request.Context(), email, auth.HashToken(token), expiresAt, outbox)
	case "email_verification":
		created, err = s.emailOutboxStore.CreateEmailVerificationTokenForEmailAndEmail(c.Request.Context(), email, auth.HashToken(token), expiresAt, outbox)
	default:
		return false, errors.New("unsupported token purpose")
	}
	if err == nil && created {
		s.notifyAuthTokenQueued(AuthTokenDelivery{Purpose: purpose, Email: email, Token: token, ExpiresAt: expiresAt})
	}
	return created, err
}

func (s *Server) issueBrandCloudLoginToken(c *gin.Context, tenantSlug, email string) (bool, error) {
	if s.emailOutboxStore == nil {
		return false, ErrEmailOutboxUnavailable
	}
	token, expiresAt, err := s.newAuthToken("login_activation")
	if err != nil {
		return false, err
	}
	created, err := s.emailOutboxStore.CreateBrandCloudLoginActivationTokenForEmailAndEmail(
		c.Request.Context(), tenantSlug, email, auth.HashToken(token), expiresAt,
		authTokenEmailOutbox(email, "login_activation", token, expiresAt),
	)
	if err == nil && created {
		s.notifyAuthTokenQueued(AuthTokenDelivery{Purpose: "login_activation", Email: email, Token: token, ExpiresAt: expiresAt})
	}
	return created, err
}

func (s *Server) newAuthToken(purpose string) (string, time.Time, error) {
	token, err := auth.RandomToken()
	if err != nil {
		return "", time.Time{}, err
	}
	ttl := 30 * time.Minute
	switch purpose {
	case "email_verification":
		ttl = s.emailVerificationTTL
	case "password_reset":
		ttl = s.passwordResetTTL
	}
	return token, time.Now().UTC().Add(ttl), nil
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
	for i := range orgPage.Organizations {
		if orgPage.Organizations[i].OrganizationKind == model.OrganizationKindBrandCloud {
			orgPage.Organizations[i].Capabilities = s.developerCapabilitiesForUser(c.Request.Context(), userID, orgPage.Organizations[i].ID, orgPage.Organizations[i].Role)
		}
	}
	capabilities, err := s.store.ListUserPlatformPermissions(c.Request.Context(), userID)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"user": user, "organizations": orgPage.Organizations, "capabilities": capabilities})
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
	filter := store.DeviceListFilter{OrganizationID: c.Param("orgId"), Limit: limit, Offset: offset}
	if currentSubjectType(c) == auth.SubjectTypeBrandCloudUser {
		filter.BrandCloudUserID = currentBrandCloudUserID(c)
		filter.ScopePermission = "registry_device.read"
	}
	if currentSubjectType(c) != auth.SubjectTypeBrandCloudUser && !s.currentUserIsPlatformAdmin(c) {
		filter.UserID = currentUserID(c)
		filter.ScopePermission = "registry_device.read"
	}
	devicePage, err := s.store.ListDevicesFiltered(c.Request.Context(), filter)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"devices": devicePage.Devices, "pagination": devicePage.Page})
}

func (s *Server) listFleetDevices(c *gin.Context) {
	limit := queryInt(c, "limit", 100)
	if limit <= 0 {
		limit = 100
	}
	if limit > 250 {
		limit = 250
	}
	filter := store.DeviceListFilter{
		OrganizationID: c.Param("orgId"),
		Query:          c.Query("q"),
		SKU:            c.Query("sku_id"),
		GroupID:        c.Query("group_id"),
		GroupIDs:       splitCSVQuery(c.Query("group_ids")),
		Region:         c.Query("region"),
		Regions:        splitCSVQuery(c.Query("regions")),
		Category:       c.Query("category"),
		Model:          c.Query("model"),
		Status:         c.Query("status"),
		Statuses:       splitCSVQuery(c.Query("statuses")),
		Readiness:      c.Query("readiness"),
		Firmware:       c.Query("firmware"),
		Firmwares:      splitCSVQuery(c.Query("firmwares")),
		Sort:           c.Query("sort"),
		Direction:      c.Query("direction"),
		Limit:          limit,
		Offset:         queryInt(c, "offset", 0),
	}
	if currentSubjectType(c) == auth.SubjectTypeBrandCloudUser {
		filter.BrandCloudUserID = currentBrandCloudUserID(c)
		filter.ScopePermission = "registry_device.read"
	}
	if currentSubjectType(c) != auth.SubjectTypeBrandCloudUser && !s.currentUserIsPlatformAdmin(c) {
		filter.UserID = currentUserID(c)
		filter.ScopePermission = "registry_device.read"
	}
	page, err := s.store.ListDevicesFiltered(c.Request.Context(), filter)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"devices": page.Devices, "pagination": page.Page, "query": gin.H{
		"server_side": true,
		"q":           c.Query("q"), "sku_id": c.Query("sku_id"), "group_id": c.Query("group_id"), "region": c.Query("region"), "category": c.Query("category"), "model": c.Query("model"), "status": c.Query("status"),
	}})
}

func splitCSVQuery(value string) []string {
	parts := strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' })
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return result
}

func (s *Server) fleetSummary(c *gin.Context) {
	var summary store.FleetSummary
	var err error
	if currentSubjectType(c) == auth.SubjectTypeBrandCloudUser {
		summary, err = s.store.FleetSummaryForBrandCloudUser(c.Request.Context(), c.Param("orgId"), currentBrandCloudUserID(c))
	} else if s.currentUserIsPlatformAdmin(c) {
		summary, err = s.store.FleetSummary(c.Request.Context(), c.Param("orgId"))
	} else {
		summary, err = s.store.FleetSummaryForUser(c.Request.Context(), c.Param("orgId"), currentUserID(c))
	}
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, summary)
}

func (s *Server) currentUserIsPlatformAdmin(c *gin.Context) bool {
	if currentSubjectType(c) == auth.SubjectTypeBrandCloudUser {
		return false
	}
	allowed, err := s.store.IsPlatformAdmin(c.Request.Context(), currentUserID(c))
	return err == nil && allowed
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

func (s *Server) listOrganizationTags(c *gin.Context) {
	limit, offset := pagination(c)
	page, err := s.store.ListOrganizationTags(c.Request.Context(), c.Param("orgId"), limit, offset)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"tags": page.Tags, "pagination": page.Page})
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
			case auth.SubjectTypeEndUser:
				if _, err := s.store.GetEndUser(c.Request.Context(), claims.EndUserID); err != nil {
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
		c.Set("endUserID", claims.EndUserID)
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
			if !allowed && (permission == "registry_device.read" || permission == "device_group.read" || permission == "device_tag.read") && c.Param("deviceId") == "" && c.Param("profileId") == "" {
				allowed, err = s.store.HasBrandCloudPermissionAnyResource(c.Request.Context(), currentBrandCloudUserID(c), orgID, permission)
			}
			if !allowed && c.Param("deviceId") == "" && c.Param("profileId") == "" {
				writeError(c, http.StatusForbidden, "forbidden", "Insufficient permissions")
				c.Abort()
				return
			}
			if deviceID := c.Param("deviceId"); deviceID != "" {
				allowed, err = s.store.HasBrandCloudDevicePermission(c.Request.Context(), currentBrandCloudUserID(c), orgID, permission, deviceID)
				if err != nil {
					writeError(c, http.StatusNotFound, "not_found", "Resource not found")
					c.Abort()
					return
				}
				if !allowed {
					canRead, _ := s.store.HasBrandCloudDevicePermission(c.Request.Context(), currentBrandCloudUserID(c), orgID, "registry_device.read", deviceID)
					if canRead {
						writeError(c, http.StatusForbidden, "forbidden", "Insufficient permissions")
					} else {
						writeError(c, http.StatusNotFound, "not_found", "Resource not found")
					}
					c.Abort()
					return
				}
			}
			if profileID := c.Param("profileId"); profileID != "" {
				allowed, err = s.store.HasBrandCloudPermissionForResource(c.Request.Context(), currentBrandCloudUserID(c), orgID, permission, store.ScopeTypeSKU, profileID)
				if err != nil {
					writeError(c, http.StatusNotFound, "not_found", "Resource not found")
					c.Abort()
					return
				}
				if !allowed {
					canRead, _ := s.store.HasBrandCloudPermissionForResource(c.Request.Context(), currentBrandCloudUserID(c), orgID, "registry_device.read", store.ScopeTypeSKU, profileID)
					if canRead {
						writeError(c, http.StatusForbidden, "forbidden", "Insufficient permissions")
					} else {
						writeError(c, http.StatusNotFound, "not_found", "Resource not found")
					}
					c.Abort()
					return
				}
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
		if isAdmin, err := s.store.IsPlatformAdmin(c.Request.Context(), currentUserID(c)); err == nil && isAdmin {
			c.Set("permission", permission)
			c.Next()
			return
		}
		var allowed bool
		var err error
		if deviceID := c.Param("deviceId"); deviceID != "" {
			allowed, err = s.store.HasUserDevicePermission(c.Request.Context(), currentUserID(c), c.Param("orgId"), permission, deviceID)
		} else if profileID := c.Param("profileId"); profileID != "" {
			allowed, err = s.store.HasUserPermissionForResource(c.Request.Context(), currentUserID(c), c.Param("orgId"), permission, store.ScopeTypeSKU, profileID)
		} else {
			allowed, err = s.store.HasPermission(c.Request.Context(), currentUserID(c), c.Param("orgId"), permission)
			if !allowed && (permission == "registry_device.read" || permission == "device_group.read" || permission == "device_tag.read") {
				allowed, err = s.store.HasUserPermissionAnyResource(c.Request.Context(), currentUserID(c), c.Param("orgId"), permission)
			}
		}
		if err != nil {
			writeError(c, http.StatusNotFound, "not_found", "Resource not found")
			c.Abort()
			return
		}
		if !allowed {
			canRead := false
			if deviceID := c.Param("deviceId"); deviceID != "" {
				canRead, _ = s.store.HasUserDevicePermission(c.Request.Context(), currentUserID(c), c.Param("orgId"), "registry_device.read", deviceID)
			} else if profileID := c.Param("profileId"); profileID != "" {
				canRead, _ = s.store.HasUserPermissionForResource(c.Request.Context(), currentUserID(c), c.Param("orgId"), "registry_device.read", store.ScopeTypeSKU, profileID)
			}
			if canRead {
				writeError(c, http.StatusForbidden, "forbidden", "Insufficient permissions")
			} else if c.Param("deviceId") != "" || c.Param("profileId") != "" {
				writeError(c, http.StatusNotFound, "not_found", "Resource not found")
			} else {
				writeError(c, http.StatusForbidden, "forbidden", "Insufficient permissions")
			}
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

func currentEndUserID(c *gin.Context) string {
	value, _ := c.Get("endUserID")
	id, _ := value.(string)
	return id
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
	case errors.Is(err, store.ErrDeveloperCloudLimitExceeded):
		writeError(c, http.StatusConflict, "developer_cloud_limit_exceeded", "Developer brand cloud limit exceeded")
	case errors.Is(err, errOperationStateInconsistent):
		writeError(c, http.StatusInternalServerError, "operation_state_inconsistent", err.Error())
	case isUniqueViolation(err):
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
	case isUniqueViolation(err):
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

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
