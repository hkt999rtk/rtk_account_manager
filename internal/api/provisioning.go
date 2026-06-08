package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/channel"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

const defaultDeactivationReason = "account_device_disabled"

var errOperationStateInconsistent = errors.New("lifecycle operation is missing an outbox message")

type provisionRequest struct {
	VideoCloudDevid string         `json:"video_cloud_devid"`
	ActivityID      string         `json:"activity_id"`
	ClipPublicKey   string         `json:"clip_public_key"`
	ServiceOptions  []string       `json:"service_options"`
	OperationID     string         `json:"operation_id"`
	ClaimMaterial   map[string]any `json:"claim_material"`
	QRPayload       *string        `json:"qr_payload"`
	QRCode          *string        `json:"qr_code"`
	SerialNumber    *string        `json:"serial_number"`
	ActivationCode  *string        `json:"activation_code"`
	MACAddress      *string        `json:"mac_address"`
	FactoryIdentity *string        `json:"factory_identity"`
}

type deactivateRequest struct {
	Reason      string `json:"reason"`
	OperationID string `json:"operation_id"`
}

type unprovisionRequest struct {
	Reason   string         `json:"reason"`
	Evidence map[string]any `json:"evidence"`
}

type claimResolveRequest struct {
	ClaimToken string `json:"claim_token"`
	DeviceName string `json:"device_name"`
}

type operationBody struct {
	Operation operationResponse `json:"operation"`
}

type claimResolveResponse struct {
	ClaimID        string                     `json:"claim_id"`
	Device         model.Device               `json:"device"`
	ProvisionInput store.DeviceProvisionInput `json:"provision_input"`
}

type provisioningBody struct {
	Operation     *operationResponse `json:"operation"`
	Readiness     readinessResponse  `json:"readiness"`
	VideoMetadata map[string]any     `json:"video_metadata"`
}

type deviceUnprovisionResponse struct {
	Unprovision deviceUnprovisionBody `json:"unprovision"`
}

type deviceUnprovisionBody struct {
	DeviceID        string    `json:"device_id"`
	OrganizationID  string    `json:"organization_id"`
	VideoCloudDevid string    `json:"video_cloud_devid"`
	Status          string    `json:"status"`
	UnprovisionedAt time.Time `json:"unprovisioned_at"`
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

type readinessResponse struct {
	State        model.DeviceReadinessState  `json:"state"`
	ProductState model.ProductReadinessState `json:"product_state"`
	Sources      readinessSourcesResponse    `json:"sources"`
	Failure      *readinessFailureResponse   `json:"failure,omitempty"`
}

type readinessSourcesResponse struct {
	DeviceEnabled               bool                         `json:"device_enabled"`
	DeviceStatus                model.DeviceStatus           `json:"device_status"`
	ProvisioningOperationStatus *model.DeviceOperationStatus `json:"provisioning_operation_status,omitempty"`
	VideoCloudActivationStatus  *string                      `json:"video_cloud_activation_status,omitempty"`
	DeactivationOperationStatus *model.DeviceOperationStatus `json:"deactivation_operation_status,omitempty"`
	VideoCloudLastError         any                          `json:"video_cloud_last_error,omitempty"`
}

type readinessFailureResponse struct {
	FailedLayer  string     `json:"failed_layer"`
	SourceState  string     `json:"source_state"`
	Retryable    bool       `json:"retryable"`
	ErrorCode    string     `json:"error_code"`
	ErrorMessage string     `json:"error_message"`
	OperationID  *string    `json:"operation_id,omitempty"`
	OccurredAt   *time.Time `json:"occurred_at,omitempty"`
}

func (s *Server) provisionDevice(c *gin.Context) {
	var req provisionRequest
	if !bindStrict(c, &req) {
		return
	}
	if !rejectUnsupportedClaimMaterial(c, req) {
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
	serviceOptions, ok := canonicalOptionalServiceOptions(c, req.ServiceOptions)
	if !ok {
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
	if len(serviceOptions) > 0 {
		requestPayload["service_options"] = serviceOptions
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
			"service_options":   serviceOptions,
			"requested_by":      currentUserID(c),
		},
		MetadataPatch: store.PendingProvisionMetadata(videoCloudDevid, activityID, clipPublicKey, serviceOptions),
		Now:           time.Now().UTC().Truncate(time.Microsecond),
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

func (s *Server) resolveDeviceClaim(c *gin.Context) {
	var req claimResolveRequest
	if !bindStrict(c, &req) {
		return
	}

	claimToken := strings.TrimSpace(req.ClaimToken)
	deviceName := strings.TrimSpace(req.DeviceName)
	if !requireNonBlank(c, "claim_token", claimToken) ||
		!requireNonBlank(c, "device_name", deviceName) {
		return
	}

	result, err := s.store.ResolveDeviceClaimToken(c.Request.Context(), store.DeviceClaimResolveInput{
		TokenHash:      auth.HashToken(claimToken),
		OrganizationID: c.Param("orgId"),
		RequestedBy:    currentUserID(c),
		DeviceName:     deviceName,
		Now:            time.Now().UTC(),
	})
	if err != nil {
		s.logger.Error("device claim resolve failed",
			zap.String("org_id", c.Param("orgId")),
			zap.String("actor_user_id", currentUserID(c)),
			zap.String("brand_cloud_user_id", currentBrandCloudUserID(c)),
			zap.String("subject_type", string(currentSubjectType(c))),
			zap.Error(err),
		)
		writeClaimResolveError(c, err)
		return
	}

	c.JSON(http.StatusCreated, claimResolveResponse{
		ClaimID:        result.Claim.ID,
		Device:         result.Device,
		ProvisionInput: result.ProvisionInput,
	})
}

func rejectUnsupportedClaimMaterial(c *gin.Context, req provisionRequest) bool {
	fields := map[string]bool{
		"claim_material":   req.ClaimMaterial != nil,
		"qr_payload":       req.QRPayload != nil,
		"qr_code":          req.QRCode != nil,
		"serial_number":    req.SerialNumber != nil,
		"activation_code":  req.ActivationCode != nil,
		"mac_address":      req.MACAddress != nil,
		"factory_identity": req.FactoryIdentity != nil,
	}
	for _, field := range []string{"claim_material", "qr_payload", "qr_code", "serial_number", "activation_code", "mac_address", "factory_identity"} {
		if fields[field] {
			writeError(c, http.StatusBadRequest, "unsupported_claim_material", field+" is not accepted by this endpoint")
			return false
		}
	}
	return true
}

func (s *Server) getProvisioningState(c *gin.Context) {
	device, err := s.store.GetDevice(c.Request.Context(), c.Param("orgId"), c.Param("deviceId"))
	if err != nil {
		writeStoreError(c, err)
		return
	}

	operation, err := s.store.GetLatestDeviceOperationByType(c.Request.Context(), c.Param("orgId"), c.Param("deviceId"), model.DeviceOperationTypeProvision)
	var latestProvision *model.DeviceOperation
	var operationResponseValue *operationResponse
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeStoreError(c, err)
		return
	}
	if err == nil {
		latestProvision = &operation
		message, err := s.store.GetLatestOutboxMessageByOperationID(c.Request.Context(), operation.OperationID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeStoreError(c, errOperationStateInconsistent)
				return
			}
			writeStoreError(c, err)
			return
		}
		value := operationFromResult(operation, message)
		operationResponseValue = &value
	}

	deactivationOperation, err := s.store.GetLatestDeviceOperationByType(c.Request.Context(), c.Param("orgId"), c.Param("deviceId"), model.DeviceOperationTypeDeactivate)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeStoreError(c, err)
		return
	}
	var latestDeactivation *model.DeviceOperation
	if err == nil {
		latestDeactivation = &deactivationOperation
	}

	c.JSON(http.StatusOK, provisioningBody{
		Operation:     operationResponseValue,
		Readiness:     readinessFromProjection(device, latestProvision, latestDeactivation),
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

func (s *Server) unprovisionDevice(c *gin.Context) {
	var req unprovisionRequest
	if !bindStrict(c, &req) {
		return
	}
	result, err := s.store.UnprovisionDevice(c.Request.Context(), store.DeviceUnprovisionInput{
		OrganizationID: c.Param("orgId"),
		DeviceID:       c.Param("deviceId"),
		ActorUserID:    currentUserID(c),
		Reason:         req.Reason,
		Now:            time.Now().UTC().Truncate(time.Microsecond),
	})
	if err != nil {
		writeUnprovisionError(c, err)
		return
	}
	c.JSON(http.StatusOK, unprovisionResponseFromResult(result))
}

func (s *Server) adminUnprovisionDevice(c *gin.Context) {
	var req unprovisionRequest
	if !bindStrict(c, &req) {
		return
	}
	result, err := s.store.UnprovisionDevice(c.Request.Context(), store.DeviceUnprovisionInput{
		DeviceID:         c.Param("deviceId"),
		ActorUserID:      currentUserID(c),
		Reason:           req.Reason,
		Evidence:         req.Evidence,
		PlatformOverride: true,
		Now:              time.Now().UTC().Truncate(time.Microsecond),
	})
	if err != nil {
		writeUnprovisionError(c, err)
		return
	}
	c.JSON(http.StatusOK, unprovisionResponseFromResult(result))
}

func writeUnprovisionError(c *gin.Context, err error) {
	if errors.Is(err, store.ErrClaimEvidenceRequired) {
		writeError(c, http.StatusBadRequest, "operator_evidence_required", "Operator reason and evidence are required")
		return
	}
	writeStoreError(c, err)
}

func unprovisionResponseFromResult(result store.DeviceUnprovisionResult) deviceUnprovisionResponse {
	return deviceUnprovisionResponse{Unprovision: deviceUnprovisionBody{
		DeviceID:        result.DeviceID,
		OrganizationID:  result.OrganizationID,
		VideoCloudDevid: result.VideoCloudDevid,
		Status:          "unprovisioned",
		UnprovisionedAt: result.UnprovisionedAt,
	}}
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

func readinessFromProjection(device model.Device, provisioningOperation *model.DeviceOperation, latestDeactivation *model.DeviceOperation) readinessResponse {
	sources := readinessSourcesResponse{
		DeviceEnabled: device.DisabledAt == nil,
		DeviceStatus:  device.Status,
	}
	if provisioningOperation != nil {
		sources.ProvisioningOperationStatus = &provisioningOperation.Status
	}
	if activationStatus, ok := metadataString(device.Metadata, model.DeviceMetadataVideoCloudActivationStatus); ok {
		sources.VideoCloudActivationStatus = &activationStatus
	}
	if lastError, ok := device.Metadata[model.DeviceMetadataVideoCloudLastError]; ok {
		sources.VideoCloudLastError = lastError
	}
	if latestDeactivation != nil {
		sources.DeactivationOperationStatus = &latestDeactivation.Status
	}

	return readinessResponse{
		State:        readinessStateFromSources(sources),
		ProductState: productReadinessStateFromSources(sources),
		Sources:      sources,
		Failure:      readinessFailureFromProjection(device, provisioningOperation, latestDeactivation),
	}
}

func readinessFailureFromProjection(device model.Device, provisioningOperation *model.DeviceOperation, latestDeactivation *model.DeviceOperation) *readinessFailureResponse {
	if latestDeactivation != nil && operationFailed(latestDeactivation.Status) {
		return failureFromOperation("deactivation", *latestDeactivation)
	}
	if provisioningOperation != nil && operationFailed(provisioningOperation.Status) {
		return failureFromOperation("cloud_activation", *provisioningOperation)
	}
	if lastError, ok := device.Metadata[model.DeviceMetadataVideoCloudLastError]; ok {
		return failureFromMetadata(lastError, device.UpdatedAt)
	}
	return nil
}

func operationFailed(status model.DeviceOperationStatus) bool {
	return status == model.DeviceOperationStatusFailed || status == model.DeviceOperationStatusDeadLettered
}

func failureFromOperation(layer string, operation model.DeviceOperation) *readinessFailureResponse {
	retryable := false
	if operation.Retryable != nil {
		retryable = *operation.Retryable
	}
	errorCode := "operation_failed"
	if operation.Status == model.DeviceOperationStatusDeadLettered {
		errorCode = "dead_lettered"
	}
	if operation.ErrorCode != nil && strings.TrimSpace(*operation.ErrorCode) != "" {
		errorCode = strings.TrimSpace(*operation.ErrorCode)
	}
	errorMessage := "Operation failed"
	if operation.ErrorMessage != nil && strings.TrimSpace(*operation.ErrorMessage) != "" {
		errorMessage = strings.TrimSpace(*operation.ErrorMessage)
	}
	occurredAt := operation.CompletedAt
	if occurredAt == nil {
		occurredAt = &operation.UpdatedAt
	}
	return &readinessFailureResponse{
		FailedLayer:  layer,
		SourceState:  string(operation.Status),
		Retryable:    retryable,
		ErrorCode:    errorCode,
		ErrorMessage: errorMessage,
		OperationID:  &operation.OperationID,
		OccurredAt:   occurredAt,
	}
}

func failureFromMetadata(lastError any, updatedAt time.Time) *readinessFailureResponse {
	errorCode := "video_cloud_last_error"
	errorMessage := "Projected video cloud error"
	if values, ok := lastError.(map[string]any); ok {
		if code, ok := metadataString(values, "code"); ok {
			errorCode = code
		}
		if message, ok := metadataString(values, "message"); ok {
			errorMessage = message
		}
	}
	occurredAt := updatedAt
	return &readinessFailureResponse{
		FailedLayer:  "cloud_activation",
		SourceState:  "video_cloud_last_error",
		Retryable:    false,
		ErrorCode:    errorCode,
		ErrorMessage: errorMessage,
		OccurredAt:   &occurredAt,
	}
}

func readinessStateFromSources(sources readinessSourcesResponse) model.DeviceReadinessState {
	if sources.DeactivationOperationStatus != nil {
		switch *sources.DeactivationOperationStatus {
		case model.DeviceOperationStatusPending, model.DeviceOperationStatusPublished, model.DeviceOperationStatusRetrying:
			return model.DeviceReadinessStateDeactivationPending
		case model.DeviceOperationStatusFailed, model.DeviceOperationStatusDeadLettered:
			return model.DeviceReadinessStateDeactivationFailed
		case model.DeviceOperationStatusSucceeded:
			return model.DeviceReadinessStateDeactivated
		}
	}

	if sources.VideoCloudActivationStatus != nil && *sources.VideoCloudActivationStatus == string(model.VideoCloudActivationStatusDeactivated) {
		return model.DeviceReadinessStateDeactivated
	}

	if !sources.DeviceEnabled || sources.DeviceStatus == model.DeviceStatusDisabled {
		return model.DeviceReadinessStateDisabled
	}

	if (sources.ProvisioningOperationStatus != nil &&
		(*sources.ProvisioningOperationStatus == model.DeviceOperationStatusFailed ||
			*sources.ProvisioningOperationStatus == model.DeviceOperationStatusDeadLettered)) ||
		(sources.VideoCloudActivationStatus != nil && *sources.VideoCloudActivationStatus == string(model.VideoCloudActivationStatusFailed)) ||
		sources.VideoCloudLastError != nil {
		return model.DeviceReadinessStateActivationFailed
	}

	if sources.ProvisioningOperationStatus != nil &&
		*sources.ProvisioningOperationStatus == model.DeviceOperationStatusSucceeded &&
		sources.VideoCloudActivationStatus != nil &&
		*sources.VideoCloudActivationStatus == string(model.VideoCloudActivationStatusActivated) {
		if sources.DeviceStatus == model.DeviceStatusOnline {
			return model.DeviceReadinessStateReady
		}
		return model.DeviceReadinessStateTransportPending
	}

	return model.DeviceReadinessStateActivationPending
}

func productReadinessStateFromSources(sources readinessSourcesResponse) model.ProductReadinessState {
	if sources.DeactivationOperationStatus != nil {
		switch *sources.DeactivationOperationStatus {
		case model.DeviceOperationStatusPending, model.DeviceOperationStatusPublished, model.DeviceOperationStatusRetrying:
			return model.ProductReadinessStateDeactivationPending
		case model.DeviceOperationStatusFailed, model.DeviceOperationStatusDeadLettered:
			return model.ProductReadinessStateFailed
		case model.DeviceOperationStatusSucceeded:
			return model.ProductReadinessStateDeactivated
		}
	}

	if sources.VideoCloudActivationStatus != nil && *sources.VideoCloudActivationStatus == string(model.VideoCloudActivationStatusDeactivated) {
		return model.ProductReadinessStateDeactivated
	}

	if !sources.DeviceEnabled || sources.DeviceStatus == model.DeviceStatusDisabled {
		return model.ProductReadinessStateRegistered
	}

	if (sources.ProvisioningOperationStatus != nil &&
		(*sources.ProvisioningOperationStatus == model.DeviceOperationStatusFailed ||
			*sources.ProvisioningOperationStatus == model.DeviceOperationStatusDeadLettered)) ||
		(sources.VideoCloudActivationStatus != nil && *sources.VideoCloudActivationStatus == string(model.VideoCloudActivationStatusFailed)) ||
		sources.VideoCloudLastError != nil {
		return model.ProductReadinessStateFailed
	}

	if sources.ProvisioningOperationStatus != nil &&
		*sources.ProvisioningOperationStatus == model.DeviceOperationStatusSucceeded &&
		sources.VideoCloudActivationStatus != nil &&
		*sources.VideoCloudActivationStatus == string(model.VideoCloudActivationStatusActivated) {
		if sources.DeviceStatus == model.DeviceStatusOnline {
			return model.ProductReadinessStateOnline
		}
		return model.ProductReadinessStateActivated
	}

	if sources.ProvisioningOperationStatus == nil {
		return model.ProductReadinessStateRegistered
	}

	return model.ProductReadinessStateCloudActivationPending
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
