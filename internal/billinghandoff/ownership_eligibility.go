package billinghandoff

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strconv"
	"time"
)

type OwnershipEligibilityRequest struct {
	CloudID          string `json:"cloud_id"`
	SourceUserID     string `json:"source_user_id"`
	TargetUserID     string `json:"target_user_id"`
	TransferID       string `json:"transfer_id"`
	Action           string `json:"action"`
	OwnershipVersion int64  `json:"ownership_version"`
}

func (in OwnershipEligibilityRequest) valid() bool {
	return validUUID(in.CloudID) && validUUID(in.SourceUserID) && validUUID(in.TargetUserID) && in.SourceUserID != in.TargetUserID && in.OwnershipVersion > 0 &&
		((in.Action == "request" && in.TransferID == "") || (in.Action == "accept" && validUUID(in.TransferID)))
}

type OwnershipEligibility struct {
	Request        OwnershipEligibilityRequest `json:"request"`
	ReceiptID      string                      `json:"receipt_id"`
	EvidenceSHA256 string                      `json:"evidence_sha256"`
	Currency       string                      `json:"currency"`
	BalanceMinor   int64                       `json:"balance_minor"`
	Complete       bool                        `json:"complete"`
	Blockers       []string                    `json:"blockers"`
	ObservedAt     time.Time                   `json:"observed_at"`
	ExpiresAt      time.Time                   `json:"expires_at"`
}

// This advisory query cannot reserve funds, manufacture collector evidence or
// authorize commit. The store rechecks admission after the network round trip.
func (c *Client) CheckOwnershipEligibility(ctx context.Context, in OwnershipEligibilityRequest) (OwnershipEligibility, error) {
	if c == nil || c.http == nil || !in.valid() {
		return OwnershipEligibility{}, ErrInvalid
	}
	body, _ := json.Marshal(struct {
		TargetUserID string `json:"target_user_id"`
		TransferID   string `json:"transfer_id"`
		Action       string `json:"action"`
	}{in.TargetUserID, in.TransferID, in.Action})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/internal/billing/clouds/"+in.CloudID+"/ownership-eligibility", bytes.NewReader(body))
	if err != nil {
		return OwnershipEligibility{}, ErrInvalid
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-Billing-Owner-User-ID", in.SourceUserID)
	req.Header.Set("X-Billing-Ownership-Version", strconv.FormatInt(in.OwnershipVersion, 10))
	res, err := c.http.Do(req)
	if err != nil {
		return OwnershipEligibility{}, ErrUnavailable
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 64*1024+1))
	if err != nil || len(raw) > 64*1024 || res.StatusCode != http.StatusOK || res.Header.Get("Cache-Control") != "no-store" {
		return OwnershipEligibility{}, ErrUnavailable
	}
	var out OwnershipEligibility
	var envelope map[string]json.RawMessage
	if !hasFields(raw, "request", "receipt_id", "evidence_sha256", "currency", "balance_minor", "complete", "blockers", "observed_at", "expires_at") || json.Unmarshal(raw, &out) != nil || json.Unmarshal(raw, &envelope) != nil ||
		!hasFields(envelope["request"], "cloud_id", "source_user_id", "target_user_id", "transfer_id", "action", "ownership_version") || validateOwnershipEligibility(in, out, time.Now()) != nil {
		return OwnershipEligibility{}, ErrUnavailable
	}
	return out, nil
}

func validateOwnershipEligibility(in OwnershipEligibilityRequest, out OwnershipEligibility, now time.Time) error {
	if out.Request != in || out.Currency != "TWD" || out.Blockers == nil || out.ObservedAt.After(now) || out.ObservedAt.Before(now.Add(-5*time.Minute)) || !out.ExpiresAt.After(now) || out.ExpiresAt.After(out.ObservedAt.Add(5*time.Minute)) {
		return ErrUnavailable
	}
	if (out.Complete || out.ReceiptID != "" || out.EvidenceSHA256 != "") && (!validUUID(out.ReceiptID) || !digest(out.EvidenceSHA256)) {
		return ErrUnavailable
	}
	if !out.Complete && len(out.Blockers) == 0 {
		return ErrUnavailable
	}
	seen := map[string]bool{}
	for _, code := range out.Blockers {
		if !slices.Contains([]string{"balance_negative", "usage_unsettled", "debt_outstanding", "payment_pending", "refund_pending", "dispute_pending", "lifecycle_conflict", "evidence_unavailable"}, code) || seen[code] {
			return ErrUnavailable
		}
		seen[code] = true
	}
	if (out.BalanceMinor < 0) != seen["balance_negative"] {
		return ErrUnavailable
	}
	return nil
}
