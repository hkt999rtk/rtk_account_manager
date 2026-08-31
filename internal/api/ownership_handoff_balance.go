package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

func requireHandoffGlobalSession(c *gin.Context) bool {
	if currentSubjectType(c) != auth.SubjectTypeUser || currentUserID(c) == "" {
		writeError(c, http.StatusForbidden, "global_account_required", "Owner handoff requires a global human account session")
		return false
	}
	return true
}

func ownerHandoffQuery(c *gin.Context) store.BrandCloudOwnerTransferQuery {
	return store.BrandCloudOwnerTransferQuery{BrandCloudID: c.Param("brandCloudId"), TransferID: c.Param("transferId"), RequesterID: currentUserID(c)}
}
func (s *Server) previewOwnerHandoff(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if !requireHandoffGlobalSession(c) {
		return
	}
	view, err := s.store.PreviewOwnerHandoff(c.Request.Context(), ownerHandoffQuery(c))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, brandCloudOwnerTransferResponse{OwnerTransfer: view})
}
func (s *Server) confirmOwnerHandoff(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if !requireHandoffGlobalSession(c) {
		return
	}
	key := c.GetHeader("Idempotency-Key")
	if len(key) == 0 || len(key) > 128 {
		writeError(c, http.StatusBadRequest, "invalid_request", "Idempotency-Key is required (1-128 printable characters)")
		return
	}
	for _, r := range key {
		if r < 33 || r > 126 {
			writeError(c, http.StatusBadRequest, "invalid_request", "Invalid Idempotency-Key")
			return
		}
	}
	var req struct {
		OwnershipVersion       int64  `json:"ownership_version"`
		BillingSnapshotVersion int64  `json:"billing_snapshot_version"`
		BalanceMinor           *int64 `json:"balance_minor"`
		Currency               string `json:"currency"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16*1024)
	if !bindStrict(c, &req) {
		return
	}
	if req.OwnershipVersion < 1 || req.BillingSnapshotVersion < 2 || req.BalanceMinor == nil || *req.BalanceMinor < 0 || req.Currency != "TWD" {
		writeError(c, http.StatusBadRequest, "invalid_request", "Explicit nonnegative balance, TWD currency and exact ownership/Billing versions are required")
		return
	}
	view, err := s.store.ConfirmOwnerHandoff(c.Request.Context(), store.HandoffConfirmationInput{Query: ownerHandoffQuery(c), IdempotencyKey: key, Snapshot: model.CloudBalanceSnapshot{
		OwnershipVersion: req.OwnershipVersion, BillingSnapshotVersion: req.BillingSnapshotVersion, BalanceMinor: *req.BalanceMinor, Currency: req.Currency}})
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, brandCloudOwnerTransferResponse{OwnerTransfer: view})
}
