package api

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

type productionRunRequest struct {
	FactoryID       string    `json:"factory_id,omitempty"`
	BatchID         string    `json:"batch_id,omitempty"`
	AllowedQuantity int       `json:"allowed_quantity" binding:"required"`
	ValidFrom       time.Time `json:"valid_from" binding:"required"`
	ValidUntil      time.Time `json:"valid_until" binding:"required"`
}

type productionRunResponse struct {
	ProductionRun model.ProductionRun `json:"production_run"`
	FactoryJWT    string              `json:"factory_jwt"`
	TokenType     string              `json:"token_type"`
	ExpiresAt     time.Time           `json:"expires_at"`
	Audience      string              `json:"audience"`
}

type productionJWTClaims struct {
	ProductionRunID     string `json:"production_run_id"`
	BrandCloudID        string `json:"brand_cloud_id"`
	DeviceItemProfileID string `json:"device_item_profile_id"`
	ProfileKey          string `json:"profile_key,omitempty"`
	FactoryID           string `json:"factory_id,omitempty"`
	BatchID             string `json:"batch_id,omitempty"`
	AllowedQuantity     int    `json:"allowed_quantity"`
	jwt.RegisteredClaims
}

func (s *Server) createProductionRun(c *gin.Context) {
	if strings.TrimSpace(s.productionJWTSecret) == "" {
		writeError(c, http.StatusServiceUnavailable, "production_jwt_signer_unavailable", "production JWT signer is not configured")
		return
	}
	var req productionRunRequest
	if !bindStrict(c, &req) {
		return
	}
	if req.AllowedQuantity <= 0 {
		writeError(c, http.StatusBadRequest, "invalid_allowed_quantity", "allowed_quantity must be positive")
		return
	}
	if req.ValidFrom.IsZero() || req.ValidUntil.IsZero() || !req.ValidUntil.After(req.ValidFrom) {
		writeError(c, http.StatusBadRequest, "invalid_production_period", "valid_until must be after valid_from")
		return
	}

	brandCloudID := c.Param("brandCloudId")
	profileID := c.Param("profileId")
	run, err := s.store.CreateProductionRun(c.Request.Context(), store.ProductionRunCreateInput{
		ActorUserID:         stringPtr(currentUserID(c)),
		BrandCloudID:        brandCloudID,
		DeviceItemProfileID: profileID,
		FactoryID:           req.FactoryID,
		BatchID:             req.BatchID,
		AllowedQuantity:     req.AllowedQuantity,
		ValidFrom:           req.ValidFrom,
		ValidUntil:          req.ValidUntil,
	})
	if err != nil {
		writeStoreError(c, err)
		return
	}
	profile, err := s.store.GetDeviceItemProfile(c.Request.Context(), brandCloudID, profileID)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	token, err := s.signProductionJWT(run, profile)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "production_jwt_sign_failed", "failed to sign production JWT")
		return
	}
	c.JSON(http.StatusCreated, productionRunResponse{
		ProductionRun: run,
		FactoryJWT:    token,
		TokenType:     "Bearer",
		ExpiresAt:     run.ValidUntil,
		Audience:      s.productionJWTAudience,
	})
}

func (s *Server) signProductionJWT(run model.ProductionRun, profile model.DeviceItemProfile) (string, error) {
	tokenID, err := randomJWTID()
	if err != nil {
		return "", err
	}
	claims := productionJWTClaims{
		ProductionRunID:     run.ID,
		BrandCloudID:        run.BrandCloudID,
		DeviceItemProfileID: run.DeviceItemProfileID,
		ProfileKey:          profile.ProfileKey,
		FactoryID:           run.FactoryID,
		BatchID:             run.BatchID,
		AllowedQuantity:     run.AllowedQuantity,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "factory_production_run:" + run.ID,
			Audience:  jwt.ClaimStrings{s.productionJWTAudience},
			NotBefore: jwt.NewNumericDate(run.ValidFrom),
			ExpiresAt: jwt.NewNumericDate(run.ValidUntil),
			IssuedAt:  jwt.NewNumericDate(time.Now().UTC()),
			ID:        tokenID,
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(s.productionJWTSecret))
}

func randomJWTID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(data[:]), nil
}
