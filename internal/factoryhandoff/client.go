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
var _ store.CloudDeletionResourceObserver = (*Client)(nil)
var _ store.CloudDeletionProducer = (*Client)(nil)
var _ store.CloudDeletionCancelProducer = (*Client)(nil)

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

type deletionScope struct {
	CloudID              string `json:"cloud_id"`
	OwnerUserID          string `json:"owner_user_id"`
	OwnershipVersion     int64  `json:"ownership_version"`
	AuthorizationVersion int64  `json:"authorization_version"`
}

type deletionEvidence struct {
	deletionScope
	Complete       bool      `json:"complete"`
	ReceiptID      string    `json:"receipt_id"`
	EvidenceSHA256 string    `json:"evidence_sha256"`
	Blockers       []string  `json:"blockers"`
	ObservedAt     time.Time `json:"observed_at"`
	ExpiresAt      time.Time `json:"expires_at"`
}

func (e deletionEvidence) digest() string {
	fields := []string{"video-control-plane-cloud-deletion-v1", e.CloudID, e.OwnerUserID, strconv.FormatInt(e.OwnershipVersion, 10), strconv.FormatInt(e.AuthorizationVersion, 10),
		strconv.FormatBool(e.Complete), e.ReceiptID, e.ObservedAt.UTC().Format(time.RFC3339Nano), e.ExpiresAt.UTC().Format(time.RFC3339Nano)}
	fields = append(fields, e.Blockers...)
	h := sha256.New()
	for _, field := range fields {
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(field)))
		_, _ = h.Write(size[:])
		_, _ = h.Write([]byte(field))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (c *Client) ObserveCloudDeletion(ctx context.Context, in store.CloudDeletionResourceScope) (store.CloudDeletionResourceEvidence, error) {
	scope := deletionScope{CloudID: in.CloudID, OwnerUserID: in.OwnerUserID, OwnershipVersion: in.OwnershipVersion, AuthorizationVersion: in.AuthorizationVersion}
	if c == nil || c.http == nil || c.participant != ParticipantVideoControlPlane || !validUUID(scope.CloudID) || !validUUID(scope.OwnerUserID) || scope.OwnershipVersion <= 0 || scope.AuthorizationVersion <= 0 {
		return store.CloudDeletionResourceEvidence{}, ErrInvalid
	}
	raw, err := json.Marshal(scope)
	if err != nil {
		return store.CloudDeletionResourceEvidence{}, ErrInvalid
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+c.endpoint+"deletion-preflight", bytes.NewReader(raw))
	if err != nil {
		return store.CloudDeletionResourceEvidence{}, ErrInvalid
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return store.CloudDeletionResourceEvidence{}, ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-store" {
		return store.CloudDeletionResourceEvidence{}, ErrUnavailable
	}
	raw, err = io.ReadAll(io.LimitReader(resp.Body, (16<<10)+1))
	if err != nil || len(raw) > 16<<10 {
		return store.CloudDeletionResourceEvidence{}, ErrUnavailable
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var evidence deletionEvidence
	if decoder.Decode(&evidence) != nil || decoder.Decode(new(any)) != io.EOF || evidence.deletionScope != scope || !evidence.Complete || !validUUID(evidence.ReceiptID) || evidence.Blockers == nil || evidence.EvidenceSHA256 != evidence.digest() {
		return store.CloudDeletionResourceEvidence{}, ErrUnavailable
	}
	for _, blocker := range evidence.Blockers {
		switch blocker {
		case "products_present", "devices_present", "jobs_running", "lifecycle_conflict":
		default:
			return store.CloudDeletionResourceEvidence{}, ErrUnavailable
		}
	}
	return store.CloudDeletionResourceEvidence{Scope: in, Complete: true, ReceiptID: evidence.ReceiptID, EvidenceSHA256: evidence.EvidenceSHA256, Blockers: evidence.Blockers, ObservedAt: evidence.ObservedAt, ExpiresAt: evidence.ExpiresAt}, nil
}

type deletionBinding struct {
	deletionScope
	OperationID string    `json:"operation_id"`
	Cutoff      time.Time `json:"cutoff"`
}

func wireDeletionBinding(in billinghandoff.ClosureBinding, authorizationVersion int64) deletionBinding {
	return deletionBinding{deletionScope: deletionScope{CloudID: in.CloudID, OwnerUserID: in.OwnerUserID, OwnershipVersion: in.OwnershipVersion, AuthorizationVersion: authorizationVersion}, OperationID: in.OperationID, Cutoff: in.Cutoff}
}

func (b deletionBinding) valid() bool {
	return validUUID(b.CloudID) && validUUID(b.OwnerUserID) && validUUID(b.OperationID) && b.OwnershipVersion > 0 && b.AuthorizationVersion > 0 && !b.Cutoff.IsZero() && b.Cutoff.Equal(b.Cutoff.Truncate(time.Microsecond))
}

func (b deletionBinding) digest(tag string, extra ...string) string {
	fields := []string{"video-control-plane-cloud-deletion-hold-v1", tag, b.CloudID, b.OwnerUserID, b.OperationID, strconv.FormatInt(b.OwnershipVersion, 10), strconv.FormatInt(b.AuthorizationVersion, 10), b.Cutoff.UTC().Format(time.RFC3339Nano)}
	fields = append(fields, extra...)
	h := sha256.New()
	for _, field := range fields {
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(field)))
		_, _ = h.Write(size[:])
		_, _ = h.Write([]byte(field))
	}
	return hex.EncodeToString(h.Sum(nil))
}

type deletionHold struct {
	deletionBinding
	Participant    string `json:"participant"`
	Phase          string `json:"phase"`
	Held           bool   `json:"held"`
	Empty          bool   `json:"empty"`
	ReceiptSHA256  string `json:"receipt_sha256"`
	CancellationID string `json:"cancellation_id,omitempty"`
}

func (c *Client) callDeletion(ctx context.Context, path string, body any, binding deletionBinding) (deletionHold, error) {
	if c == nil || c.http == nil || c.participant != ParticipantVideoControlPlane || !binding.valid() {
		return deletionHold{}, ErrInvalid
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return deletionHold{}, ErrInvalid
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+c.endpoint+path, bytes.NewReader(raw))
	if err != nil {
		return deletionHold{}, ErrInvalid
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return deletionHold{}, ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-store" {
		return deletionHold{}, ErrUnavailable
	}
	raw, err = io.ReadAll(io.LimitReader(resp.Body, (16<<10)+1))
	if err != nil || len(raw) > 16<<10 {
		return deletionHold{}, ErrUnavailable
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var out deletionHold
	if decoder.Decode(&out) != nil || decoder.Decode(new(any)) != io.EOF || out.deletionBinding != binding || out.Participant != ParticipantVideoControlPlane || !validDigest(out.ReceiptSHA256) {
		return deletionHold{}, ErrUnavailable
	}
	return out, nil
}

func (c *Client) PrepareCloudDeletion(ctx context.Context, in billinghandoff.ClosureBinding, authorizationVersion int64) (store.CloudDeletionHold, error) {
	binding := wireDeletionBinding(in, authorizationVersion)
	out, err := c.callDeletion(ctx, "deletion-hold", binding, binding)
	if err != nil {
		return store.CloudDeletionHold{}, err
	}
	if out.Phase != "holding" || !out.Held || out.CancellationID != "" || out.ReceiptSHA256 != binding.digest("holding", strconv.FormatBool(out.Empty)) {
		return store.CloudDeletionHold{}, ErrUnavailable
	}
	return store.CloudDeletionHold{Binding: in, AuthorizationVersion: authorizationVersion, Participant: out.Participant, Held: true, Empty: out.Empty, ReceiptSHA256: out.ReceiptSHA256}, nil
}

func (c *Client) CancelCloudDeletion(ctx context.Context, in billinghandoff.ClosureBinding, authorizationVersion int64, cancellationID, cancellationSHA string) (store.CloudDeletionRelease, error) {
	binding := wireDeletionBinding(in, authorizationVersion)
	body := struct {
		deletionBinding
		CancellationID     string `json:"cancellation_id"`
		CancellationSHA256 string `json:"cancellation_sha256"`
	}{binding, cancellationID, cancellationSHA}
	if !validUUID(cancellationID) || !validDigest(cancellationSHA) {
		return store.CloudDeletionRelease{}, ErrInvalid
	}
	out, err := c.callDeletion(ctx, "deletion-cancel", body, binding)
	if err != nil {
		return store.CloudDeletionRelease{}, err
	}
	if out.Phase != "canceled" || out.Held || out.Empty || out.CancellationID != cancellationID || out.ReceiptSHA256 != binding.digest("canceled", cancellationID, cancellationSHA) {
		return store.CloudDeletionRelease{}, ErrUnavailable
	}
	return store.CloudDeletionRelease{Binding: in, CancellationID: cancellationID, Participant: out.Participant, ReceiptSHA256: out.ReceiptSHA256, Released: true}, nil
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
