// Package factoryhandoff authenticates factory producer evidence for the durable
// AM coordinator. A successful delivery alone never releases an operation fence.
package factoryhandoff

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"rtk_account_manager/internal/billinghandoff"
	"rtk_account_manager/internal/store"
)

const Participant = "factory"

const (
	ParticipantVideoControlPlane = "video_control_plane"
	ParticipantMQTTUsage         = "mqtt_usage"
)

var (
	ErrInvalid     = errors.New("invalid factory handoff configuration or binding")
	ErrUnavailable = errors.New("factory handoff evidence unavailable; retain the operation fence")
)

type Config struct {
	BaseURL, Token string
	Transport      http.RoundTripper
}
type Client struct {
	baseURL, token        string
	participant, endpoint string
	digestDomain          string
	http                  *http.Client
}

var _ store.HandoffParticipant = (*Client)(nil)

func New(in Config) (*Client, error) {
	return NewParticipant(Participant, in)
}

// NewParticipant builds the same strict authenticated protocol adapter for one
// of the reviewed Video Cloud resource boundaries. Endpoint names and digest
// domains are fixed by participant identity and cannot be supplied by config.
func NewParticipant(participant string, in Config) (*Client, error) {
	endpoint, domain := "", ""
	switch participant {
	case Participant:
		endpoint, domain = "/v1/internal/factory-handoffs/", "factory-cloud-handoff-v1"
	case ParticipantVideoControlPlane:
		endpoint, domain = "/v1/internal/video-control-plane-handoffs/", "video-control-plane-cloud-handoff-v1"
	case ParticipantMQTTUsage:
		endpoint, domain = "/v1/internal/mqtt-usage-handoffs/", "mqtt-usage-cloud-handoff-v1"
	default:
		return nil, ErrInvalid
	}
	u, err := url.Parse(in.BaseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" || len(in.Token) < 32 || strings.TrimSpace(in.Token) != in.Token || strings.ContainsAny(in.Token, " \t\r\n") {
		return nil, ErrInvalid
	}
	if !billinghandoff.TrustedTransportOrigin(u) {
		return nil, ErrInvalid
	}
	u.Path = ""
	return &Client{baseURL: u.String(), token: in.Token, participant: participant, endpoint: endpoint, digestDomain: domain, http: &http.Client{Transport: in.Transport, Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

type binding struct {
	CloudID          string    `json:"cloud_id"`
	OperationID      string    `json:"operation_id"`
	SourceUserID     string    `json:"source_user_id"`
	TargetUserID     string    `json:"target_user_id"`
	OwnershipVersion int64     `json:"ownership_version"`
	Cutoff           time.Time `json:"cutoff"`
}

func wireBinding(in billinghandoff.Binding) binding {
	return binding{in.CloudID, in.OperationID, in.SourceUserID, in.TargetUserID, in.OwnershipVersion, in.Cutoff}
}
func validUUID(s string) bool {
	var id pgtype.UUID
	return id.Scan(s) == nil && id.Valid && id.Bytes != [16]byte{} && id.String() == s
}
func validDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func (b binding) valid() bool {
	return validUUID(b.CloudID) && validUUID(b.OperationID) && validUUID(b.SourceUserID) && validUUID(b.TargetUserID) && b.SourceUserID != b.TargetUserID && b.OwnershipVersion > 0 && !b.Cutoff.IsZero() && b.Cutoff.Equal(b.Cutoff.Truncate(time.Microsecond))
}
func (b binding) equal(o binding) bool {
	return b.CloudID == o.CloudID && b.OperationID == o.OperationID && b.SourceUserID == o.SourceUserID && b.TargetUserID == o.TargetUserID && b.OwnershipVersion == o.OwnershipVersion && b.Cutoff.Equal(o.Cutoff)
}
func (b binding) digest(tag string, extra ...string) string {
	return b.domainDigest("factory-cloud-handoff-v1", tag, extra...)
}

func (b binding) domainDigest(domain, tag string, extra ...string) string {
	fields := []string{domain, tag, b.CloudID, b.OperationID, b.SourceUserID, b.TargetUserID, strconv.FormatInt(b.OwnershipVersion, 10), b.Cutoff.UTC().Format(time.RFC3339Nano)}
	h := sha256.New()
	for _, field := range append(fields, extra...) {
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(field)))
		h.Write(size[:])
		h.Write([]byte(field))
	}
	return hex.EncodeToString(h.Sum(nil))
}

type decision struct {
	binding
	ID               string    `json:"decision_id"`
	SHA256           string    `json:"decision_sha256"`
	At               time.Time `json:"decided_at"`
	CommittedVersion int64     `json:"committed_ownership_version,omitempty"`
}
type record struct {
	binding
	Phase            string     `json:"phase"`
	Hold             string     `json:"hold_receipt_sha256"`
	Drain            string     `json:"drain_checkpoint_sha256,omitempty"`
	Completed        *int64     `json:"completed_count"`
	Canceled         *int64     `json:"canceled_count"`
	DecisionID       string     `json:"decision_id,omitempty"`
	DecisionSHA256   string     `json:"decision_sha256,omitempty"`
	DecidedAt        *time.Time `json:"decided_at,omitempty"`
	CommittedVersion int64      `json:"committed_ownership_version,omitempty"`
}

func (r record) valid(b binding, domain string) bool {
	if !r.binding.equal(b) || r.Hold != b.domainDigest(domain, "hold") || r.Completed == nil || r.Canceled == nil || *r.Completed < 0 || *r.Canceled < 0 {
		return false
	}
	return r.Drain == "" || r.Drain == b.domainDigest(domain, "drained", strconv.FormatInt(*r.Completed, 10), strconv.FormatInt(*r.Canceled, 10))
}

func (c *Client) call(ctx context.Context, path string, b binding, body any) (record, error) {
	if !b.valid() {
		return record{}, ErrInvalid
	}
	if c == nil || c.http == nil {
		return record{}, ErrUnavailable
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return record{}, ErrInvalid
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+c.endpoint+path, bytes.NewReader(raw))
	if err != nil {
		return record{}, ErrInvalid
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return record{}, ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return record{}, ErrUnavailable
	}
	raw, err = io.ReadAll(io.LimitReader(resp.Body, (16<<10)+1))
	if err != nil || len(raw) > 16<<10 {
		return record{}, ErrUnavailable
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var out record
	if decoder.Decode(&out) != nil || decoder.Decode(new(any)) != io.EOF || !out.valid(b, c.digestDomain) {
		return record{}, ErrUnavailable
	}
	return out, nil
}

func (c *Client) Prepare(ctx context.Context, in billinghandoff.Binding) (store.HandoffPrepareAck, error) {
	b := wireBinding(in)
	r, err := c.call(ctx, "prepare", b, b)
	if err != nil {
		return store.HandoffPrepareAck{}, err
	}
	if r.Phase != "prepared" || r.Drain == "" || r.DecisionID != "" || r.DecisionSHA256 != "" || r.DecidedAt != nil || r.CommittedVersion != 0 {
		return store.HandoffPrepareAck{}, ErrUnavailable
	}
	return store.HandoffPrepareAck{CloudID: b.CloudID, OperationID: b.OperationID, SourceUserID: b.SourceUserID, TargetUserID: b.TargetUserID, OwnershipVersion: b.OwnershipVersion, Cutoff: b.Cutoff, Participant: c.participant, HoldReceiptSHA256: r.Hold, DrainCheckpointSHA256: r.Drain}, nil
}

func (c *Client) decide(ctx context.Context, path string, d decision) (string, error) {
	if !d.binding.valid() || !validUUID(d.ID) || !validDigest(d.SHA256) || d.At.Before(d.Cutoff) || !d.At.Equal(d.At.Truncate(time.Microsecond)) {
		return "", ErrInvalid
	}
	if path == "release" && (d.OwnershipVersion == 1<<63-1 || d.CommittedVersion != d.OwnershipVersion+1) {
		return "", ErrInvalid
	}
	r, err := c.call(ctx, path, d.binding, d)
	if err != nil {
		return "", err
	}
	phase := "aborted"
	if path == "release" {
		phase = "released"
	}
	if r.Phase != phase || r.DecisionID != d.ID || r.DecisionSHA256 != d.SHA256 || r.DecidedAt == nil || !r.DecidedAt.Equal(d.At) || r.CommittedVersion != d.CommittedVersion || (path == "release" && r.Drain == "") {
		return "", ErrUnavailable
	}
	// Bind the exact authenticated durable response, never just an HTTP status.
	return d.binding.domainDigest(c.digestDomain, phase, d.ID, d.SHA256, d.At.UTC().Format(time.RFC3339Nano), strconv.FormatInt(d.CommittedVersion, 10)), nil
}
func (c *Client) Abort(ctx context.Context, in store.HandoffCanceledDecision) (store.HandoffAbortAck, error) {
	b := wireBinding(in.Binding)
	proof, err := c.decide(ctx, "abort", decision{binding: b, ID: in.CancellationID, SHA256: in.DecisionSHA256, At: in.CanceledAt})
	if err != nil {
		return store.HandoffAbortAck{}, err
	}
	return store.HandoffAbortAck{CloudID: b.CloudID, OperationID: b.OperationID, OwnershipVersion: b.OwnershipVersion, Participant: c.participant, ReceiptSHA256: proof}, nil
}
func (c *Client) Release(ctx context.Context, in store.HandoffCommittedDecision) (store.HandoffFinalizationAck, error) {
	b := wireBinding(in.Binding)
	proof, err := c.decide(ctx, "release", decision{binding: b, ID: in.AuthorizationID, SHA256: in.DecisionSHA256, At: in.CommittedAt, CommittedVersion: in.CommittedOwnershipVersion})
	if err != nil {
		return store.HandoffFinalizationAck{}, err
	}
	return store.HandoffFinalizationAck{CloudID: b.CloudID, OperationID: b.OperationID, OwnershipVersion: b.OwnershipVersion, DecisionSHA256: in.DecisionSHA256, Participant: c.participant, ReceiptSHA256: proof}, nil
}
