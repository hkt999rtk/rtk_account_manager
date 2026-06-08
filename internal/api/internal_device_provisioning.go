package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/channel"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

type internalDeviceProvisioningResultRequest struct {
	OperationID     string     `json:"operation_id" binding:"required"`
	MessageID       string     `json:"message_id"`
	CorrelationID   string     `json:"correlation_id"`
	OrgID           string     `json:"org_id" binding:"required"`
	AccountDeviceID string     `json:"account_device_id" binding:"required"`
	VideoCloudDevid string     `json:"video_cloud_devid" binding:"required"`
	ActivityID      string     `json:"activity_id" binding:"required"`
	ActivatedAt     *time.Time `json:"activated_at"`
}

func (s *Server) handleInternalDeviceProvisioningResult(c *gin.Context) {
	if !s.requireInternalAuthToken(c) {
		return
	}
	var req internalDeviceProvisioningResultRequest
	if !bind(c, &req) {
		return
	}
	req.OperationID = strings.TrimSpace(req.OperationID)
	req.OrgID = strings.TrimSpace(req.OrgID)
	req.AccountDeviceID = strings.TrimSpace(req.AccountDeviceID)
	req.VideoCloudDevid = strings.TrimSpace(req.VideoCloudDevid)
	req.ActivityID = strings.TrimSpace(req.ActivityID)
	req.MessageID = strings.TrimSpace(req.MessageID)
	req.CorrelationID = strings.TrimSpace(req.CorrelationID)

	operation, err := s.store.GetDeviceOperation(c.Request.Context(), req.OperationID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(c, http.StatusNotFound, "operation_not_found", "Provisioning operation was not found")
			return
		}
		writeStoreError(c, err)
		return
	}
	if operation.OrganizationID != req.OrgID || operation.DeviceID != req.AccountDeviceID || operation.OperationType != model.DeviceOperationTypeProvision {
		writeError(c, http.StatusConflict, "operation_mismatch", "Provisioning result does not match operation")
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = operation.CorrelationID
	}
	if req.MessageID == "" {
		req.MessageID = "video-provision-succeeded-" + req.OperationID
	}
	activatedAt := time.Now().UTC()
	if req.ActivatedAt != nil {
		activatedAt = req.ActivatedAt.UTC()
	}

	payload := channel.DeviceProvisionSucceededPayload{
		OrgID:           req.OrgID,
		AccountDeviceID: req.AccountDeviceID,
		VideoCloudDevid: req.VideoCloudDevid,
		ActivityID:      req.ActivityID,
		ActivatedAt:     activatedAt,
	}
	if err := payload.Validate(); err != nil {
		writeError(c, http.StatusBadRequest, "invalid_provisioning_result", err.Error())
		return
	}

	if _, _, err := s.store.CreateOrGetInboxMessage(c.Request.Context(), store.DeviceMessageInboxCreateInput{
		MessageID:     req.MessageID,
		OperationID:   req.OperationID,
		CorrelationID: req.CorrelationID,
		Stream:        string(channel.StreamVideoAccountEvents),
		MessageType:   string(channel.MessageTypeDeviceProvisionSucceeded),
		SchemaVersion: "1.0",
		PartitionKey:  req.AccountDeviceID,
		Payload: map[string]any{
			"org_id":            payload.OrgID,
			"account_device_id": payload.AccountDeviceID,
			"video_cloud_devid": payload.VideoCloudDevid,
			"activity_id":       payload.ActivityID,
			"activated_at":      payload.ActivatedAt,
		},
		Status:     model.DeviceMessageInboxStatusRetrying,
		ReceivedAt: time.Now().UTC(),
	}); err != nil {
		writeStoreError(c, err)
		return
	}

	processedAt := time.Now().UTC()
	status := model.DeviceOperationStatusSucceeded
	projection := store.ProvisionSucceededProjection(payload)
	result, err := s.store.RecordInboxProcessTransition(c.Request.Context(), store.InboxProcessTransitionInput{
		MessageID:            req.MessageID,
		MessageStatus:        model.DeviceMessageInboxStatusProcessed,
		AttemptCount:         1,
		ProcessedAt:          &processedAt,
		OperationStatus:      &status,
		OperationResult:      map[string]any{"video_cloud_devid": payload.VideoCloudDevid, "activity_id": payload.ActivityID, "activated_at": payload.ActivatedAt},
		OperationCompletedAt: &activatedAt,
		OrganizationID:       payload.OrgID,
		DeviceID:             payload.AccountDeviceID,
		Projection:           &projection,
	})
	if err != nil {
		writeStoreError(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status":            "ok",
		"operation_id":      req.OperationID,
		"message_id":        result.Message.MessageID,
		"org_id":            payload.OrgID,
		"account_device_id": payload.AccountDeviceID,
		"video_cloud_devid": payload.VideoCloudDevid,
	})
}
