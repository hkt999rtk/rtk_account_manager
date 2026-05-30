package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

type deviceClaimTokenAdminCreateRequest struct {
	OrganizationID  *string        `json:"organization_id"`
	ClaimToken      *string        `json:"claim_token"`
	Category        string         `json:"category" binding:"required"`
	VideoCloudDevid string         `json:"video_cloud_devid" binding:"required"`
	ActivityID      string         `json:"activity_id" binding:"required"`
	ClipPublicKey   string         `json:"clip_public_key" binding:"required"`
	ServiceOptions  []string       `json:"service_options" binding:"required"`
	ExpiresAt       time.Time      `json:"expires_at" binding:"required"`
	Metadata        map[string]any `json:"metadata"`
	Notes           *string        `json:"notes"`
}

type deviceClaimTokenResponse struct {
	DeviceClaimToken model.DeviceClaimToken `json:"device_claim_token"`
	ClaimToken       *string                `json:"claim_token,omitempty"`
}

type deviceClaimTokensResponse struct {
	DeviceClaimTokens []model.DeviceClaimToken `json:"device_claim_tokens"`
	Pagination        store.Page               `json:"pagination"`
}

type deviceClaimOverrideRequest struct {
	TargetOrganizationID string         `json:"target_organization_id" binding:"required"`
	Reason               string         `json:"reason" binding:"required"`
	Evidence             map[string]any `json:"evidence" binding:"required"`
}

type deviceClaimOverrideResponse struct {
	DeviceClaimToken model.DeviceClaimToken `json:"device_claim_token"`
	DeviceClaim      model.DeviceClaim      `json:"device_claim"`
	Device           model.Device           `json:"device"`
}

func (s *Server) createDeviceClaimToken(c *gin.Context) {
	var req deviceClaimTokenAdminCreateRequest
	if !bindStrict(c, &req) {
		return
	}

	category, ok := parseDeviceClaimTokenCategory(c, req.Category)
	if !ok {
		return
	}
	videoCloudDevid := strings.TrimSpace(req.VideoCloudDevid)
	activityID := strings.TrimSpace(req.ActivityID)
	clipPublicKey := strings.TrimSpace(req.ClipPublicKey)
	if !requireNonBlank(c, "video_cloud_devid", videoCloudDevid) ||
		!requireNonBlank(c, "activity_id", activityID) ||
		!requireNonBlank(c, "clip_public_key", clipPublicKey) {
		return
	}
	if !req.ExpiresAt.After(time.Now().UTC()) {
		writeError(c, http.StatusBadRequest, "invalid_request", "expires_at must be in the future")
		return
	}
	serviceOptions, ok := canonicalServiceOptions(c, req.ServiceOptions)
	if !ok {
		return
	}

	rawToken, generated, ok := adminClaimTokenRawValue(c, req.ClaimToken)
	if !ok {
		return
	}
	var responseRaw *string
	if generated {
		responseRaw = &rawToken
	}

	token, err := s.store.CreateDeviceClaimToken(c.Request.Context(), store.DeviceClaimTokenCreateInput{
		OrganizationID:  trimPtr(req.OrganizationID),
		CreatedBy:       stringPtr(currentUserID(c)),
		TokenHash:       auth.HashToken(rawToken),
		Category:        category,
		VideoCloudDevid: videoCloudDevid,
		ActivityID:      activityID,
		ClipPublicKey:   clipPublicKey,
		ServiceOptions:  serviceOptions,
		Metadata:        req.Metadata,
		Notes:           trimPtr(req.Notes),
		ExpiresAt:       req.ExpiresAt,
		Now:             time.Now().UTC(),
	})
	if err != nil {
		writeStoreError(c, err)
		return
	}

	c.JSON(http.StatusCreated, deviceClaimTokenResponse{DeviceClaimToken: token, ClaimToken: responseRaw})
}

func (s *Server) listDeviceClaimTokens(c *gin.Context) {
	limit, offset := pagination(c)
	page, err := s.store.ListDeviceClaimTokens(c.Request.Context(), store.DeviceClaimTokenListFilter{
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, deviceClaimTokensResponse{DeviceClaimTokens: page.Tokens, Pagination: page.Page})
}

func (s *Server) getDeviceClaimToken(c *gin.Context) {
	token, err := s.store.GetDeviceClaimToken(c.Request.Context(), c.Param("tokenId"))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, deviceClaimTokenResponse{DeviceClaimToken: token})
}

func (s *Server) revokeDeviceClaimToken(c *gin.Context) {
	token, err := s.store.RevokeDeviceClaimToken(c.Request.Context(), c.Param("tokenId"), time.Now().UTC())
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, deviceClaimTokenResponse{DeviceClaimToken: token})
}

func (s *Server) transferDeviceClaim(c *gin.Context) {
	var req deviceClaimOverrideRequest
	if !bindClaimOverrideRequest(c, &req) {
		return
	}
	result, err := s.store.TransferDeviceClaim(c.Request.Context(), store.DeviceClaimTransferInput{
		ClaimID:              c.Param("claimId"),
		TargetOrganizationID: strings.TrimSpace(req.TargetOrganizationID),
		ActorUserID:          currentUserID(c),
		Reason:               strings.TrimSpace(req.Reason),
		Evidence:             req.Evidence,
		Now:                  time.Now().UTC(),
	})
	if err != nil {
		writeClaimResolveError(c, err)
		return
	}
	c.JSON(http.StatusOK, deviceClaimOverrideResponse{DeviceClaimToken: result.Token, DeviceClaim: result.Claim, Device: result.Device})
}

func (s *Server) reclaimDeviceClaimToken(c *gin.Context) {
	var req deviceClaimOverrideRequest
	if !bindClaimOverrideRequest(c, &req) {
		return
	}
	result, err := s.store.ReclaimDeviceClaimToken(c.Request.Context(), store.DeviceClaimReclaimInput{
		TokenID:              c.Param("tokenId"),
		TargetOrganizationID: strings.TrimSpace(req.TargetOrganizationID),
		ActorUserID:          currentUserID(c),
		Reason:               strings.TrimSpace(req.Reason),
		Evidence:             req.Evidence,
		Now:                  time.Now().UTC(),
	})
	if err != nil {
		writeClaimResolveError(c, err)
		return
	}
	c.JSON(http.StatusOK, deviceClaimOverrideResponse{DeviceClaimToken: result.Token, DeviceClaim: result.Claim, Device: result.Device})
}

func bindClaimOverrideRequest(c *gin.Context, req *deviceClaimOverrideRequest) bool {
	if !bindStrict(c, req) {
		return false
	}
	if !requireNonBlank(c, "target_organization_id", strings.TrimSpace(req.TargetOrganizationID)) ||
		!requireNonBlank(c, "reason", strings.TrimSpace(req.Reason)) {
		return false
	}
	if len(req.Evidence) == 0 {
		writeError(c, http.StatusBadRequest, "operator_evidence_required", "evidence must not be empty")
		return false
	}
	return true
}

func parseDeviceClaimTokenCategory(c *gin.Context, raw string) (model.DeviceCategory, bool) {
	switch model.DeviceCategory(strings.TrimSpace(raw)) {
	case model.DeviceCategoryIPCamera:
		return model.DeviceCategoryIPCamera, true
	case model.DeviceCategoryMQTT:
		return model.DeviceCategoryMQTT, true
	case model.DeviceCategoryGeneric:
		return model.DeviceCategoryGeneric, true
	default:
		writeError(c, http.StatusBadRequest, "invalid_request", "category must be ip_camera, mqtt_device, or generic")
		return "", false
	}
}

func adminClaimTokenRawValue(c *gin.Context, raw *string) (string, bool, bool) {
	if raw != nil {
		trimmed := strings.TrimSpace(*raw)
		if trimmed == "" {
			writeError(c, http.StatusBadRequest, "invalid_request", "claim_token must not be blank")
			return "", false, false
		}
		return trimmed, false, true
	}
	first, err := newOpaqueID()
	if err != nil {
		writeError(c, http.StatusInternalServerError, "id_generation_failed", "Could not generate claim token")
		return "", false, false
	}
	second, err := newOpaqueID()
	if err != nil {
		writeError(c, http.StatusInternalServerError, "id_generation_failed", "Could not generate claim token")
		return "", false, false
	}
	return "claim_" + first + second, true, true
}

func canonicalServiceOptions(c *gin.Context, raw []string) ([]string, bool) {
	options, ok := canonicalOptionalServiceOptions(c, raw)
	if !ok {
		return nil, false
	}
	if len(options) == 0 {
		writeError(c, http.StatusBadRequest, "invalid_request", "service_options must include at least one option")
		return nil, false
	}
	return options, true
}

func canonicalOptionalServiceOptions(c *gin.Context, raw []string) ([]string, bool) {
	if len(raw) == 0 {
		return nil, true
	}
	seen := map[string]struct{}{}
	options := make([]string, 0, len(raw))
	for _, option := range raw {
		option = strings.TrimSpace(option)
		switch option {
		case "mqtt", "video_streaming", "video_storage":
		default:
			writeError(c, http.StatusBadRequest, "invalid_request", "service_options may contain only mqtt, video_streaming, or video_storage")
			return nil, false
		}
		if _, exists := seen[option]; exists {
			writeError(c, http.StatusBadRequest, "invalid_request", "service_options must not contain duplicates")
			return nil, false
		}
		seen[option] = struct{}{}
		options = append(options, option)
	}
	return options, true
}
