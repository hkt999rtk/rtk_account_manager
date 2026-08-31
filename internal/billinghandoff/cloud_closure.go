package billinghandoff

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"
)

type ClosureBinding struct {
	CloudDeletionScope
	OperationID string    `json:"operation_id"`
	Cutoff      time.Time `json:"cutoff"`
}
type ClosureOperation struct {
	ID               string    `json:"id"`
	AccountID        string    `json:"account_id"`
	OwnerUserID      string    `json:"owner_user_id"`
	OwnershipVersion int64     `json:"ownership_version"`
	Cutoff           time.Time `json:"cutoff"`
	Phase            string    `json:"phase"`
	Version          int64     `json:"version"`
	CreatedAt        time.Time `json:"created_at"`
}
type ClosureStatus struct {
	Operation ClosureOperation `json:"operation"`
	Ready     bool             `json:"ready"`
	ReceiptID string           `json:"receipt_id,omitempty"`
	Blockers  []string         `json:"blockers"`
}
type ClosureAcknowledgment struct {
	OperationID   string    `json:"operation_id"`
	Phase         string    `json:"phase"`
	ClosedAt      time.Time `json:"closed_at"`
	ReceiptSHA256 string    `json:"receipt_sha256"`
}
type CloseCommandResolution struct {
	OperationID       string                 `json:"operation_id"`
	SettlementID      string                 `json:"settlement_id"`
	AMReadinessSHA256 string                 `json:"am_readiness_sha256"`
	Outcome           string                 `json:"outcome"`
	ReceiptSHA256     string                 `json:"receipt_sha256,omitempty"`
	RetiredAt         *time.Time             `json:"retired_at,omitempty"`
	Acknowledgment    *ClosureAcknowledgment `json:"acknowledgment,omitempty"`
}

func (c *Client) RetireCloudClose(ctx context.Context, in ClosureBinding, receipt, sha string) (CloseCommandResolution, error) {
	if !validUUID(receipt) || !digest(sha) {
		return CloseCommandResolution{}, ErrInvalid
	}
	raw, err := c.callClosure(ctx, in, "POST", "retire-command", map[string]string{"settlement_id": receipt, "am_readiness_sha256": sha}, "resolution")
	var out CloseCommandResolution
	if err != nil {
		return out, err
	}
	if json.Unmarshal(raw, &out) != nil || out.OperationID != in.OperationID || out.SettlementID != receipt || out.AMReadinessSHA256 != sha {
		return CloseCommandResolution{}, ErrUnavailable
	}
	switch out.Outcome {
	case "retired":
		if out.RetiredAt == nil || out.RetiredAt.IsZero() || !digest(out.ReceiptSHA256) || out.Acknowledgment != nil {
			return CloseCommandResolution{}, ErrUnavailable
		}
	case "closed":
		if out.RetiredAt != nil || out.ReceiptSHA256 != "" || out.Acknowledgment == nil || out.Acknowledgment.OperationID != in.OperationID || out.Acknowledgment.Phase != "closed" || out.Acknowledgment.ClosedAt.IsZero() || !digest(out.Acknowledgment.ReceiptSHA256) {
			return CloseCommandResolution{}, ErrUnavailable
		}
	default:
		return CloseCommandResolution{}, ErrUnavailable
	}
	return out, nil
}
func (c *Client) CancelCloudClosure(ctx context.Context, in ClosureBinding, cancellationID, sha string) (ClosureOperation, error) {
	if !validUUID(cancellationID) || !digest(sha) {
		return ClosureOperation{}, ErrInvalid
	}
	raw, err := c.callClosure(ctx, in, "POST", "cancel", map[string]string{"cancellation_id": cancellationID, "am_cancellation_sha256": sha}, "operation")
	var out ClosureOperation
	if err != nil {
		return out, err
	}
	if json.Unmarshal(raw, &out) != nil || !validClosureOperation(in, out) || (out.Phase != "canceling" && out.Phase != "canceled") {
		return ClosureOperation{}, ErrUnavailable
	}
	return out, nil
}

func (in ClosureBinding) valid() bool {
	return validUUID(in.CloudID) && validUUID(in.OwnerUserID) && validUUID(in.OperationID) && in.OwnershipVersion > 0 && !in.Cutoff.IsZero()
}
func validClosureOperation(in ClosureBinding, out ClosureOperation) bool {
	if out.ID != in.OperationID || !validUUID(out.AccountID) || out.OwnerUserID != in.OwnerUserID || out.OwnershipVersion != in.OwnershipVersion || !out.Cutoff.Equal(in.Cutoff.UTC().Truncate(time.Microsecond)) || out.Version < 1 || out.CreatedAt.IsZero() {
		return false
	}
	switch out.Phase {
	case "preparing", "closed", "canceling", "canceled":
		return true
	}
	return false
}
func (c *Client) PrepareCloudClosure(ctx context.Context, in ClosureBinding, requestSHA string) (ClosureOperation, error) {
	if !digest(requestSHA) {
		return ClosureOperation{}, ErrInvalid
	}
	raw, err := c.callClosure(ctx, in, "POST", "prepare", map[string]any{"cutoff": in.Cutoff, "am_request_sha256": requestSHA}, "operation")
	var out ClosureOperation
	if err != nil {
		return out, err
	}
	if json.Unmarshal(raw, &out) != nil || !validClosureOperation(in, out) {
		return ClosureOperation{}, ErrUnavailable
	}
	return out, nil
}
func (c *Client) CloudClosureStatus(ctx context.Context, in ClosureBinding) (ClosureStatus, error) {
	raw, err := c.callClosure(ctx, in, "GET", "status", nil, "status")
	var out ClosureStatus
	if err != nil {
		return out, err
	}
	if !hasFields(raw, "operation", "ready", "blockers") || json.Unmarshal(raw, &out) != nil || !validClosureOperation(in, out.Operation) || out.Blockers == nil || (out.ReceiptID != "" && !validUUID(out.ReceiptID)) || out.Ready != (out.Operation.Phase == "preparing" && len(out.Blockers) == 0 && out.ReceiptID != "") {
		return ClosureStatus{}, ErrUnavailable
	}
	seen := map[string]bool{}
	for _, code := range out.Blockers {
		if seen[code] || code == "" {
			return ClosureStatus{}, ErrUnavailable
		}
		seen[code] = true
	}
	return out, nil
}
func (c *Client) CloseCloud(ctx context.Context, in ClosureBinding, settlementID, readinessSHA string) (ClosureAcknowledgment, error) {
	if !validUUID(settlementID) || !digest(readinessSHA) {
		return ClosureAcknowledgment{}, ErrInvalid
	}
	raw, err := c.callClosure(ctx, in, "POST", "close", map[string]string{"settlement_id": settlementID, "am_readiness_sha256": readinessSHA}, "acknowledgment")
	var out ClosureAcknowledgment
	if err != nil {
		return out, err
	}
	if json.Unmarshal(raw, &out) != nil || out.OperationID != in.OperationID || out.Phase != "closed" || out.ClosedAt.IsZero() || !digest(out.ReceiptSHA256) {
		return ClosureAcknowledgment{}, ErrUnavailable
	}
	return out, nil
}

// Original command IDs and content survive ambiguous delivery. This client does
// not retry or turn an HTTP error/timeout into successful closure.
func (c *Client) callClosure(ctx context.Context, in ClosureBinding, method, action string, body any, field string) (json.RawMessage, error) {
	if c == nil || c.http == nil || !in.valid() {
		return nil, ErrInvalid
	}
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			return nil, ErrInvalid
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+"/v1/internal/billing/clouds/"+in.CloudID+"/closures/"+in.OperationID+"/"+action, bytes.NewReader(encoded))
	if err != nil {
		return nil, ErrInvalid
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Billing-Owner-User-ID", in.OwnerUserID)
	req.Header.Set("X-Billing-Ownership-Version", strconv.FormatInt(in.OwnershipVersion, 10))
	res, err := c.http.Do(req)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 64*1024+1))
	if err != nil || len(raw) > 64*1024 {
		return nil, ErrUnavailable
	}
	if res.StatusCode != 200 {
		var envelope struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &envelope)
		code := envelope.Error.Code
		switch code {
		case "BILLING_CLOSURE_NOT_FOUND", "BILLING_CLOSURE_NOT_READY", "BILLING_CLOSURE_CONFLICT", "BILLING_CLOSURE_UNAVAILABLE", "BILLING_CLOSURE_COMMAND_RETIRED":
		default:
			code = "BILLING_CLOSURE_HTTP_ERROR"
		}
		return nil, &HTTPError{Status: res.StatusCode, Code: code}
	}
	var envelope struct {
		CloudDeletionScope
		OperationID string `json:"operation_id"`
	}
	var fields map[string]json.RawMessage
	if res.Header.Get("Cache-Control") != "no-store" || !hasFields(raw, "cloud_id", "owner_user_id", "ownership_version", "operation_id", field) || json.Unmarshal(raw, &envelope) != nil || envelope.CloudDeletionScope != in.CloudDeletionScope || envelope.OperationID != in.OperationID || json.Unmarshal(raw, &fields) != nil {
		return nil, ErrUnavailable
	}
	return fields[field], nil
}
