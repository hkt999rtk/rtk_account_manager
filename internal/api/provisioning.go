package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/channel"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

const defaultDeactivationReason = "account_device_disabled"

var errOperationStateInconsistent = errors.New("lifecycle operation is missing an outbox message")

type provisionRequest struct {
	VideoCloudDevid string `json:"video_cloud_devid" binding:"required"`
	ActivityID      string `json:"activity_id" binding:"required"`
	ClipPublicKey   string `json:"clip_public_key" binding:"required"`
	OperationID     string `json:"operation_id"`
}

type deactivateRequest struct {
	Reason      string `json:"reason"`
	OperationID string `json:"operation_id"`
}

type operationBody struct {
	Operation operationResponse `json:"operation"`
}

type provisioningBody struct {
	Operation     operationResponse `json:"operation"`
	VideoMetadata map[string]any    `json:"video_metadata"`
}

type operationResponse struct {
	OperationID   string                      `json:"operation_id"`
	CorrelationID string                      `json:"correlation_id"`
	MessageID     string                      `json:"message_id"`
	DeviceID      string                      `json:"device_id"`
	OperationType model.DeviceOperationType   `json:"operation_type"`
	Status        model.DeviceOperationStatus `json:"status"`
	RequestedBy   *string                     `json:"requested_by,omitempty"`
	ErrorCode     *string                     `json:"error_code,omitempty"`
	ErrorMessage  *string                     `json:"error_message,omitempty"`
	Retryable     *bool                       `json:"retryable,omitempty"`
	CreatedAt     time.Time                   `json:"created_at"`
	UpdatedAt     time.Time                   `json:"updated_at"`
	CompletedAt   *time.Time                  `json:"completed_at,omitempty"`
}

func (s *Server) provisionDevice(c *gin.Context) {
	var req provisionRequest
	if !bind(c, &req) {
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

	operationID, ok := ensureOperationID(c, req.OperationID)
	if !ok {
		return
	}
	hasExplicitOperationID := strings.TrimSpace(req.OperationID) != ""

	requestPayload := map[string]any{
		"video_cloud_devid": videoCloudDevid,
		"activity_id":       activityID,
		"clip_public_key":   clipPublicKey,
	}
	if hasExplicitOperationID {
		if _, err := s.store.GetDevice(c.Request.Context(), c.Param("orgId"), c.Param("deviceId")); err != nil {
			writeStoreError(c, err)
			return
		}
		handled, err := s.writeExistingLifecycleOperationIfMatch(c, operationID, func(existing model.DeviceOperation) error {
			return matchExistingProvisionOperation(existing, operationID, c.Param("orgId"), c.Param("deviceId"), requestPayload)
		})
		if err != nil {
			writeStoreError(c, err)
			return
		}
		if handled {
			return
		}
	}

	messageID, err := newOpaqueID()
	if err != nil {
		writeError(c, http.StatusInternalServerError, "id_generation_failed", "Could not generate outbox message id")
		return
	}

	result, err := s.store.StartDeviceLifecycleOperation(c.Request.Context(), store.DeviceLifecycleOperationInput{
		OperationID:       operationID,
		CorrelationID:     operationID,
		MessageID:         messageID,
		OrganizationID:    c.Param("orgId"),
		DeviceID:          c.Param("deviceId"),
		OperationType:     model.DeviceOperationTypeProvision,
		RequestedBy:       stringPtr(currentUserID(c)),
		RequestPayload:    requestPayload,
		OutboxMessageType: string(channel.MessageTypeDeviceProvisionRequested),
		OutboxPayload: map[string]any{
			"org_id":            c.Param("orgId"),
			"account_device_id": c.Param("deviceId"),
			"video_cloud_devid": videoCloudDevid,
			"activity_id":       activityID,
			"clip_public_key":   clipPublicKey,
			"requested_by":      currentUserID(c),
		},
		Now: time.Now().UTC().Truncate(time.Microsecond),
	})
	if err != nil {
		writeStoreError(c, err)
		return
	}

	status := http.StatusCreated
	if !result.Created {
		status = http.StatusOK
	}
	c.JSON(status, operationBody{Operation: operationFromResult(result.Operation, result.Message)})
}

func (s *Server) getProvisioningState(c *gin.Context) {
	device, err := s.store.GetDevice(c.Request.Context(), c.Param("orgId"), c.Param("deviceId"))
	if err != nil {
		writeStoreError(c, err)
		return
	}

	operation, err := s.store.GetLatestDeviceOperationByType(c.Request.Context(), c.Param("orgId"), c.Param("deviceId"), model.DeviceOperationTypeProvision)
	if err != nil {
		writeStoreError(c, err)
		return
	}

	message, err := s.store.GetLatestOutboxMessageByOperationID(c.Request.Context(), operation.OperationID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeStoreError(c, errOperationStateInconsistent)
			return
		}
		writeStoreError(c, err)
		return
	}

	c.JSON(http.StatusOK, provisioningBody{
		Operation:     operationFromResult(operation, message),
		VideoMetadata: projectedVideoMetadata(device.Metadata),
	})
}

func (s *Server) deactivateDevice(c *gin.Context) {
	var req deactivateRequest
	if !bind(c, &req) {
		return
	}

	operationID, ok := ensureOperationID(c, req.OperationID)
	if !ok {
		return
	}
	hasExplicitOperationID := strings.TrimSpace(req.OperationID) != ""

	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		reason = defaultDeactivationReason
	}
	if hasExplicitOperationID {
		handled, err := s.writeExistingDeactivateOperationIfMatch(c, operationID, reason)
		if err != nil {
			writeStoreError(c, err)
			return
		}
		if handled {
			return
		}
	}

	messageID, err := newOpaqueID()
	if err != nil {
		writeError(c, http.StatusInternalServerError, "id_generation_failed", "Could not generate outbox message id")
		return
	}

	result, err := s.store.StartDeviceDeactivationOperation(c.Request.Context(), store.DeviceDeactivationOperationInput{
		OperationID:    operationID,
		CorrelationID:  operationID,
		MessageID:      messageID,
		OrganizationID: c.Param("orgId"),
		DeviceID:       c.Param("deviceId"),
		RequestedBy:    stringPtr(currentUserID(c)),
		Reason:         reason,
		Now:            time.Now().UTC().Truncate(time.Microsecond),
	})
	if err != nil {
		writeStoreError(c, err)
		return
	}

	status := http.StatusCreated
	if !result.Created {
		status = http.StatusOK
	}
	c.JSON(status, operationBody{Operation: operationFromResult(result.Operation, result.Message)})
}

func (s *Server) writeExistingLifecycleOperationIfMatch(c *gin.Context, operationID string, match func(model.DeviceOperation) error) (bool, error) {
	operation, err := s.store.GetDeviceOperation(c.Request.Context(), operationID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := match(operation); err != nil {
		return false, err
	}

	message, err := s.store.GetLatestOutboxMessageByOperationID(c.Request.Context(), operationID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, errOperationStateInconsistent
		}
		return false, err
	}

	c.JSON(http.StatusOK, operationBody{Operation: operationFromResult(operation, message)})
	return true, nil
}

func (s *Server) writeExistingDeactivateOperationIfMatch(c *gin.Context, operationID, reason string) (bool, error) {
	device, err := s.store.GetDevice(c.Request.Context(), c.Param("orgId"), c.Param("deviceId"))
	if err != nil {
		return false, err
	}
	return s.writeExistingLifecycleOperationIfMatch(c, operationID, func(existing model.DeviceOperation) error {
		return matchExistingDeactivateOperation(existing, operationID, c.Param("orgId"), c.Param("deviceId"), reason, device.Metadata)
	})
}

func matchExistingProvisionOperation(existing model.DeviceOperation, operationID, orgID, deviceID string, requestPayload map[string]any) error {
	if err := matchExistingLifecycleOperation(existing, operationID, orgID, deviceID, model.DeviceOperationTypeProvision); err != nil {
		return err
	}
	if !requestPayloadMatches(existing.RequestPayload, requestPayload, "video_cloud_devid", "activity_id", "clip_public_key") {
		return store.ErrConflict
	}
	return nil
}

func matchExistingDeactivateOperation(existing model.DeviceOperation, operationID, orgID, deviceID, reason string, deviceMetadata map[string]any) error {
	if err := matchExistingLifecycleOperation(existing, operationID, orgID, deviceID, model.DeviceOperationTypeDeactivate); err != nil {
		return err
	}
	if !requestPayloadMatches(existing.RequestPayload, map[string]any{"reason": reason}, "reason") {
		return store.ErrConflict
	}

	videoCloudDevid, ok := metadataString(deviceMetadata, model.DeviceMetadataVideoCloudDevid)
	if ok && !requestPayloadMatches(existing.RequestPayload, map[string]any{"video_cloud_devid": videoCloudDevid}, "video_cloud_devid") {
		return store.ErrConflict
	}
	return nil
}

func matchExistingLifecycleOperation(existing model.DeviceOperation, operationID, orgID, deviceID string, operationType model.DeviceOperationType) error {
	if existing.OperationID != operationID ||
		existing.CorrelationID != operationID ||
		existing.OrganizationID != orgID ||
		existing.DeviceID != deviceID ||
		existing.OperationType != operationType {
		return store.ErrConflict
	}
	return nil
}

func requestPayloadMatches(existing, want map[string]any, keys ...string) bool {
	for _, key := range keys {
		existingValue, ok := metadataString(existing, key)
		if !ok {
			return false
		}
		wantValue, ok := metadataString(want, key)
		if !ok || existingValue != wantValue {
			return false
		}
	}
	return true
}

func operationFromResult(operation model.DeviceOperation, message model.DeviceMessageOutbox) operationResponse {
	return operationResponse{
		OperationID:   operation.OperationID,
		CorrelationID: operation.CorrelationID,
		MessageID:     message.MessageID,
		DeviceID:      operation.DeviceID,
		OperationType: operation.OperationType,
		Status:        operation.Status,
		RequestedBy:   operation.RequestedBy,
		ErrorCode:     operation.ErrorCode,
		ErrorMessage:  operation.ErrorMessage,
		Retryable:     operation.Retryable,
		CreatedAt:     operation.CreatedAt,
		UpdatedAt:     operation.UpdatedAt,
		CompletedAt:   operation.CompletedAt,
	}
}

func projectedVideoMetadata(metadata map[string]any) map[string]any {
	projected := map[string]any{}
	for key, value := range metadata {
		if strings.HasPrefix(key, "video_cloud_") {
			projected[key] = value
		}
	}
	return projected
}

func metadataString(metadata map[string]any, key string) (string, bool) {
	value, ok := metadata[key]
	if !ok {
		return "", false
	}
	text, ok := value.(string)
	if !ok {
		return "", false
	}
	text = strings.TrimSpace(text)
	return text, text != ""
}

func ensureOperationID(c *gin.Context, raw string) (string, bool) {
	operationID := strings.TrimSpace(raw)
	if operationID != "" {
		return operationID, true
	}
	generated, err := newOpaqueID()
	if err != nil {
		writeError(c, http.StatusInternalServerError, "id_generation_failed", "Could not generate operation id")
		return "", false
	}
	return generated, true
}

func newOpaqueID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	data[6] = (data[6] & 0x0f) | 0x40
	data[8] = (data[8] & 0x3f) | 0x80
	return hex.EncodeToString(data[0:4]) + "-" +
		hex.EncodeToString(data[4:6]) + "-" +
		hex.EncodeToString(data[6:8]) + "-" +
		hex.EncodeToString(data[8:10]) + "-" +
		hex.EncodeToString(data[10:16]), nil
}

func stringPtr(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
