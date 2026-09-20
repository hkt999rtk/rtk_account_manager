package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"rtk_account_manager/internal/store"
)

type platformServicePersistence interface {
	ApprovePlatformServiceWorkload(context.Context, store.PlatformServiceWorkloadApproval) error
	RevokePlatformServiceWorkload(context.Context, store.PlatformServiceWorkloadRevocation, time.Time) error
	SetPlatformServiceStatus(context.Context, string, string, string, string) (int64, error)
	RegisterPlatformServiceInstance(context.Context, store.PlatformServiceRegistration, store.PlatformServicePrincipal, time.Time) (store.PlatformServiceLease, error)
	HeartbeatPlatformServiceInstance(context.Context, string, string, string, bool, store.PlatformServicePrincipal, time.Time) (store.PlatformServiceLease, error)
	DeregisterPlatformServiceInstance(context.Context, string, string, store.PlatformServicePrincipal, time.Time) error
	PublishPlatformServiceManifest(context.Context, string, string, string, store.PlatformServicePrincipal, time.Time) (int64, error)
	ListPlatformServiceOptions(context.Context, string, time.Time) (store.PlatformServiceCatalog, error)
}

func (s *Server) ConfigurePlatformServices(repository platformServicePersistence, environment string) {
	s.platformServices = repository
	s.platformServiceEnvironment = strings.TrimSpace(environment)
}

func (s *Server) ConfigurePlatformServiceProductWrites(enabled bool) {
	s.platformServiceProductWrites = enabled
}

func (s *Server) productServiceOptions(c *gin.Context, raw []string) ([]string, bool) {
	if !s.platformServiceProductWrites {
		return canonicalServiceOptions(c, raw)
	}
	options := make([]string, len(raw))
	for i, code := range raw {
		options[i] = strings.TrimSpace(code)
	}
	if len(options) == 0 || store.ValidateProductServiceOptionCodes(options) != nil {
		writeError(c, 400, "unsupported_service_option", "Select valid registered service options")
		return nil, false
	}
	return options, true
}

// ServiceRouter is served only on the dedicated mTLS listener, not the user API.
func (s *Server) ServiceRouter() *gin.Engine {
	r := gin.New()
	r.Use(s.requestLogger(), s.recoveryLogger())
	r.PUT("/v1/platform/services/:serviceId/instances/:instanceId", s.registerPlatformService)
	r.POST("/v1/platform/services/:serviceId/instances/:instanceId/heartbeat", s.heartbeatPlatformService)
	r.DELETE("/v1/platform/services/:serviceId/instances/:instanceId", s.deregisterPlatformService)
	r.PATCH("/v1/platform/services/:serviceId/publication", s.publishPlatformService)
	return r
}

func (s *Server) servicePrincipal(c *gin.Context) (store.PlatformServicePrincipal, bool) {
	if s.platformServices == nil || s.platformServiceEnvironment == "" {
		writeError(c, http.StatusServiceUnavailable, "service_registry_unavailable", "Service registry is unavailable")
		return store.PlatformServicePrincipal{}, false
	}
	if c.Request.TLS == nil || len(c.Request.TLS.VerifiedChains) == 0 || len(c.Request.TLS.VerifiedChains[0]) < 2 {
		writeError(c, http.StatusUnauthorized, "service_certificate_required", "Verified service certificate is required")
		return store.PlatformServicePrincipal{}, false
	}
	chain := c.Request.TLS.VerifiedChains[0]
	leaf, issuer := chain[0], chain[1]
	if leaf.IsCA || leaf.Subject.CommonName == "" || time.Now().Before(leaf.NotBefore) || !time.Now().Before(leaf.NotAfter) {
		writeError(c, http.StatusUnauthorized, "service_certificate_invalid", "Invalid service certificate")
		return store.PlatformServicePrincipal{}, false
	}
	hash := sha256.Sum256(issuer.Raw)
	return store.PlatformServicePrincipal{Environment: s.platformServiceEnvironment, CertificateSubject: leaf.Subject.CommonName, IssuerFingerprint: hex.EncodeToString(hash[:])}, true
}

func decodePlatformServiceJSON(c *gin.Context, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10))
	decoder.DisallowUnknownFields()
	var extra any
	if decoder.Decode(target) != nil || decoder.Decode(&extra) != io.EOF {
		writeError(c, http.StatusBadRequest, "invalid_service_registration", "Invalid service registration body")
		return false
	}
	return true
}

func platformServiceError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrServiceRegistrationInvalid):
		writeError(c, 400, "invalid_service_registration", err.Error())
	case errors.Is(err, store.ErrServiceRegistrationDenied):
		writeError(c, 403, "service_registration_denied", "Service identity is not authorized")
	case errors.Is(err, store.ErrNotFound):
		writeError(c, 404, "service_instance_not_found", "Service instance does not exist")
	case errors.Is(err, store.ErrConflict):
		writeError(c, 409, "service_registration_conflict", "Service manifest or revision conflicts")
	default:
		writeError(c, 503, "service_registry_unavailable", "Service registry is unavailable")
	}
}

func (s *Server) registerPlatformService(c *gin.Context) {
	principal, ok := s.servicePrincipal(c)
	if !ok {
		return
	}
	var request store.PlatformServiceRegistration
	if !decodePlatformServiceJSON(c, &request) {
		return
	}
	if request.ServiceID != c.Param("serviceId") || request.InstanceID != c.Param("instanceId") {
		writeError(c, 400, "service_identity_mismatch", "Route and manifest identities differ")
		return
	}
	lease, err := s.platformServices.RegisterPlatformServiceInstance(c.Request.Context(), request, principal, time.Now().UTC())
	if err != nil {
		platformServiceError(c, err)
		return
	}
	c.JSON(200, lease)
}

func (s *Server) heartbeatPlatformService(c *gin.Context) {
	principal, ok := s.servicePrincipal(c)
	if !ok {
		return
	}
	var request struct {
		ManifestVersion string `json:"manifest_version"`
		Ready           bool   `json:"ready"`
	}
	if !decodePlatformServiceJSON(c, &request) {
		return
	}
	if request.ManifestVersion == "" {
		writeError(c, 400, "manifest_version_required", "Manifest version is required")
		return
	}
	lease, err := s.platformServices.HeartbeatPlatformServiceInstance(c.Request.Context(), c.Param("serviceId"), c.Param("instanceId"), request.ManifestVersion, request.Ready, principal, time.Now().UTC())
	if err != nil {
		platformServiceError(c, err)
		return
	}
	c.JSON(200, lease)
}

func (s *Server) deregisterPlatformService(c *gin.Context) {
	principal, ok := s.servicePrincipal(c)
	if !ok {
		return
	}
	if err := s.platformServices.DeregisterPlatformServiceInstance(c.Request.Context(), c.Param("serviceId"), c.Param("instanceId"), principal, time.Now().UTC()); err != nil {
		platformServiceError(c, err)
		return
	}
	c.Status(204)
}

func (s *Server) publishPlatformService(c *gin.Context) {
	principal, ok := s.servicePrincipal(c)
	if !ok {
		return
	}
	var request struct {
		ExpectedVersion string `json:"expected_version"`
		ManifestVersion string `json:"manifest_version"`
	}
	if !decodePlatformServiceJSON(c, &request) {
		return
	}
	revision, err := s.platformServices.PublishPlatformServiceManifest(c.Request.Context(), c.Param("serviceId"), request.ExpectedVersion, request.ManifestVersion, principal, time.Now().UTC())
	if err != nil {
		platformServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"service_id": c.Param("serviceId"), "manifest_version": request.ManifestVersion, "catalog_revision": revision})
}

func (s *Server) approvePlatformServiceWorkload(c *gin.Context) {
	if s.platformServices == nil {
		writeError(c, 503, "service_registry_unavailable", "Service registry is unavailable")
		return
	}
	var request store.PlatformServiceWorkloadApproval
	if !decodePlatformServiceJSON(c, &request) {
		return
	}
	request.Environment = s.platformServiceEnvironment
	request.ApprovedBy = currentUserID(c)
	if err := s.platformServices.ApprovePlatformServiceWorkload(c.Request.Context(), request); err != nil {
		platformServiceError(c, err)
		return
	}
	c.Status(204)
}

func (s *Server) revokePlatformServiceWorkload(c *gin.Context) {
	if s.platformServices == nil {
		writeError(c, 503, "service_registry_unavailable", "Service registry is unavailable")
		return
	}
	var request store.PlatformServiceWorkloadRevocation
	if !decodePlatformServiceJSON(c, &request) {
		return
	}
	request.Environment = s.platformServiceEnvironment
	request.RevokedBy = currentUserID(c)
	if err := s.platformServices.RevokePlatformServiceWorkload(c.Request.Context(), request, time.Now().UTC()); err != nil {
		platformServiceError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (s *Server) setPlatformServiceStatus(c *gin.Context) {
	if s.platformServices == nil {
		writeError(c, 503, "service_registry_unavailable", "Service registry is unavailable")
		return
	}
	var request struct {
		Status string `json:"status"`
	}
	if !decodePlatformServiceJSON(c, &request) {
		return
	}
	revision, err := s.platformServices.SetPlatformServiceStatus(c.Request.Context(), s.platformServiceEnvironment, c.Param("serviceId"), request.Status, currentUserID(c))
	if err != nil {
		platformServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"service_id": c.Param("serviceId"), "status": request.Status, "catalog_revision": revision})
}

func (s *Server) listPlatformServiceOptions(c *gin.Context) {
	if s.platformServices == nil {
		writeError(c, 503, "service_registry_unavailable", "Service registry is unavailable")
		return
	}
	brandCloudID := strings.TrimSpace(c.Query("brand_cloud_id"))
	if brandCloudID == "" {
		writeError(c, 400, "brand_cloud_id_required", "Brand Cloud is required")
		return
	}
	_, err := s.store.GetOrganization(c.Request.Context(), brandCloudID, currentUserID(c))
	if err != nil {
		writeError(c, 403, "forbidden", "Brand Cloud access denied")
		return
	}
	catalog, err := s.platformServices.ListPlatformServiceOptions(c.Request.Context(), s.platformServiceEnvironment, time.Now().UTC())
	if err != nil {
		platformServiceError(c, err)
		return
	}
	c.JSON(200, gin.H{
		"catalog_revision":       catalog.CatalogRevision,
		"options":                catalog.Options,
		"product_writes_enabled": s.platformServiceProductWrites,
	})
}
