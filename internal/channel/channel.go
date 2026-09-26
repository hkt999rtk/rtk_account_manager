package channel

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	SchemaVersionV1 = "1.0"

	StreamAccountVideoCommands = "account.video.commands"
	StreamVideoAccountEvents   = "video.account.events"

	ServiceAccountManager    = "rtk_account_manager"
	ServiceRealtekVideoCloud = "realtek_video_server"
)

var serviceOptionCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

type MessageType string

const (
	MessageTypeDeviceProvisionRequested           MessageType = "DeviceProvisionRequested"
	MessageTypeDeviceProvisionSucceeded           MessageType = "DeviceProvisionSucceeded"
	MessageTypeDeviceProvisionFailed              MessageType = "DeviceProvisionFailed"
	MessageTypeDeviceDeactivateRequested          MessageType = "DeviceDeactivateRequested"
	MessageTypeDeviceDeactivateSucceeded          MessageType = "DeviceDeactivateSucceeded"
	MessageTypeDeviceDeactivateFailed             MessageType = "DeviceDeactivateFailed"
	MessageTypeDeviceUnprovisionRequested         MessageType = "DeviceUnprovisionRequested"
	MessageTypeDeviceUnprovisionSucceeded         MessageType = "DeviceUnprovisionSucceeded"
	MessageTypeDeviceUnprovisionFailed            MessageType = "DeviceUnprovisionFailed"
	MessageTypeDeviceEntitlementSnapshotRequested MessageType = "DeviceEntitlementSnapshotRequested"
	MessageTypeDeviceEntitlementSnapshotSucceeded MessageType = "DeviceEntitlementSnapshotSucceeded"
	MessageTypeDeviceEntitlementSnapshotFailed    MessageType = "DeviceEntitlementSnapshotFailed"
	MessageTypeDeviceOnlineChanged                MessageType = "DeviceOnlineChanged"
	MessageTypeDeviceMetadataChanged              MessageType = "DeviceMetadataChanged"
)

type OnlineStatus string

const (
	OnlineStatusOnline  OnlineStatus = "online"
	OnlineStatusOffline OnlineStatus = "offline"
)

type Envelope struct {
	MessageID     string          `json:"message_id"`
	CorrelationID string          `json:"correlation_id"`
	CausationID   string          `json:"causation_id,omitempty"`
	OperationID   string          `json:"operation_id"`
	SourceService string          `json:"source_service"`
	TargetService string          `json:"target_service"`
	MessageType   MessageType     `json:"message_type"`
	SchemaVersion string          `json:"schema_version"`
	PartitionKey  string          `json:"partition_key"`
	OccurredAt    time.Time       `json:"occurred_at"`
	Payload       json.RawMessage `json:"payload"`
}

func (e *Envelope) UnmarshalJSON(data []byte) error {
	type envelopeAlias Envelope

	var decoded envelopeAlias
	if err := decodeStrictJSON(data, &decoded); err != nil {
		return err
	}

	*e = Envelope(decoded)
	return nil
}

type Payload interface {
	Validate() error
	PartitionKey() string
}

type DeviceProvisionRequestedPayload struct {
	OrgID                  string   `json:"org_id"`
	AccountDeviceID        string   `json:"account_device_id"`
	VideoCloudDevid        string   `json:"video_cloud_devid"`
	ActivityID             string   `json:"activity_id"`
	ClipPublicKey          string   `json:"clip_public_key"`
	ServiceOptions         []string `json:"service_options,omitempty"`
	ProductID              string   `json:"product_id,omitempty"`
	ProductServiceRevision *int64   `json:"product_service_revision,omitempty"`
	ServiceGrantSHA256     string   `json:"service_grant_sha256,omitempty"`
	LogRetentionDays       *int     `json:"log_retention_days,omitempty"`
	TransferReservationID  string   `json:"transfer_reservation_id,omitempty"`
	RequestedBy            string   `json:"requested_by"`
}

type DeviceProvisionSucceededPayload struct {
	OrgID           string    `json:"org_id"`
	AccountDeviceID string    `json:"account_device_id"`
	VideoCloudDevid string    `json:"video_cloud_devid"`
	ActivityID      string    `json:"activity_id"`
	ActivatedAt     time.Time `json:"activated_at"`
}

type DeviceProvisionFailedPayload struct {
	OrgID           string    `json:"org_id"`
	AccountDeviceID string    `json:"account_device_id"`
	VideoCloudDevid string    `json:"video_cloud_devid"`
	ActivityID      string    `json:"activity_id"`
	ErrorCode       string    `json:"error_code"`
	ErrorMessage    string    `json:"error_message"`
	Retryable       bool      `json:"retryable"`
	FailedAt        time.Time `json:"failed_at"`
}

type DeviceDeactivateRequestedPayload struct {
	OrgID           string `json:"org_id"`
	AccountDeviceID string `json:"account_device_id"`
	VideoCloudDevid string `json:"video_cloud_devid"`
	ActivityID      string `json:"activity_id,omitempty"`
	RequestedBy     string `json:"requested_by"`
	Reason          string `json:"reason"`
}

type DeviceDeactivateSucceededPayload struct {
	OrgID           string    `json:"org_id"`
	AccountDeviceID string    `json:"account_device_id"`
	VideoCloudDevid string    `json:"video_cloud_devid"`
	ActivityID      string    `json:"activity_id,omitempty"`
	DeactivatedAt   time.Time `json:"deactivated_at"`
}

type DeviceDeactivateFailedPayload struct {
	OrgID           string    `json:"org_id"`
	AccountDeviceID string    `json:"account_device_id"`
	VideoCloudDevid string    `json:"video_cloud_devid"`
	ActivityID      string    `json:"activity_id,omitempty"`
	ErrorCode       string    `json:"error_code"`
	ErrorMessage    string    `json:"error_message"`
	Retryable       bool      `json:"retryable"`
	FailedAt        time.Time `json:"failed_at"`
}

type DeviceUnprovisionRequestedPayload struct {
	OrgID            string    `json:"org_id"`
	AccountDeviceID  string    `json:"account_device_id"`
	VideoCloudDevid  string    `json:"video_cloud_devid"`
	RequestedBy      string    `json:"requested_by"`
	Reason           string    `json:"reason"`
	PlatformOverride bool      `json:"platform_override"`
	UnprovisionedAt  time.Time `json:"unprovisioned_at"`
}

type DeviceUnprovisionSucceededPayload struct {
	OrgID           string    `json:"org_id"`
	AccountDeviceID string    `json:"account_device_id"`
	VideoCloudDevid string    `json:"video_cloud_devid"`
	UnprovisionedAt time.Time `json:"unprovisioned_at"`
}

type DeviceUnprovisionFailedPayload struct {
	OrgID           string    `json:"org_id"`
	AccountDeviceID string    `json:"account_device_id"`
	VideoCloudDevid string    `json:"video_cloud_devid"`
	ErrorCode       string    `json:"error_code"`
	ErrorMessage    string    `json:"error_message"`
	Retryable       bool      `json:"retryable"`
	FailedAt        time.Time `json:"failed_at"`
}

type DeviceEntitlementSnapshotRequestedPayload struct {
	OrgID                       string   `json:"org_id"`
	AccountDeviceID             string   `json:"account_device_id"`
	VideoCloudDevid             string   `json:"video_cloud_devid"`
	ProductID                   string   `json:"product_id"`
	ProductServiceRevision      int64    `json:"product_service_revision"`
	ServiceGrantSHA256          string   `json:"service_grant_sha256"`
	PlatformEntitlementRevision int64    `json:"platform_entitlement_revision"`
	ServiceOptions              []string `json:"service_options"`
	LogRetentionDays            *int     `json:"log_retention_days,omitempty"`
	State                       string   `json:"state"`
	RequestedBy                 string   `json:"requested_by"`
}

type DeviceEntitlementSnapshotSucceededPayload struct {
	OrgID                       string    `json:"org_id"`
	AccountDeviceID             string    `json:"account_device_id"`
	VideoCloudDevid             string    `json:"video_cloud_devid"`
	PlatformEntitlementRevision int64     `json:"platform_entitlement_revision"`
	AppliedAt                   time.Time `json:"applied_at"`
}

type DeviceEntitlementSnapshotFailedPayload struct {
	OrgID                       string    `json:"org_id"`
	AccountDeviceID             string    `json:"account_device_id"`
	VideoCloudDevid             string    `json:"video_cloud_devid"`
	PlatformEntitlementRevision int64     `json:"platform_entitlement_revision"`
	ErrorCode                   string    `json:"error_code"`
	ErrorMessage                string    `json:"error_message"`
	Retryable                   bool      `json:"retryable"`
	FailedAt                    time.Time `json:"failed_at"`
}

type DeviceOnlineChangedPayload struct {
	OrgID           string       `json:"org_id"`
	AccountDeviceID string       `json:"account_device_id"`
	VideoCloudDevid string       `json:"video_cloud_devid"`
	Status          OnlineStatus `json:"status"`
	LastSeenAt      time.Time    `json:"last_seen_at"`
}

type DeviceMetadataChangedPayload struct {
	OrgID           string         `json:"org_id"`
	AccountDeviceID string         `json:"account_device_id"`
	VideoCloudDevid string         `json:"video_cloud_devid"`
	Metadata        map[string]any `json:"metadata"`
}

type messageSpec struct {
	stream             string
	sourceService      string
	targetService      string
	requiredJSONFields []string
	newPayload         func() Payload
}

type Route struct {
	Stream        string
	SourceService string
	TargetService string
}

var messageSpecs = map[MessageType]messageSpec{
	MessageTypeDeviceProvisionRequested: {
		stream:        StreamAccountVideoCommands,
		sourceService: ServiceAccountManager,
		targetService: ServiceRealtekVideoCloud,
		newPayload: func() Payload {
			return &DeviceProvisionRequestedPayload{}
		},
	},
	MessageTypeDeviceProvisionSucceeded: {
		stream:        StreamVideoAccountEvents,
		sourceService: ServiceRealtekVideoCloud,
		targetService: ServiceAccountManager,
		newPayload: func() Payload {
			return &DeviceProvisionSucceededPayload{}
		},
	},
	MessageTypeDeviceProvisionFailed: {
		stream:             StreamVideoAccountEvents,
		sourceService:      ServiceRealtekVideoCloud,
		targetService:      ServiceAccountManager,
		requiredJSONFields: []string{"retryable"},
		newPayload: func() Payload {
			return &DeviceProvisionFailedPayload{}
		},
	},
	MessageTypeDeviceDeactivateRequested: {
		stream:        StreamAccountVideoCommands,
		sourceService: ServiceAccountManager,
		targetService: ServiceRealtekVideoCloud,
		newPayload: func() Payload {
			return &DeviceDeactivateRequestedPayload{}
		},
	},
	MessageTypeDeviceDeactivateSucceeded: {
		stream:        StreamVideoAccountEvents,
		sourceService: ServiceRealtekVideoCloud,
		targetService: ServiceAccountManager,
		newPayload: func() Payload {
			return &DeviceDeactivateSucceededPayload{}
		},
	},
	MessageTypeDeviceDeactivateFailed: {
		stream:             StreamVideoAccountEvents,
		sourceService:      ServiceRealtekVideoCloud,
		targetService:      ServiceAccountManager,
		requiredJSONFields: []string{"retryable"},
		newPayload: func() Payload {
			return &DeviceDeactivateFailedPayload{}
		},
	},
	MessageTypeDeviceUnprovisionRequested: {
		stream:             StreamAccountVideoCommands,
		sourceService:      ServiceAccountManager,
		targetService:      ServiceRealtekVideoCloud,
		requiredJSONFields: []string{"platform_override"},
		newPayload: func() Payload {
			return &DeviceUnprovisionRequestedPayload{}
		},
	},
	MessageTypeDeviceUnprovisionSucceeded: {
		stream:        StreamVideoAccountEvents,
		sourceService: ServiceRealtekVideoCloud,
		targetService: ServiceAccountManager,
		newPayload: func() Payload {
			return &DeviceUnprovisionSucceededPayload{}
		},
	},
	MessageTypeDeviceUnprovisionFailed: {
		stream:             StreamVideoAccountEvents,
		sourceService:      ServiceRealtekVideoCloud,
		targetService:      ServiceAccountManager,
		requiredJSONFields: []string{"retryable"},
		newPayload: func() Payload {
			return &DeviceUnprovisionFailedPayload{}
		},
	},
	MessageTypeDeviceEntitlementSnapshotRequested: {
		stream: StreamAccountVideoCommands, sourceService: ServiceAccountManager, targetService: ServiceRealtekVideoCloud,
		newPayload: func() Payload { return &DeviceEntitlementSnapshotRequestedPayload{} },
	},
	MessageTypeDeviceEntitlementSnapshotSucceeded: {
		stream: StreamVideoAccountEvents, sourceService: ServiceRealtekVideoCloud, targetService: ServiceAccountManager,
		newPayload: func() Payload { return &DeviceEntitlementSnapshotSucceededPayload{} },
	},
	MessageTypeDeviceEntitlementSnapshotFailed: {
		stream: StreamVideoAccountEvents, sourceService: ServiceRealtekVideoCloud, targetService: ServiceAccountManager,
		requiredJSONFields: []string{"retryable"},
		newPayload:         func() Payload { return &DeviceEntitlementSnapshotFailedPayload{} },
	},
	MessageTypeDeviceOnlineChanged: {
		stream:        StreamVideoAccountEvents,
		sourceService: ServiceRealtekVideoCloud,
		targetService: ServiceAccountManager,
		newPayload: func() Payload {
			return &DeviceOnlineChangedPayload{}
		},
	},
	MessageTypeDeviceMetadataChanged: {
		stream:        StreamVideoAccountEvents,
		sourceService: ServiceRealtekVideoCloud,
		targetService: ServiceAccountManager,
		newPayload: func() Payload {
			return &DeviceMetadataChangedPayload{}
		},
	},
}

func RouteForMessageType(messageType MessageType) (Route, error) {
	spec, ok := messageSpecs[messageType]
	if !ok {
		return Route{}, fmt.Errorf("unsupported message type %q", messageType)
	}
	return Route{
		Stream:        spec.stream,
		SourceService: spec.sourceService,
		TargetService: spec.targetService,
	}, nil
}

func (e Envelope) Validate(expectedStream string) error {
	_, err := e.ValidateAndDecode(expectedStream)
	return err
}

func (e Envelope) ValidateAndDecode(expectedStream string) (Payload, error) {
	if err := requireNonBlank("message_id", e.MessageID); err != nil {
		return nil, err
	}
	if err := requireNonBlank("correlation_id", e.CorrelationID); err != nil {
		return nil, err
	}
	if err := requireNonBlank("operation_id", e.OperationID); err != nil {
		return nil, err
	}
	if err := requireNonBlank("source_service", e.SourceService); err != nil {
		return nil, err
	}
	if err := requireNonBlank("target_service", e.TargetService); err != nil {
		return nil, err
	}
	if err := requireNonBlank("message_type", string(e.MessageType)); err != nil {
		return nil, err
	}
	if err := requireNonBlank("schema_version", e.SchemaVersion); err != nil {
		return nil, err
	}
	if err := requireNonBlank("partition_key", e.PartitionKey); err != nil {
		return nil, err
	}
	if e.OccurredAt.IsZero() {
		return nil, fieldError("occurred_at", "must be set")
	}
	if err := validateUTC("occurred_at", e.OccurredAt); err != nil {
		return nil, err
	}
	if len(e.Payload) == 0 {
		return nil, fieldError("payload", "must be set")
	}

	spec, ok := messageSpecs[e.MessageType]
	if !ok {
		return nil, fieldError("message_type", fmt.Sprintf("unsupported value %q", e.MessageType))
	}
	if e.SchemaVersion != SchemaVersionV1 {
		return nil, fieldError("schema_version", fmt.Sprintf("unsupported value %q", e.SchemaVersion))
	}
	if expectedStream != "" && expectedStream != spec.stream {
		return nil, fieldError("stream", fmt.Sprintf("message type %q must use %q", e.MessageType, spec.stream))
	}
	if e.SourceService != spec.sourceService {
		return nil, fieldError("source_service", fmt.Sprintf("message type %q must use %q", e.MessageType, spec.sourceService))
	}
	if e.TargetService != spec.targetService {
		return nil, fieldError("target_service", fmt.Sprintf("message type %q must use %q", e.MessageType, spec.targetService))
	}
	if err := requireJSONFields(e.Payload, "payload", spec.requiredJSONFields...); err != nil {
		return nil, err
	}

	payload := spec.newPayload()
	if err := decodeStrictJSON(e.Payload, payload); err != nil {
		return nil, fmt.Errorf("payload: %w", err)
	}
	if err := payload.Validate(); err != nil {
		return nil, err
	}
	if e.PartitionKey != payload.PartitionKey() {
		return nil, fieldError("partition_key", "must equal payload.account_device_id")
	}
	return payload, nil
}

func (p *DeviceProvisionRequestedPayload) Validate() error {
	if err := validateLifecyclePayloadIDs(p.OrgID, p.AccountDeviceID); err != nil {
		return err
	}
	if p.TransferReservationID != "" {
		if len(p.TransferReservationID) != 64 || p.TransferReservationID != strings.ToLower(p.TransferReservationID) {
			return fieldError("payload.transfer_reservation_id", "must be lowercase SHA-256 hex")
		}
		if _, err := hex.DecodeString(p.TransferReservationID); err != nil {
			return fieldError("payload.transfer_reservation_id", "must be lowercase SHA-256 hex")
		}
	}
	if err := validateServiceOptions("payload.service_options", p.ServiceOptions); err != nil {
		return err
	}
	if err := validateLogRetentionDays(p.ServiceOptions, p.LogRetentionDays); err != nil {
		return err
	}
	if p.ProductID != "" || p.ProductServiceRevision != nil || p.ServiceGrantSHA256 != "" {
		if err := requireUUID("payload.product_id", p.ProductID); err != nil {
			return err
		}
		if p.ProductServiceRevision == nil || *p.ProductServiceRevision < 1 {
			return fieldError("payload.product_service_revision", "must be positive")
		}
		if len(p.ServiceOptions) == 0 || len(p.ServiceGrantSHA256) != 64 || p.ServiceGrantSHA256 != strings.ToLower(p.ServiceGrantSHA256) {
			return fieldError("payload.service_grant_sha256", "requires services and a lowercase SHA-256 digest")
		}
		if _, err := hex.DecodeString(p.ServiceGrantSHA256); err != nil {
			return fieldError("payload.service_grant_sha256", "must be SHA-256 hex")
		}
	}

	return validateRequiredStrings(
		fieldValue{"payload.video_cloud_devid", p.VideoCloudDevid},
		fieldValue{"payload.activity_id", p.ActivityID},
		fieldValue{"payload.clip_public_key", p.ClipPublicKey},
		fieldValue{"payload.requested_by", p.RequestedBy},
	)
}

func (p *DeviceProvisionRequestedPayload) PartitionKey() string {
	return p.AccountDeviceID
}

func (p *DeviceProvisionSucceededPayload) Validate() error {
	if err := validateLifecyclePayloadIDs(p.OrgID, p.AccountDeviceID); err != nil {
		return err
	}
	if err := validateRequiredStrings(
		fieldValue{"payload.video_cloud_devid", p.VideoCloudDevid},
		fieldValue{"payload.activity_id", p.ActivityID},
	); err != nil {
		return err
	}
	if p.ActivatedAt.IsZero() {
		return fieldError("payload.activated_at", "must be set")
	}
	if err := validateUTC("payload.activated_at", p.ActivatedAt); err != nil {
		return err
	}
	return nil
}

func (p *DeviceProvisionSucceededPayload) PartitionKey() string {
	return p.AccountDeviceID
}

func (p *DeviceProvisionFailedPayload) Validate() error {
	if err := validateLifecyclePayloadIDs(p.OrgID, p.AccountDeviceID); err != nil {
		return err
	}
	if err := validateRequiredStrings(
		fieldValue{"payload.video_cloud_devid", p.VideoCloudDevid},
		fieldValue{"payload.activity_id", p.ActivityID},
		fieldValue{"payload.error_code", p.ErrorCode},
		fieldValue{"payload.error_message", p.ErrorMessage},
	); err != nil {
		return err
	}
	if p.FailedAt.IsZero() {
		return fieldError("payload.failed_at", "must be set")
	}
	if err := validateUTC("payload.failed_at", p.FailedAt); err != nil {
		return err
	}
	return nil
}

func (p *DeviceProvisionFailedPayload) PartitionKey() string {
	return p.AccountDeviceID
}

func (p *DeviceDeactivateRequestedPayload) Validate() error {
	if err := validateLifecyclePayloadIDs(p.OrgID, p.AccountDeviceID); err != nil {
		return err
	}

	return validateRequiredStrings(
		fieldValue{"payload.video_cloud_devid", p.VideoCloudDevid},
		fieldValue{"payload.requested_by", p.RequestedBy},
		fieldValue{"payload.reason", p.Reason},
	)
}

func (p *DeviceDeactivateRequestedPayload) PartitionKey() string {
	return p.AccountDeviceID
}

func (p *DeviceDeactivateSucceededPayload) Validate() error {
	if err := validateLifecyclePayloadIDs(p.OrgID, p.AccountDeviceID); err != nil {
		return err
	}
	if err := validateRequiredStrings(
		fieldValue{"payload.video_cloud_devid", p.VideoCloudDevid},
	); err != nil {
		return err
	}
	if p.DeactivatedAt.IsZero() {
		return fieldError("payload.deactivated_at", "must be set")
	}
	if err := validateUTC("payload.deactivated_at", p.DeactivatedAt); err != nil {
		return err
	}
	return nil
}

func (p *DeviceDeactivateSucceededPayload) PartitionKey() string {
	return p.AccountDeviceID
}

func (p *DeviceDeactivateFailedPayload) Validate() error {
	if err := validateLifecyclePayloadIDs(p.OrgID, p.AccountDeviceID); err != nil {
		return err
	}
	if err := validateRequiredStrings(
		fieldValue{"payload.video_cloud_devid", p.VideoCloudDevid},
		fieldValue{"payload.error_code", p.ErrorCode},
		fieldValue{"payload.error_message", p.ErrorMessage},
	); err != nil {
		return err
	}
	if p.FailedAt.IsZero() {
		return fieldError("payload.failed_at", "must be set")
	}
	if err := validateUTC("payload.failed_at", p.FailedAt); err != nil {
		return err
	}
	return nil
}

func (p *DeviceDeactivateFailedPayload) PartitionKey() string {
	return p.AccountDeviceID
}

func (p *DeviceUnprovisionRequestedPayload) Validate() error {
	if err := validateLifecyclePayloadIDs(p.OrgID, p.AccountDeviceID); err != nil {
		return err
	}
	if err := validateRequiredStrings(
		fieldValue{"payload.video_cloud_devid", p.VideoCloudDevid},
		fieldValue{"payload.requested_by", p.RequestedBy},
		fieldValue{"payload.reason", p.Reason},
	); err != nil {
		return err
	}
	if p.UnprovisionedAt.IsZero() {
		return fieldError("payload.unprovisioned_at", "must be set")
	}
	if err := validateUTC("payload.unprovisioned_at", p.UnprovisionedAt); err != nil {
		return err
	}
	return nil
}

func (p *DeviceUnprovisionRequestedPayload) PartitionKey() string {
	return p.AccountDeviceID
}

func (p *DeviceUnprovisionSucceededPayload) Validate() error {
	if err := validateLifecyclePayloadIDs(p.OrgID, p.AccountDeviceID); err != nil {
		return err
	}
	if err := validateRequiredStrings(
		fieldValue{"payload.video_cloud_devid", p.VideoCloudDevid},
	); err != nil {
		return err
	}
	if p.UnprovisionedAt.IsZero() {
		return fieldError("payload.unprovisioned_at", "must be set")
	}
	if err := validateUTC("payload.unprovisioned_at", p.UnprovisionedAt); err != nil {
		return err
	}
	return nil
}

func (p *DeviceUnprovisionSucceededPayload) PartitionKey() string {
	return p.AccountDeviceID
}

func (p *DeviceUnprovisionFailedPayload) Validate() error {
	if err := validateLifecyclePayloadIDs(p.OrgID, p.AccountDeviceID); err != nil {
		return err
	}
	if err := validateRequiredStrings(
		fieldValue{"payload.video_cloud_devid", p.VideoCloudDevid},
		fieldValue{"payload.error_code", p.ErrorCode},
		fieldValue{"payload.error_message", p.ErrorMessage},
	); err != nil {
		return err
	}
	if p.FailedAt.IsZero() {
		return fieldError("payload.failed_at", "must be set")
	}
	if err := validateUTC("payload.failed_at", p.FailedAt); err != nil {
		return err
	}
	return nil
}

func (p *DeviceUnprovisionFailedPayload) PartitionKey() string {
	return p.AccountDeviceID
}

func (p *DeviceEntitlementSnapshotRequestedPayload) Validate() error {
	if err := validateLifecyclePayloadIDs(p.OrgID, p.AccountDeviceID); err != nil {
		return err
	}
	if err := requireUUID("payload.product_id", p.ProductID); err != nil {
		return err
	}
	if err := validateRequiredStrings(
		fieldValue{"payload.video_cloud_devid", p.VideoCloudDevid},
		fieldValue{"payload.requested_by", p.RequestedBy},
	); err != nil {
		return err
	}
	if p.ProductServiceRevision < 1 || p.PlatformEntitlementRevision < 1 {
		return fieldError("payload.platform_entitlement_revision", "revisions must be positive")
	}
	if len(p.ServiceGrantSHA256) != 64 || strings.ToLower(p.ServiceGrantSHA256) != p.ServiceGrantSHA256 {
		return fieldError("payload.service_grant_sha256", "must be SHA-256 hex")
	}
	if _, err := hex.DecodeString(p.ServiceGrantSHA256); err != nil {
		return fieldError("payload.service_grant_sha256", "must be SHA-256 hex")
	}
	if len(p.ServiceOptions) == 0 {
		return fieldError("payload.service_options", "must be set")
	}
	if err := validateServiceOptions("payload.service_options", p.ServiceOptions); err != nil {
		return err
	}
	if err := validateLogRetentionDays(p.ServiceOptions, p.LogRetentionDays); err != nil {
		return err
	}
	if p.State != "active" && p.State != "suspended" && p.State != "revoked" {
		return fieldError("payload.state", "unsupported entitlement state")
	}
	return nil
}

func (p *DeviceEntitlementSnapshotRequestedPayload) PartitionKey() string { return p.AccountDeviceID }

func validateLogRetentionDays(options []string, days *int) error {
	if days == nil {
		return nil
	} // Historical messages did not carry pinned settings.
	if *days != 7 && *days != 30 && *days != 90 || !slices.Contains(options, "device_logging") {
		return fieldError("payload.log_retention_days", "requires device_logging and 7, 30, or 90 days")
	}
	return nil
}

func (p *DeviceEntitlementSnapshotSucceededPayload) Validate() error {
	if err := validateLifecyclePayloadIDs(p.OrgID, p.AccountDeviceID); err != nil {
		return err
	}
	if err := requireNonBlank("payload.video_cloud_devid", p.VideoCloudDevid); err != nil {
		return err
	}
	if p.PlatformEntitlementRevision < 1 || p.AppliedAt.IsZero() {
		return fieldError("payload.platform_entitlement_revision", "revision and applied_at are required")
	}
	return validateUTC("payload.applied_at", p.AppliedAt)
}

func (p *DeviceEntitlementSnapshotSucceededPayload) PartitionKey() string { return p.AccountDeviceID }

func (p *DeviceEntitlementSnapshotFailedPayload) Validate() error {
	if err := validateLifecyclePayloadIDs(p.OrgID, p.AccountDeviceID); err != nil {
		return err
	}
	if err := validateRequiredStrings(
		fieldValue{"payload.video_cloud_devid", p.VideoCloudDevid},
		fieldValue{"payload.error_code", p.ErrorCode},
		fieldValue{"payload.error_message", p.ErrorMessage},
	); err != nil {
		return err
	}
	if p.PlatformEntitlementRevision < 1 || p.FailedAt.IsZero() {
		return fieldError("payload.platform_entitlement_revision", "revision and failed_at are required")
	}
	return validateUTC("payload.failed_at", p.FailedAt)
}

func (p *DeviceEntitlementSnapshotFailedPayload) PartitionKey() string { return p.AccountDeviceID }

func (p *DeviceOnlineChangedPayload) Validate() error {
	if err := validateLifecyclePayloadIDs(p.OrgID, p.AccountDeviceID); err != nil {
		return err
	}
	if err := validateRequiredStrings(
		fieldValue{"payload.video_cloud_devid", p.VideoCloudDevid},
	); err != nil {
		return err
	}
	if p.Status != OnlineStatusOnline && p.Status != OnlineStatusOffline {
		return fieldError("payload.status", fmt.Sprintf("unsupported value %q", p.Status))
	}
	if p.LastSeenAt.IsZero() {
		return fieldError("payload.last_seen_at", "must be set")
	}
	if err := validateUTC("payload.last_seen_at", p.LastSeenAt); err != nil {
		return err
	}
	return nil
}

func (p *DeviceOnlineChangedPayload) PartitionKey() string {
	return p.AccountDeviceID
}

func (p *DeviceMetadataChangedPayload) Validate() error {
	if err := validateLifecyclePayloadIDs(p.OrgID, p.AccountDeviceID); err != nil {
		return err
	}
	if err := validateRequiredStrings(
		fieldValue{"payload.video_cloud_devid", p.VideoCloudDevid},
	); err != nil {
		return err
	}
	if p.Metadata == nil {
		return fieldError("payload.metadata", "must be set")
	}
	return nil
}

func (p *DeviceMetadataChangedPayload) PartitionKey() string {
	return p.AccountDeviceID
}

type fieldValue struct {
	name  string
	value string
}

func validateRequiredStrings(fields ...fieldValue) error {
	for _, field := range fields {
		if err := requireNonBlank(field.name, field.value); err != nil {
			return err
		}
	}
	return nil
}

func validateLifecyclePayloadIDs(orgID, accountDeviceID string) error {
	if err := requireUUID("payload.org_id", orgID); err != nil {
		return err
	}
	if err := requireUUID("payload.account_device_id", accountDeviceID); err != nil {
		return err
	}
	return nil
}

func requireNonBlank(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return fieldError(field, "must be non-empty")
	}
	return nil
}

func requireUUID(field, value string) error {
	if err := requireNonBlank(field, value); err != nil {
		return err
	}
	if !isUUID(value) {
		return fieldError(field, "must be a UUID")
	}
	return nil
}

func validateUTC(field string, value time.Time) error {
	_, offset := value.Zone()
	if offset != 0 {
		return fieldError(field, "must use UTC")
	}
	return nil
}

func validateServiceOptions(field string, options []string) error {
	if len(options) > 64 {
		return fieldError(field, "must not exceed 64 options")
	}
	seen := map[string]struct{}{}
	for _, option := range options {
		if !serviceOptionCodePattern.MatchString(option) {
			return fieldError(field, "must contain valid service option codes")
		}
		if _, ok := seen[option]; ok {
			return fieldError(field, "must not contain duplicates")
		}
		seen[option] = struct{}{}
	}
	return nil
}

func decodeStrictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(target); err != nil {
		return err
	}

	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("must contain a single JSON value")
	}

	return nil
}

func requireJSONFields(data []byte, prefix string, fields ...string) error {
	if len(fields) == 0 {
		return nil
	}

	var decoded map[string]json.RawMessage
	if err := decodeStrictJSON(data, &decoded); err != nil {
		return err
	}

	for _, field := range fields {
		if _, ok := decoded[field]; !ok {
			return fieldError(prefix+"."+field, "must be set")
		}
	}

	return nil
}

func isUUID(value string) bool {
	if len(value) != 36 {
		return false
	}

	segments := [5]string{
		value[0:8],
		value[9:13],
		value[14:18],
		value[19:23],
		value[24:36],
	}
	if value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for _, segment := range segments {
		if _, err := hex.DecodeString(segment); err != nil {
			return false
		}
	}
	return true
}

func fieldError(field, message string) error {
	return errors.New(field + " " + message)
}
