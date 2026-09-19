package api

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/store"
)

type devicePKIStore interface {
	ClaimDevicePKI(context.Context) (store.DevicePKIJob, error)
	WithDevicePKIContext(context.Context, store.DevicePKIJob, func() error) (bool, error)
	FinishDevicePKI(context.Context, store.DevicePKIJob, store.DevicePKIReceipt, string) error
}

// RunDevicePKI uses the underlying store, not a possibly stale user cache. Only
// this worker can mint the dedicated machine assertion; the human PKI proxy
// does not expose /automatic or accept client-supplied roles.
func (s *Server) RunDevicePKI(ctx context.Context, jobs devicePKIStore) {
	if s.pkiClient == nil {
		return
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		work, cancel := context.WithTimeout(ctx, 45*time.Second)
		err := s.runDevicePKIOnce(work, jobs)
		if err != nil && !errors.Is(err, store.ErrNotFound) && s.logger != nil {
			s.logger.Warn("automatic Device PKI work could not complete")
		}
		cancel()
	}
}

func (s *Server) runDevicePKIOnce(ctx context.Context, jobs devicePKIStore) error {
	j, err := jobs.ClaimDevicePKI(ctx)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"brand_cloud_id": j.CloudID, "device_item_profile_id": j.ProductID})
	var status int
	var raw []byte
	active, err := jobs.WithDevicePKIContext(ctx, j, func() error {
		var callErr error
		status, raw, callErr = s.pkiCall(ctx, auth.PKIAssertionInput{UserID: "service:account-manager:pki-provisioner", Roles: []string{"pki_provisioner"}, Environment: s.pkiClient.environment, Method: "POST", Path: "/v1/pki/automatic", Body: body, IdempotencyKey: j.OperationID, ActiveContext: true}, s.now())
		return callErr
	})
	if err == nil && !active {
		return jobs.FinishDevicePKI(ctx, j, store.DevicePKIReceipt{Status: "cancelled"}, "context_inactive")
	}
	r := store.DevicePKIReceipt{Status: "pending"}
	code := ""
	if err != nil || status >= 500 {
		code = "provider_unavailable"
	} else if status != 200 {
		r.Status = "failed"
		code = "reconciliation_required"
	} else if json.Unmarshal(raw, &r) != nil || (r.Status != "ready" && r.Status != "pending") || r.Validate() != nil {
		r = store.DevicePKIReceipt{Status: "failed"}
		code = "reconciliation_required"
	}
	if code != "" && s.logger != nil {
		s.logger.Warn("automatic Device PKI requires retry or reconciliation")
	}
	return jobs.FinishDevicePKI(ctx, j, r, code)
}
