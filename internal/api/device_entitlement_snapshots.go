package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/store"
)

type deviceEntitlementSnapshotRequest struct {
	OperationID                  string   `json:"operation_id"`
	TargetProductServiceRevision *int64   `json:"target_product_service_revision"`
	ServiceOptions               []string `json:"service_options"`
	State                        string   `json:"state"`
}

type deviceEntitlementSnapshotResponse struct {
	Operation operationResponse               `json:"operation"`
	Snapshot  store.DeviceEntitlementSnapshot `json:"snapshot"`
}

func (s *Server) createDeviceEntitlementSnapshot(c *gin.Context) {
	var req deviceEntitlementSnapshotRequest
	if !bindStrict(c, &req) {
		return
	}
	if req.State != "active" && req.State != "suspended" && req.State != "revoked" {
		writeError(c, http.StatusBadRequest, "invalid_entitlement_state", "state must be active, suspended, or revoked")
		return
	}
	var options []string
	if req.ServiceOptions != nil {
		var ok bool
		options, ok = s.optionalRegisteredServiceEcho(c, req.ServiceOptions)
		if !ok || len(options) == 0 {
			if ok {
				writeError(c, http.StatusBadRequest, "unsupported_service_option", "service_options cannot be empty")
			}
			return
		}
	}
	if req.TargetProductServiceRevision != nil && options == nil {
		writeError(c, http.StatusBadRequest, "service_options_required", "A Product revision migration requires explicit service_options")
		return
	}
	operationID, ok := ensureOperationID(c, req.OperationID)
	if !ok {
		return
	}
	messageID, err := newOpaqueID()
	if err != nil {
		writeError(c, http.StatusInternalServerError, "id_generation_failed", "Could not generate outbox message id")
		return
	}
	result, err := s.store.StartDeviceEntitlementSnapshot(c.Request.Context(), store.DeviceEntitlementSnapshotInput{
		OperationID: operationID, CorrelationID: operationID, MessageID: messageID,
		OrganizationID: c.Param("orgId"), DeviceID: c.Param("deviceId"), RequestedBy: currentUserID(c),
		TargetProductServiceRevision: req.TargetProductServiceRevision, ServiceOptions: options,
		State: strings.TrimSpace(req.State), Now: time.Now().UTC().Truncate(time.Microsecond),
	})
	if err != nil {
		writeStoreError(c, err)
		return
	}
	status := http.StatusAccepted
	if !result.Created {
		status = http.StatusOK
	}
	c.JSON(status, deviceEntitlementSnapshotResponse{
		Operation: operationFromResult(result.Operation, result.Message), Snapshot: result.Snapshot,
	})
}
