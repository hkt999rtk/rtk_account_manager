// Package billinghandoff is the dedicated Account Manager-to-Billing transport.
// It carries durable coordinator decisions, not browser-supplied authority.
package billinghandoff

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

var (
	ErrInvalid     = errors.New("invalid billing handoff configuration or scope")
	ErrUnavailable = errors.New("billing handoff response unavailable or invalid; retain the operation fence")
)

type HTTPError struct {
	Status int
	Code   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("billing handoff rejected (%d, %s)", e.Status, e.Code)
}

type Config struct {
	BaseURL, Token string
	// Optional trust/mTLS transport, configured by the service, never a tenant.
	Transport http.RoundTripper
}
type Client struct {
	baseURL, token string
	http           *http.Client
}

func New(in Config) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(in.BaseURL))
	in.Token = strings.TrimSpace(in.Token)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" ||
		len(in.Token) < 32 || strings.ContainsAny(in.Token, " \t\r\n") {
		return nil, ErrInvalid
	}
	if !TrustedTransportOrigin(u) {
		return nil, ErrInvalid
	}
	u.Path = ""
	return &Client{baseURL: u.String(), token: in.Token, http: &http.Client{Transport: in.Transport, Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// TrustedTransportOrigin permits TLS everywhere and plaintext only where the
// destination is provably local to the process or Kubernetes service network.
// The cluster-local case is additionally fenced by a dedicated credential and
// ingress NetworkPolicy in the deployment repository.
func TrustedTransportOrigin(u *url.URL) bool {
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme != "http" {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return strings.HasSuffix(host, ".svc.cluster.local")
}

type Binding struct {
	CloudID, OperationID, SourceUserID, TargetUserID string
	OwnershipVersion                                 int64
	Cutoff                                           time.Time
}

func validUUID(value string) bool {
	var id pgtype.UUID
	return id.Scan(value) == nil && id.Valid && id.Bytes != [16]byte{} && id.String() == value
}
func (in Binding) valid() bool {
	return validUUID(in.CloudID) && validUUID(in.OperationID) && validUUID(in.SourceUserID) && validUUID(in.TargetUserID) && in.SourceUserID != in.TargetUserID && in.OwnershipVersion > 0 && !in.Cutoff.IsZero()
}
func digest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

type Prepared struct {
	ID               string    `json:"id"`
	AccountID        string    `json:"account_id"`
	SourceUserID     string    `json:"source_user_id"`
	TargetUserID     string    `json:"target_user_id"`
	OwnershipVersion int64     `json:"ownership_version"`
	Cutoff           time.Time `json:"cutoff"`
	Phase            string    `json:"phase"`
	Version          int64     `json:"version"`
	CreatedAt        time.Time `json:"created_at"`
}
type Snapshot struct {
	Version         int64     `json:"version"`
	BalanceMinor    int64     `json:"balance_minor"`
	Currency        string    `json:"currency"`
	Cutoff          time.Time `json:"cutoff"`
	SourceConfirmed bool      `json:"source_confirmed"`
	TargetConfirmed bool      `json:"target_confirmed"`
}
type Settlement struct {
	OperationID string    `json:"operation_id"`
	Phase       string    `json:"phase"`
	Blockers    []string  `json:"blockers"`
	Snapshot    *Snapshot `json:"snapshot,omitempty"`
}
type Confirmation struct {
	UserID          string `json:"user_id"`
	SnapshotVersion int64  `json:"snapshot_version"`
	BalanceMinor    int64  `json:"balance_minor"`
	Currency        string `json:"currency"`
}
type Authorization struct {
	OperationID     string    `json:"operation_id"`
	AuthorizationID string    `json:"authorization_id"`
	SnapshotVersion int64     `json:"snapshot_version"`
	CreatedAt       time.Time `json:"created_at"`
}
type ProtocolAck struct {
	OperationID string `json:"operation_id"`
	Phase       string `json:"phase"`
}

func (c *Client) Prepare(ctx context.Context, in Binding) (Prepared, error) {
	raw, err := c.call(ctx, in, http.MethodPost, "prepare", map[string]any{"source_user_id": in.SourceUserID, "target_user_id": in.TargetUserID, "cutoff": in.Cutoff}, "operation")
	var out Prepared
	if err != nil {
		return out, err
	}
	if json.Unmarshal(raw, &out) != nil || out.ID != in.OperationID || !validUUID(out.AccountID) || out.SourceUserID != in.SourceUserID || out.TargetUserID != in.TargetUserID ||
		out.OwnershipVersion != in.OwnershipVersion || !out.Cutoff.Equal(in.Cutoff.UTC().Truncate(time.Microsecond)) || out.Version < 1 || out.CreatedAt.IsZero() || out.Phase == "" {
		return Prepared{}, ErrUnavailable
	}
	return out, nil
}

func (c *Client) Settlement(ctx context.Context, in Binding) (Settlement, error) {
	raw, err := c.call(ctx, in, http.MethodGet, "settlement", nil, "settlement")
	if err != nil {
		return Settlement{}, err
	}
	return decodeSettlement(raw, in)
}
func (c *Client) Confirm(ctx context.Context, in Binding, confirmation Confirmation) (Settlement, error) {
	if (confirmation.UserID != in.SourceUserID && confirmation.UserID != in.TargetUserID) || confirmation.SnapshotVersion < 2 || confirmation.BalanceMinor < 0 || confirmation.Currency != "TWD" {
		return Settlement{}, ErrInvalid
	}
	raw, err := c.call(ctx, in, http.MethodPost, "confirm", confirmation, "settlement")
	if err != nil {
		return Settlement{}, err
	}
	out, err := decodeSettlement(raw, in)
	if err != nil {
		return Settlement{}, err
	}
	if out.Snapshot == nil || out.Snapshot.Version != confirmation.SnapshotVersion || out.Snapshot.BalanceMinor != confirmation.BalanceMinor ||
		(confirmation.UserID == in.SourceUserID && !out.Snapshot.SourceConfirmed) || (confirmation.UserID == in.TargetUserID && !out.Snapshot.TargetConfirmed) {
		return Settlement{}, ErrUnavailable
	}
	return out, nil
}

func decodeSettlement(raw []byte, in Binding) (Settlement, error) {
	var out Settlement
	if !hasFields(raw, "operation_id", "phase", "blockers") || json.Unmarshal(raw, &out) != nil || out.OperationID != in.OperationID || out.Phase == "" || out.Blockers == nil {
		return out, ErrUnavailable
	}
	if out.Snapshot != nil {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil || !hasFields(fields["snapshot"], "version", "balance_minor", "currency", "cutoff", "source_confirmed", "target_confirmed") ||
			out.Snapshot.Version < 2 || out.Snapshot.BalanceMinor < 0 || out.Snapshot.Currency != "TWD" || !out.Snapshot.Cutoff.Equal(in.Cutoff.UTC().Truncate(time.Microsecond)) || len(out.Blockers) != 0 || out.Phase != "prepared" {
			return Settlement{}, ErrUnavailable
		}
	} else if len(out.Blockers) == 0 {
		return Settlement{}, ErrUnavailable
	}
	return out, nil
}
func hasFields(raw []byte, names ...string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return false
	}
	for _, name := range names {
		v, ok := fields[name]
		if !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return false
		}
	}
	return true
}

func (c *Client) AuthorizeCommit(ctx context.Context, in Binding, id string, snapshotVersion int64) (Authorization, error) {
	if !validUUID(id) || snapshotVersion < 2 {
		return Authorization{}, ErrInvalid
	}
	raw, err := c.call(ctx, in, http.MethodPost, "authorize-commit", map[string]any{"authorization_id": id, "snapshot_version": snapshotVersion}, "authorization")
	var out Authorization
	if err != nil {
		return out, err
	}
	if json.Unmarshal(raw, &out) != nil || out.OperationID != in.OperationID || out.AuthorizationID != id || out.SnapshotVersion != snapshotVersion || out.CreatedAt.IsZero() {
		return Authorization{}, ErrUnavailable
	}
	return out, nil
}

func (c *Client) Finalize(ctx context.Context, in Binding, authorizationID string, committedAt time.Time, amCommitSHA256 string) (ProtocolAck, error) {
	if !validUUID(authorizationID) || in.OwnershipVersion == math.MaxInt64 || committedAt.Before(in.Cutoff) || !digest(amCommitSHA256) {
		return ProtocolAck{}, ErrInvalid
	}
	raw, err := c.call(ctx, in, http.MethodPost, "finalize", map[string]any{"authorization_id": authorizationID, "committed_owner_user_id": in.TargetUserID,
		"committed_ownership_version": in.OwnershipVersion + 1, "committed_at": committedAt, "am_commit_sha256": amCommitSHA256}, "operation")
	if err != nil {
		return ProtocolAck{}, err
	}
	var out ProtocolAck
	if json.Unmarshal(raw, &out) != nil || out.OperationID != in.OperationID || out.Phase != "finalized" {
		return ProtocolAck{}, ErrUnavailable
	}
	return out, nil
}
func (c *Client) Abort(ctx context.Context, in Binding, cancellationID, authorizationID, amCancellationSHA256 string) (ProtocolAck, error) {
	if !validUUID(cancellationID) || (authorizationID != "" && !validUUID(authorizationID)) || !digest(amCancellationSHA256) {
		return ProtocolAck{}, ErrInvalid
	}
	body := map[string]any{"cancellation_id": cancellationID, "am_cancellation_sha256": amCancellationSHA256}
	if authorizationID != "" {
		body["authorization_id"] = authorizationID
	}
	raw, err := c.call(ctx, in, http.MethodPost, "abort", body, "operation")
	if err != nil {
		return ProtocolAck{}, err
	}
	var out ProtocolAck
	if json.Unmarshal(raw, &out) != nil || out.OperationID != in.OperationID || (out.Phase != "abort_pending" && out.Phase != "aborted") {
		return ProtocolAck{}, ErrUnavailable
	}
	return out, nil
}

// No automatic retry changes an operation ID, amount or decision. Ambiguous
// delivery must be retried by the durable coordinator with the identical input.
func (c *Client) call(ctx context.Context, in Binding, method, action string, body any, field string) (json.RawMessage, error) {
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
	path := c.baseURL + "/v1/internal/billing/clouds/" + in.CloudID + "/ownership-handoffs/" + in.OperationID + "/" + action
	req, err := http.NewRequestWithContext(ctx, method, path, bytes.NewReader(encoded))
	if err != nil {
		return nil, ErrInvalid
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-Billing-Ownership-Version", strconv.FormatInt(in.OwnershipVersion, 10))
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 64*1024+1))
	if err != nil || len(raw) > 64*1024 {
		return nil, ErrUnavailable
	}
	if res.StatusCode != http.StatusOK {
		var envelope struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &envelope)
		code := envelope.Error.Code
		switch code {
		case "unauthorized", "BILLING_HANDOFF_CONTEXT_INVALID", "BILLING_HANDOFF_REQUEST_INVALID", "BILLING_HANDOFF_NOT_FOUND", "BILLING_HANDOFF_PARTICIPANT_REQUIRED",
			"BILLING_HANDOFF_VERSION_CONFLICT", "BILLING_HANDOFF_SNAPSHOT_CONFLICT", "BILLING_HANDOFF_CONFLICT", "BILLING_HANDOFF_UNAVAILABLE":
		default:
			code = "BILLING_HANDOFF_HTTP_ERROR"
		}
		return nil, &HTTPError{Status: res.StatusCode, Code: code}
	}
	var envelope struct {
		CloudID          string `json:"cloud_id"`
		OperationID      string `json:"operation_id"`
		OwnershipVersion int64  `json:"ownership_version"`
	}
	var fields map[string]json.RawMessage
	if res.Header.Get("Cache-Control") != "no-store" || json.Unmarshal(raw, &envelope) != nil || envelope.CloudID != in.CloudID || envelope.OperationID != in.OperationID || envelope.OwnershipVersion != in.OwnershipVersion || json.Unmarshal(raw, &fields) != nil || !hasFields(raw, field) {
		return nil, ErrUnavailable
	}
	return fields[field], nil
}
