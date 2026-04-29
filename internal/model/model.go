package model

import "time"

type Role string

const (
	RoleOwner  Role = "owner"
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
)

type DeviceCategory string

const (
	DeviceCategoryIPCamera DeviceCategory = "ip_camera"
	DeviceCategoryMQTT     DeviceCategory = "mqtt_device"
	DeviceCategoryGeneric  DeviceCategory = "generic"
)

type DeviceStatus string

const (
	DeviceStatusUnknown  DeviceStatus = "unknown"
	DeviceStatusOnline   DeviceStatus = "online"
	DeviceStatusOffline  DeviceStatus = "offline"
	DeviceStatusDisabled DeviceStatus = "disabled"
)

type VideoCloudActivationStatus string

const (
	VideoCloudActivationStatusPending     VideoCloudActivationStatus = "pending"
	VideoCloudActivationStatusActivated   VideoCloudActivationStatus = "activated"
	VideoCloudActivationStatusFailed      VideoCloudActivationStatus = "failed"
	VideoCloudActivationStatusDeactivated VideoCloudActivationStatus = "deactivated"
)

const (
	DeviceMetadataVideoCloudDevid            = "video_cloud_devid"
	DeviceMetadataVideoCloudActivationStatus = "video_cloud_activation_status"
	DeviceMetadataVideoCloudActivityID       = "video_cloud_activity_id"
	DeviceMetadataVideoCloudActivatedAt      = "video_cloud_activated_at"
	DeviceMetadataVideoCloudDeactivatedAt    = "video_cloud_deactivated_at"
	DeviceMetadataVideoCloudLastError        = "video_cloud_last_error"
)

type DeviceOperationType string

const (
	DeviceOperationTypeProvision  DeviceOperationType = "provision"
	DeviceOperationTypeDeactivate DeviceOperationType = "deactivate"
)

type DeviceOperationStatus string

const (
	DeviceOperationStatusPending      DeviceOperationStatus = "pending"
	DeviceOperationStatusPublished    DeviceOperationStatus = "published"
	DeviceOperationStatusSucceeded    DeviceOperationStatus = "succeeded"
	DeviceOperationStatusFailed       DeviceOperationStatus = "failed"
	DeviceOperationStatusRetrying     DeviceOperationStatus = "retrying"
	DeviceOperationStatusDeadLettered DeviceOperationStatus = "dead_lettered"
)

type DeviceMessageOutboxStatus string

const (
	DeviceMessageOutboxStatusPending      DeviceMessageOutboxStatus = "pending"
	DeviceMessageOutboxStatusPublished    DeviceMessageOutboxStatus = "published"
	DeviceMessageOutboxStatusRetrying     DeviceMessageOutboxStatus = "retrying"
	DeviceMessageOutboxStatusDeadLettered DeviceMessageOutboxStatus = "dead_lettered"
)

type DeviceMessageInboxStatus string

const (
	DeviceMessageInboxStatusProcessed    DeviceMessageInboxStatus = "processed"
	DeviceMessageInboxStatusFailed       DeviceMessageInboxStatus = "failed"
	DeviceMessageInboxStatusRetrying     DeviceMessageInboxStatus = "retrying"
	DeviceMessageInboxStatusDeadLettered DeviceMessageInboxStatus = "dead_lettered"
)

type User struct {
	ID          string     `json:"id"`
	Email       string     `json:"email"`
	DisplayName *string    `json:"display_name,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	DisabledAt  *time.Time `json:"disabled_at,omitempty"`
}

type Organization struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Role      Role      `json:"role,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Member struct {
	OrganizationID string     `json:"organization_id"`
	UserID         string     `json:"user_id"`
	Email          string     `json:"email"`
	DisplayName    *string    `json:"display_name,omitempty"`
	Role           Role       `json:"role"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	DisabledAt     *time.Time `json:"disabled_at,omitempty"`
}

type Device struct {
	ID             string         `json:"id"`
	OrganizationID string         `json:"organization_id"`
	Name           string         `json:"name"`
	Category       DeviceCategory `json:"category"`
	SerialNumber   *string        `json:"serial_number,omitempty"`
	MACAddress     *string        `json:"mac_address,omitempty"`
	Manufacturer   *string        `json:"manufacturer,omitempty"`
	Model          *string        `json:"model,omitempty"`
	Status         DeviceStatus   `json:"status"`
	LastSeenAt     *time.Time     `json:"last_seen_at,omitempty"`
	Metadata       map[string]any `json:"metadata"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DisabledAt     *time.Time     `json:"disabled_at,omitempty"`
}

type DeviceOperation struct {
	ID             string                `json:"id"`
	OperationID    string                `json:"operation_id"`
	CorrelationID  string                `json:"correlation_id"`
	OrganizationID string                `json:"organization_id"`
	DeviceID       string                `json:"device_id"`
	OperationType  DeviceOperationType   `json:"operation_type"`
	Status         DeviceOperationStatus `json:"status"`
	RequestedBy    *string               `json:"requested_by,omitempty"`
	RequestPayload map[string]any        `json:"request_payload"`
	ResultPayload  map[string]any        `json:"result_payload"`
	ErrorCode      *string               `json:"error_code,omitempty"`
	ErrorMessage   *string               `json:"error_message,omitempty"`
	Retryable      *bool                 `json:"retryable,omitempty"`
	CreatedAt      time.Time             `json:"created_at"`
	UpdatedAt      time.Time             `json:"updated_at"`
	CompletedAt    *time.Time            `json:"completed_at,omitempty"`
}

type DeviceMessageOutbox struct {
	ID            string                    `json:"id"`
	MessageID     string                    `json:"message_id"`
	OperationID   string                    `json:"operation_id"`
	CorrelationID string                    `json:"correlation_id"`
	CausationID   *string                   `json:"causation_id,omitempty"`
	Stream        string                    `json:"stream"`
	MessageType   string                    `json:"message_type"`
	SchemaVersion string                    `json:"schema_version"`
	PartitionKey  string                    `json:"partition_key"`
	Payload       map[string]any            `json:"payload"`
	Status        DeviceMessageOutboxStatus `json:"status"`
	AttemptCount  int                       `json:"attempt_count"`
	LastError     *string                   `json:"last_error,omitempty"`
	AvailableAt   time.Time                 `json:"available_at"`
	PublishedAt   *time.Time                `json:"published_at,omitempty"`
	CreatedAt     time.Time                 `json:"created_at"`
	UpdatedAt     time.Time                 `json:"updated_at"`
}

type DeviceMessageInbox struct {
	ID            string                   `json:"id"`
	MessageID     string                   `json:"message_id"`
	OperationID   string                   `json:"operation_id"`
	CorrelationID string                   `json:"correlation_id"`
	CausationID   *string                  `json:"causation_id,omitempty"`
	Stream        string                   `json:"stream"`
	MessageType   string                   `json:"message_type"`
	SchemaVersion string                   `json:"schema_version"`
	PartitionKey  string                   `json:"partition_key"`
	Payload       map[string]any           `json:"payload"`
	Status        DeviceMessageInboxStatus `json:"status"`
	AttemptCount  int                      `json:"attempt_count"`
	LastError     *string                  `json:"last_error,omitempty"`
	ReceivedAt    time.Time                `json:"received_at"`
	ProcessedAt   *time.Time               `json:"processed_at,omitempty"`
	CreatedAt     time.Time                `json:"created_at"`
	UpdatedAt     time.Time                `json:"updated_at"`
}
