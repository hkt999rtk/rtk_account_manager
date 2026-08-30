package model

import "time"

type Role string

const (
	RoleOwner  Role = "owner"
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
)

type Permission struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Domain      string    `json:"domain"`
	Action      string    `json:"action"`
	Description *string   `json:"description,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type ProductRole struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	ScopeType   string     `json:"scope_type"`
	Description *string    `json:"description,omitempty"`
	SystemRole  bool       `json:"system_role"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	DisabledAt  *time.Time `json:"disabled_at,omitempty"`
}

type RoleAssignment struct {
	ID             string     `json:"id"`
	RoleID         string     `json:"role_id"`
	RoleName       string     `json:"role_name"`
	ActorType      string     `json:"actor_type"`
	ActorID        string     `json:"actor_id"`
	ScopeType      string     `json:"scope_type"`
	ScopeID        *string    `json:"scope_id,omitempty"`
	OrganizationID *string    `json:"organization_id,omitempty"`
	CreatedBy      *string    `json:"created_by,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	DisabledAt     *time.Time `json:"disabled_at,omitempty"`
}

type ExternalGroupMapping struct {
	ID             string     `json:"id"`
	ProviderID     string     `json:"provider_id"`
	ExternalGroup  string     `json:"external_group"`
	RoleID         string     `json:"role_id"`
	RoleName       string     `json:"role_name"`
	ScopeType      string     `json:"scope_type"`
	ScopeID        *string    `json:"scope_id,omitempty"`
	OrganizationID *string    `json:"organization_id,omitempty"`
	CreatedBy      *string    `json:"created_by,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	DisabledAt     *time.Time `json:"disabled_at,omitempty"`
}

type ACLAuditEvent struct {
	ID             string         `json:"id"`
	EventType      string         `json:"event_type"`
	ActorUserID    *string        `json:"actor_user_id,omitempty"`
	OrganizationID *string        `json:"organization_id,omitempty"`
	SubjectType    string         `json:"subject_type"`
	SubjectID      string         `json:"subject_id"`
	Payload        map[string]any `json:"payload"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

type OrganizationTier string

const (
	OrganizationTierEvaluation OrganizationTier = "evaluation"
	OrganizationTierCommercial OrganizationTier = "commercial"
)

type OrganizationKind string

const (
	OrganizationKindCustomerOrg OrganizationKind = "customer_org"
	OrganizationKindBrandCloud  OrganizationKind = "brand_cloud"
)

type OrganizationStatus string

const (
	OrganizationStatusActive   OrganizationStatus = "active"
	OrganizationStatusDisabled OrganizationStatus = "disabled"
)

type QuotaRaiseRequestStatus string

const (
	QuotaRaiseRequestStatusPending  QuotaRaiseRequestStatus = "pending"
	QuotaRaiseRequestStatusApproved QuotaRaiseRequestStatus = "approved"
	QuotaRaiseRequestStatusDeclined QuotaRaiseRequestStatus = "declined"
)

type AuditEvent struct {
	ID             string         `json:"id"`
	EventType      string         `json:"event_type"`
	ActorUserID    *string        `json:"actor_user_id,omitempty"`
	OrganizationID *string        `json:"organization_id,omitempty"`
	SubjectType    string         `json:"subject_type"`
	SubjectID      string         `json:"subject_id"`
	Payload        map[string]any `json:"payload"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

type IdentityProviderType string

const (
	IdentityProviderTypeOIDC IdentityProviderType = "oidc"
)

type IdentityProvider struct {
	ID              string               `json:"id"`
	ProviderID      string               `json:"provider_id"`
	Name            string               `json:"name"`
	Type            IdentityProviderType `json:"type"`
	IssuerURL       string               `json:"issuer_url"`
	ClientID        string               `json:"client_id"`
	ClientSecretRef *string              `json:"client_secret_ref,omitempty"`
	Scopes          []string             `json:"scopes"`
	Enabled         bool                 `json:"enabled"`
	Metadata        map[string]any       `json:"metadata"`
	CreatedAt       time.Time            `json:"created_at"`
	UpdatedAt       time.Time            `json:"updated_at"`
}

type UserIdentity struct {
	ID            string         `json:"id"`
	UserID        string         `json:"user_id"`
	ProviderID    string         `json:"provider_id"`
	ProviderKey   string         `json:"provider_key"`
	IssuerURL     string         `json:"issuer_url"`
	Subject       string         `json:"subject"`
	Email         string         `json:"email"`
	EmailVerified bool           `json:"email_verified"`
	Claims        map[string]any `json:"claims"`
	LinkedAt      time.Time      `json:"linked_at"`
	LastLoginAt   *time.Time     `json:"last_login_at,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
}

type EndUser struct {
	ID           string     `json:"id"`
	PrimaryEmail string     `json:"email"`
	DisplayName  *string    `json:"display_name,omitempty"`
	Status       string     `json:"status"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	DisabledAt   *time.Time `json:"disabled_at,omitempty"`
}

type BrandCloudEndUser struct {
	BrandCloudID string         `json:"brand_cloud_id"`
	EndUserID    string         `json:"end_user_id"`
	DisplayAlias *string        `json:"display_alias,omitempty"`
	Status       string         `json:"status"`
	Consent      map[string]any `json:"consent"`
	FirstSeenAt  time.Time      `json:"first_seen_at"`
	LastSeenAt   time.Time      `json:"last_seen_at"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
}

type DeviceUserBinding struct {
	ID                 string     `json:"id"`
	DeviceID           string     `json:"device_id"`
	BrandCloudID       string     `json:"brand_cloud_id"`
	EndUserID          string     `json:"end_user_id"`
	Role               string     `json:"role"`
	CreatedFromClaimID *string    `json:"created_from_claim_id,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
	DisabledAt         *time.Time `json:"disabled_at,omitempty"`
}

type AppCertificate struct {
	ID                  string     `json:"id"`
	UserID              string     `json:"user_id"`
	SubjectType         string     `json:"subject_type,omitempty"`
	SubjectID           string     `json:"subject_id,omitempty"`
	Subject             string     `json:"subject"`
	CSRSHA256           string     `json:"csr_sha256"`
	CertificatePEM      string     `json:"certificate_pem"`
	CertificateChainPEM string     `json:"certificate_chain_pem"`
	FingerprintSHA256   string     `json:"fingerprint_sha256"`
	SerialNumber        string     `json:"serial_number"`
	IssuerRequestID     string     `json:"issuer_request_id"`
	NotBefore           time.Time  `json:"not_before"`
	NotAfter            time.Time  `json:"not_after"`
	RevokedAt           *time.Time `json:"revoked_at,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

type OIDCLoginState struct {
	ID                   string     `json:"id"`
	ProviderID           string     `json:"provider_id"`
	StateHash            string     `json:"state_hash"`
	NonceHash            string     `json:"nonce_hash"`
	RedirectURL          string     `json:"redirect_url"`
	PostLoginRedirectURL *string    `json:"post_login_redirect_url,omitempty"`
	ExpiresAt            time.Time  `json:"expires_at"`
	ConsumedAt           *time.Time `json:"consumed_at,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
}

type DeviceClaimToken struct {
	ID                  string         `json:"id"`
	OrganizationID      *string        `json:"organization_id,omitempty"`
	CreatedBy           *string        `json:"created_by,omitempty"`
	DeviceItemProfileID *string        `json:"device_item_profile_id,omitempty"`
	Category            DeviceCategory `json:"category"`
	VideoCloudDevid     string         `json:"video_cloud_devid"`
	ActivityID          string         `json:"activity_id"`
	ClipPublicKey       string         `json:"clip_public_key"`
	ServiceOptions      []string       `json:"service_options"`
	Metadata            map[string]any `json:"metadata"`
	Notes               *string        `json:"notes,omitempty"`
	ExpiresAt           time.Time      `json:"expires_at"`
	ClaimedAt           *time.Time     `json:"claimed_at,omitempty"`
	RevokedAt           *time.Time     `json:"revoked_at,omitempty"`
	CreatedAt           time.Time      `json:"created_at"`
	UpdatedAt           time.Time      `json:"updated_at"`
}

type DeviceItemProfileStatus string

const (
	DeviceItemProfileStatusActive   DeviceItemProfileStatus = "active"
	DeviceItemProfileStatusDisabled DeviceItemProfileStatus = "disabled"
)

type DeviceItemProfile struct {
	ID                 string                  `json:"id"`
	BrandCloudID       string                  `json:"brand_cloud_id"`
	ProfileKey         string                  `json:"profile_key"`
	DisplayName        string                  `json:"display_name"`
	Status             DeviceItemProfileStatus `json:"status"`
	Category           DeviceCategory          `json:"category"`
	Manufacturer       *string                 `json:"manufacturer,omitempty"`
	Model              *string                 `json:"model,omitempty"`
	MetadataDefaults   map[string]any          `json:"metadata_defaults"`
	MetadataSchema     map[string]any          `json:"metadata_schema"`
	CAProfile          string                  `json:"ca_profile"`
	IssuerProfile      string                  `json:"issuer_profile"`
	ServiceOptions     []string                `json:"service_options"`
	ClaimPolicy        map[string]any          `json:"claim_policy"`
	ProvisioningPolicy map[string]any          `json:"provisioning_policy"`
	DisabledAt         *time.Time              `json:"disabled_at,omitempty"`
	CreatedAt          time.Time               `json:"created_at"`
	UpdatedAt          time.Time               `json:"updated_at"`
	CurrentUserRole    string                  `json:"current_user_role,omitempty"`
}

type ProductionRunStatus string

const (
	ProductionRunStatusActive   ProductionRunStatus = "active"
	ProductionRunStatusDisabled ProductionRunStatus = "disabled"
)

type ProductionRun struct {
	ID                  string              `json:"id"`
	BrandCloudID        string              `json:"brand_cloud_id"`
	DeviceItemProfileID string              `json:"device_item_profile_id"`
	FactoryID           string              `json:"factory_id,omitempty"`
	BatchID             string              `json:"batch_id,omitempty"`
	Status              ProductionRunStatus `json:"status"`
	AllowedQuantity     int                 `json:"allowed_quantity"`
	IssuedQuantity      int                 `json:"issued_quantity"`
	ValidFrom           time.Time           `json:"valid_from"`
	ValidUntil          time.Time           `json:"valid_until"`
	CreatedBy           *string             `json:"created_by,omitempty"`
	CreatedAt           time.Time           `json:"created_at"`
	UpdatedAt           time.Time           `json:"updated_at"`
}

type DeviceClaim struct {
	ID             string         `json:"id"`
	TokenID        string         `json:"claim_token_id"`
	OrganizationID string         `json:"organization_id"`
	DeviceID       string         `json:"device_id"`
	ClaimedBy      string         `json:"claimed_by"`
	Status         string         `json:"status"`
	ProvisionInput map[string]any `json:"provision_input"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

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

type DeviceReadinessState string

const (
	DeviceReadinessStateActivationPending   DeviceReadinessState = "activation_pending"
	DeviceReadinessStateActivationFailed    DeviceReadinessState = "activation_failed"
	DeviceReadinessStateTransportPending    DeviceReadinessState = "transport_pending"
	DeviceReadinessStateReady               DeviceReadinessState = "ready"
	DeviceReadinessStateDeactivationPending DeviceReadinessState = "deactivation_pending"
	DeviceReadinessStateDeactivationFailed  DeviceReadinessState = "deactivation_failed"
	DeviceReadinessStateDeactivated         DeviceReadinessState = "deactivated"
	DeviceReadinessStateDisabled            DeviceReadinessState = "disabled"
)

type ProductReadinessState string

const (
	ProductReadinessStateRegistered             ProductReadinessState = "registered"
	ProductReadinessStateClaimPending           ProductReadinessState = "claim_pending"
	ProductReadinessStateLocalOnboardingPending ProductReadinessState = "local_onboarding_pending"
	ProductReadinessStateCloudActivationPending ProductReadinessState = "cloud_activation_pending"
	ProductReadinessStateActivated              ProductReadinessState = "activated"
	ProductReadinessStateOnline                 ProductReadinessState = "online"
	ProductReadinessStateFailed                 ProductReadinessState = "failed"
	ProductReadinessStateDeactivationPending    ProductReadinessState = "deactivation_pending"
	ProductReadinessStateDeactivated            ProductReadinessState = "deactivated"
)

const (
	DeviceMetadataVideoCloudDevid            = "video_cloud_devid"
	DeviceMetadataVideoCloudActivationStatus = "video_cloud_activation_status"
	DeviceMetadataVideoCloudActivityID       = "video_cloud_activity_id"
	DeviceMetadataVideoCloudActivatedAt      = "video_cloud_activated_at"
	DeviceMetadataVideoCloudDeactivatedAt    = "video_cloud_deactivated_at"
	DeviceMetadataVideoCloudClipPublicKey    = "video_cloud_clip_public_key"
	DeviceMetadataVideoCloudLastError        = "video_cloud_last_error"
	DeviceMetadataServiceOptions             = "service_options"
)

type DeviceOperationType string

const (
	DeviceOperationTypeProvision   DeviceOperationType = "provision"
	DeviceOperationTypeDeactivate  DeviceOperationType = "deactivate"
	DeviceOperationTypeUnprovision DeviceOperationType = "unprovision"
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
	ID                        string     `json:"id"`
	Email                     string     `json:"email"`
	DisplayName               *string    `json:"display_name,omitempty"`
	EmailVerified             bool       `json:"email_verified"`
	EmailVerifiedAt           *time.Time `json:"email_verified_at,omitempty"`
	SignupPendingVerification bool       `json:"signup_pending_verification"`
	DeveloperCloudLimit       int        `json:"developer_cloud_limit"`
	CreatedAt                 time.Time  `json:"created_at"`
	UpdatedAt                 time.Time  `json:"updated_at"`
	DisabledAt                *time.Time `json:"disabled_at,omitempty"`
}

type Organization struct {
	ID                    string             `json:"id"`
	Name                  string             `json:"name"`
	TenantSlug            *string            `json:"tenant_slug,omitempty"`
	Role                  Role               `json:"role,omitempty"`
	Capabilities          []string           `json:"capabilities,omitempty"`
	OrganizationKind      OrganizationKind   `json:"organization_kind"`
	Status                OrganizationStatus `json:"status"`
	Tier                  OrganizationTier   `json:"tier"`
	EvaluationDeviceQuota int                `json:"evaluation_device_quota"`
	Metadata              map[string]any     `json:"metadata,omitempty"`
	CreatedAt             time.Time          `json:"created_at"`
	UpdatedAt             time.Time          `json:"updated_at"`
}

type Member struct {
	OrganizationID string     `json:"organization_id"`
	UserID         string     `json:"user_id"`
	Email          string     `json:"email"`
	DisplayName    *string    `json:"display_name,omitempty"`
	Role           Role       `json:"role"`
	Capabilities   []string   `json:"capabilities,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	DisabledAt     *time.Time `json:"disabled_at,omitempty"`
}

type BrandCloudAccountListItem struct {
	Member
	EmailVerified             bool `json:"email_verified"`
	SignupPendingVerification bool `json:"signup_pending_verification"`
}

type BrandCloudUser struct {
	ID                        string     `json:"id"`
	BrandCloudID              string     `json:"brand_cloud_id"`
	Email                     string     `json:"email"`
	DisplayName               *string    `json:"display_name,omitempty"`
	EmailVerified             bool       `json:"email_verified"`
	EmailVerifiedAt           *time.Time `json:"email_verified_at,omitempty"`
	SignupPendingVerification bool       `json:"signup_pending_verification"`
	CreatedAt                 time.Time  `json:"created_at"`
	UpdatedAt                 time.Time  `json:"updated_at"`
	DisabledAt                *time.Time `json:"disabled_at,omitempty"`
}

type BrandCloudMember struct {
	BrandCloudID     string    `json:"brand_cloud_id"`
	BrandCloudUserID string    `json:"brand_cloud_user_id"`
	Email            string    `json:"email,omitempty"`
	DisplayName      *string   `json:"display_name,omitempty"`
	Role             Role      `json:"role"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type BrandCloudOwnerTransfer struct {
	ID                string     `json:"id"`
	BrandCloudID      string     `json:"brand_cloud_id"`
	RequestedByUserID string     `json:"requested_by_user_id"`
	TargetUserID      string     `json:"target_user_id"`
	TargetEmail       string     `json:"target_email,omitempty"`
	Status            string     `json:"status"`
	ExpiresAt         time.Time  `json:"expires_at"`
	AcceptedAt        *time.Time `json:"accepted_at,omitempty"`
	CanceledAt        *time.Time `json:"canceled_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

type BrandCloudMemberInvitation struct {
	ID              string     `json:"id"`
	BrandCloudID    string     `json:"brand_cloud_id"`
	InvitedByUserID string     `json:"invited_by_user_id"`
	TargetUserID    string     `json:"target_user_id"`
	TargetEmail     string     `json:"target_email"`
	Role            Role       `json:"role"`
	Status          string     `json:"status"`
	ExpiresAt       time.Time  `json:"expires_at"`
	AcceptedAt      *time.Time `json:"accepted_at,omitempty"`
	CanceledAt      *time.Time `json:"canceled_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

type ProductCollaborator struct {
	AssignmentID string     `json:"assignment_id"`
	ProductID    string     `json:"product_id"`
	UserID       string     `json:"user_id"`
	Email        string     `json:"email"`
	DisplayName  *string    `json:"display_name,omitempty"`
	Role         string     `json:"role"`
	DisabledAt   *time.Time `json:"disabled_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

type ProductCollaboratorInvitation struct {
	ID              string     `json:"id"`
	BrandCloudID    string     `json:"brand_cloud_id"`
	ProductID       string     `json:"product_id"`
	InvitedByUserID string     `json:"invited_by_user_id"`
	TargetUserID    string     `json:"target_user_id"`
	TargetEmail     string     `json:"target_email"`
	Role            string     `json:"role"`
	Status          string     `json:"status"`
	ExpiresAt       time.Time  `json:"expires_at"`
	AcceptedAt      *time.Time `json:"accepted_at,omitempty"`
	CanceledAt      *time.Time `json:"canceled_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

type Device struct {
	ID                  string         `json:"id"`
	OrganizationID      string         `json:"organization_id"`
	DeviceItemProfileID *string        `json:"device_item_profile_id,omitempty"`
	Name                string         `json:"name"`
	Category            DeviceCategory `json:"category"`
	SerialNumber        *string        `json:"serial_number,omitempty"`
	MACAddress          *string        `json:"mac_address,omitempty"`
	Manufacturer        *string        `json:"manufacturer,omitempty"`
	Model               *string        `json:"model,omitempty"`
	Status              DeviceStatus   `json:"status"`
	LastSeenAt          *time.Time     `json:"last_seen_at,omitempty"`
	Metadata            map[string]any `json:"metadata"`
	CreatedAt           time.Time      `json:"created_at"`
	UpdatedAt           time.Time      `json:"updated_at"`
	DisabledAt          *time.Time     `json:"disabled_at,omitempty"`
}

type DeviceGroup struct {
	ID             string    `json:"id"`
	OrganizationID string    `json:"organization_id"`
	Name           string    `json:"name"`
	Description    *string   `json:"description,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	DeviceCount    *int      `json:"device_count,omitempty"`
}

type DeviceTag struct {
	OrganizationID string    `json:"organization_id"`
	DeviceID       string    `json:"device_id"`
	Tag            string    `json:"tag"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type QuotaRaiseRequest struct {
	ID             string                  `json:"id"`
	OrganizationID string                  `json:"organization_id"`
	RequestedBy    string                  `json:"requested_by"`
	RequestedQuota int                     `json:"requested_quota"`
	UseCase        string                  `json:"use_case"`
	ContactInfo    map[string]any          `json:"contact_info"`
	Status         QuotaRaiseRequestStatus `json:"status"`
	DecidedBy      *string                 `json:"decided_by,omitempty"`
	DecisionReason *string                 `json:"decision_reason,omitempty"`
	CreatedAt      time.Time               `json:"created_at"`
	UpdatedAt      time.Time               `json:"updated_at"`
	DecidedAt      *time.Time              `json:"decided_at,omitempty"`
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
