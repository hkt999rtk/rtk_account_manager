package billinghandoff

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"
)

type CloudDeletionScope struct {
	CloudID          string `json:"cloud_id"`
	OwnerUserID      string `json:"owner_user_id"`
	OwnershipVersion int64  `json:"ownership_version"`
}
type CloudDeletionPreflight struct {
	CloudDeletionScope
	Eligible     bool      `json:"eligible"`
	Blockers     []string  `json:"blockers"`
	BalanceMinor int64     `json:"balance_minor"`
	Currency     string    `json:"currency"`
	ObservedAt   time.Time `json:"observed_at"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// Advisory read only; this result cannot authorize a closure/owner decision.
func (c *Client) CloudDeletionPreflight(ctx context.Context, in CloudDeletionScope) (CloudDeletionPreflight, error) {
	if c == nil || c.http == nil || !validUUID(in.CloudID) || !validUUID(in.OwnerUserID) || in.OwnershipVersion < 1 {
		return CloudDeletionPreflight{}, ErrInvalid
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/internal/billing/clouds/"+in.CloudID+"/deletion-preflight", nil)
	if err != nil {
		return CloudDeletionPreflight{}, ErrInvalid
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-Billing-Ownership-Version", strconv.FormatInt(in.OwnershipVersion, 10))
	req.Header.Set("X-Billing-Owner-User-ID", in.OwnerUserID)
	res, err := c.http.Do(req)
	if err != nil {
		return CloudDeletionPreflight{}, ErrUnavailable
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 64*1024+1))
	if err != nil || len(raw) > 64*1024 || res.StatusCode != http.StatusOK || res.Header.Get("Cache-Control") != "no-store" {
		return CloudDeletionPreflight{}, ErrUnavailable
	}
	var out CloudDeletionPreflight
	if !hasFields(raw, "cloud_id", "owner_user_id", "ownership_version", "eligible", "blockers", "balance_minor", "currency", "observed_at", "expires_at") || json.Unmarshal(raw, &out) != nil || ValidateCloudDeletionPreflight(in, out, time.Now()) != nil {
		return CloudDeletionPreflight{}, ErrUnavailable
	}
	return out, nil
}

func ValidateCloudDeletionPreflight(in CloudDeletionScope, out CloudDeletionPreflight, now time.Time) error {
	if out.CloudDeletionScope != in || out.Currency != "TWD" || out.Blockers == nil || out.Eligible != (len(out.Blockers) == 0) ||
		(out.Eligible && out.BalanceMinor != 0) || out.ObservedAt.After(now) || out.ObservedAt.Before(now.Add(-5*time.Minute)) ||
		!out.ExpiresAt.After(now) || out.ExpiresAt.After(out.ObservedAt.Add(5*time.Minute)) {
		return ErrUnavailable
	}
	seen := map[string]bool{}
	for _, code := range out.Blockers {
		switch code {
		case "balance_nonzero", "usage_unsettled", "debt_outstanding", "payment_pending", "refund_pending", "dispute_pending", "evidence_unavailable", "lifecycle_conflict":
		default:
			return ErrUnavailable
		}
		if seen[code] {
			return ErrUnavailable
		}
		seen[code] = true
	}
	if out.BalanceMinor != 0 && !seen["balance_nonzero"] {
		return ErrUnavailable
	}
	return nil
}
