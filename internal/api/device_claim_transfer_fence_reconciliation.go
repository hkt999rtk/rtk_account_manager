package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/store"
)

type cancelDeviceClaimTransferFenceRequest struct {
	ReservationID string         `json:"reservation_id" binding:"required"`
	Reason        string         `json:"reason" binding:"required"`
	Evidence      map[string]any `json:"evidence" binding:"required"`
}

func (s *Server) inspectDeviceClaimTransferFence(c *gin.Context) {
	s.reconcileDeviceClaimTransferFence(c, false, cancelDeviceClaimTransferFenceRequest{})
}

func (s *Server) cancelDeviceClaimTransferFence(c *gin.Context) {
	var req cancelDeviceClaimTransferFenceRequest
	if !bindStrict(c, &req) {
		return
	}
	if strings.TrimSpace(req.ReservationID) == "" || strings.TrimSpace(req.Reason) == "" || len(req.Evidence) == 0 {
		writeError(c, http.StatusBadRequest, "operator_evidence_required", "Exact reservation ID, reason, and evidence are required")
		return
	}
	s.reconcileDeviceClaimTransferFence(c, true, req)
}

func (s *Server) reconcileDeviceClaimTransferFence(c *gin.Context, cancel bool, req cancelDeviceClaimTransferFenceRequest) {
	fencer, ok := s.videoPresence.(videoTransferFencer)
	if !ok {
		writeError(c, http.StatusServiceUnavailable, "cloud_lifecycle_check_unavailable", "Video Cloud transfer fence is unavailable")
		return
	}
	result, err := s.store.ReconcileDeviceTransferFence(c.Request.Context(), store.DeviceTransferFenceReconcileInput{
		ClaimID: c.Param("claimId"), ExpectedReservationID: strings.TrimSpace(req.ReservationID),
		Cancel: cancel, ActorUserID: currentUserID(c), Reason: strings.TrimSpace(req.Reason), Evidence: req.Evidence,
		ReadFence: fencer.ReadTransferFence, ReleaseFence: fencer.ReleaseTransferFence,
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeError(c, http.StatusNotFound, "not_found", "Device claim not found")
		case errors.Is(err, store.ErrConflict):
			writeError(c, http.StatusConflict, "transfer_fence_not_cancelable", "Inspect current account and Video Cloud state before cancellation")
		case errors.Is(err, store.ErrClaimEvidenceRequired):
			writeError(c, http.StatusBadRequest, "operator_evidence_required", "Exact reservation ID, reason, and evidence are required")
		default:
			writeError(c, http.StatusServiceUnavailable, "cloud_lifecycle_check_unavailable", "Transfer fence reconciliation is unavailable; inspect state before retry")
		}
		return
	}
	c.JSON(http.StatusOK, result)
}
