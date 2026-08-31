package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"rtk_account_manager/internal/store"
)

type factoryEnrollmentPersistence interface {
	ReserveFactoryEnrollment(context.Context, store.FactoryEnrollmentAdmission) (store.FactoryEnrollmentReservation, error)
	LookupFactoryEnrollment(context.Context, store.FactoryEnrollmentAdmission) (store.FactoryEnrollmentReservation, error)
	CompleteFactoryEnrollment(context.Context, store.FactoryEnrollmentResult) (store.FactoryEnrollmentReservation, error)
}

type factoryEnrollmentScope struct {
	RunID         string `json:"production_run_id" binding:"required"`
	CloudID       string `json:"brand_cloud_id" binding:"required"`
	ProductID     string `json:"device_item_profile_id" binding:"required"`
	RequestID     string `json:"request_id" binding:"required"`
	DeviceID      string `json:"devid" binding:"required"`
	RequestSHA256 string `json:"request_sha256" binding:"required"`
}

func (in factoryEnrollmentScope) admission() store.FactoryEnrollmentAdmission {
	return store.FactoryEnrollmentAdmission{RunID: in.RunID, CloudID: in.CloudID, ProductID: in.ProductID, RequestID: in.RequestID, DeviceID: in.DeviceID, RequestSHA256: in.RequestSHA256}
}

type factoryEnrollmentResponse struct {
	factoryEnrollmentScope
	ReservationID  string  `json:"reservation_id"`
	Status         string  `json:"status"`
	EvidenceSHA256 *string `json:"evidence_sha256,omitempty"`
}

func (s *Server) factoryEnrollmentStore(c *gin.Context) (factoryEnrollmentPersistence, bool) {
	c.Header("Cache-Control", "no-store")
	if s.store == nil || s.factoryEnrollmentToken == "" {
		writeError(c, http.StatusServiceUnavailable, "factory_enrollment_unavailable", "factory enrollment coordination is unavailable")
		return nil, false
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(c.GetHeader("Authorization"))), []byte("Bearer "+s.factoryEnrollmentToken)) != 1 {
		writeError(c, http.StatusUnauthorized, "unauthorized", "Unauthorized")
		return nil, false
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 32<<10)
	return s.store, true
}

func factoryEnrollmentReply(c *gin.Context, in factoryEnrollmentScope, r store.FactoryEnrollmentReservation) {
	c.JSON(http.StatusOK, factoryEnrollmentResponse{factoryEnrollmentScope: in, ReservationID: r.ID, Status: r.Status, EvidenceSHA256: r.EvidenceSHA256})
}

func (s *Server) reserveFactoryEnrollment(c *gin.Context) {
	p, ok := s.factoryEnrollmentStore(c)
	if !ok {
		return
	}
	if s.productionJWTSecret == "" || s.productionJWTAudience == "" {
		writeError(c, http.StatusServiceUnavailable, "factory_enrollment_unavailable", "production authorization verification is unavailable")
		return
	}
	var req struct {
		factoryEnrollmentScope
		ProductionJWT string `json:"production_jwt" binding:"required"`
	}
	if !bindStrict(c, &req) {
		return
	}
	var claims productionJWTClaims
	_, err := jwt.ParseWithClaims(req.ProductionJWT, &claims, func(*jwt.Token) (any, error) { return []byte(s.productionJWTSecret), nil },
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithAudience(s.productionJWTAudience), jwt.WithExpirationRequired())
	if s.productionJWTSecret == "" || err != nil || claims.ID == "" || claims.NotBefore == nil || claims.AllowedQuantity <= 0 ||
		claims.Subject != "factory_production_run:"+claims.ProductionRunID || claims.ProductionRunID != req.RunID || claims.BrandCloudID != req.CloudID || claims.DeviceItemProfileID != req.ProductID {
		writeError(c, http.StatusUnauthorized, "production_jwt_invalid", "production authorization is invalid")
		return
	}
	r, err := p.ReserveFactoryEnrollment(c.Request.Context(), req.admission())
	if err != nil {
		factoryEnrollmentError(c, err)
		return
	}
	factoryEnrollmentReply(c, req.factoryEnrollmentScope, r)
}

func (s *Server) lookupFactoryEnrollment(c *gin.Context) {
	p, ok := s.factoryEnrollmentStore(c)
	if !ok {
		return
	}
	var req factoryEnrollmentScope
	if !bindStrict(c, &req) {
		return
	}
	r, err := p.LookupFactoryEnrollment(c.Request.Context(), req.admission())
	if err != nil {
		factoryEnrollmentError(c, err)
		return
	}
	factoryEnrollmentReply(c, req, r)
}

func (s *Server) completeFactoryEnrollment(c *gin.Context) {
	p, ok := s.factoryEnrollmentStore(c)
	if !ok {
		return
	}
	var req struct {
		factoryEnrollmentScope
		Status         string `json:"status" binding:"required"`
		EvidenceSHA256 string `json:"evidence_sha256" binding:"required"`
	}
	if !bindStrict(c, &req) {
		return
	}
	r, err := p.LookupFactoryEnrollment(c.Request.Context(), req.admission())
	if err != nil {
		factoryEnrollmentError(c, err)
		return
	}
	if r.ID != c.Param("reservationId") {
		factoryEnrollmentError(c, store.ErrNotFound)
		return
	}
	r, err = p.CompleteFactoryEnrollment(c.Request.Context(), store.FactoryEnrollmentResult{
		CloudID: req.CloudID, RunID: req.RunID, ReservationID: r.ID, RequestSHA256: req.RequestSHA256, Status: req.Status, EvidenceSHA256: req.EvidenceSHA256})
	if err != nil {
		factoryEnrollmentError(c, err)
		return
	}
	factoryEnrollmentReply(c, req.factoryEnrollmentScope, r)
}

func factoryEnrollmentError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrProductionRunCapacity):
		writeError(c, http.StatusConflict, "production_run_capacity_exhausted", "production run capacity is exhausted")
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrDeviceItemProfileDisabled):
		writeError(c, http.StatusNotFound, "factory_enrollment_not_found", "factory enrollment is unavailable for this scope")
	case errors.Is(err, store.ErrConflict):
		writeError(c, http.StatusConflict, "factory_enrollment_conflict", "factory enrollment conflicts with existing state")
	default:
		writeError(c, http.StatusServiceUnavailable, "factory_enrollment_unavailable", "factory enrollment coordination is unavailable")
	}
}
