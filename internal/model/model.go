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
