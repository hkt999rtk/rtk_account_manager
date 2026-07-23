package api

import (
	"context"
	"time"

	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

type Store interface {
	authPersistence
	userPersistence
	organizationPersistence
	memberPersistence
	devicePersistence
	deviceGroupPersistence
	deviceTagPersistence
	appCertificatePersistence
	provisioningPersistence
	deviceClaimPersistence
	metricsPersistence
	evaluationPersistence
	aclPersistence
	identityProviderPersistence
	brandCloudPersistence
	auditPersistence
	chipsetProviderPersistence
}

type authPersistence interface {
	Register(context.Context, store.RegisterInput) (store.RegisterResult, error)
	SignupDeveloper(context.Context, store.DeveloperSignupInput) (store.DeveloperSignupResult, error)
	GetUserPassword(ctx context.Context, email string) (model.User, string, error)
	GetUserPasswordByID(ctx context.Context, userID string) (model.User, string, error)
	CreateEmailVerificationToken(ctx context.Context, userID, tokenHash string, expiresAt time.Time) error
	CreatePasswordResetToken(ctx context.Context, userID, tokenHash string, expiresAt time.Time) error
	CreateLoginActivationToken(ctx context.Context, userID, tokenHash string, expiresAt time.Time) error
	CreatePasswordResetTokenForEmail(ctx context.Context, email, tokenHash string, expiresAt time.Time) (bool, error)
	CreateEmailVerificationTokenForEmail(ctx context.Context, email, tokenHash string, expiresAt time.Time) (bool, error)
	CreateLoginActivationTokenForEmail(ctx context.Context, email, tokenHash string, expiresAt time.Time) (bool, error)
	VerifyEmailToken(ctx context.Context, tokenHash string) (model.User, error)
	ActivateLoginToken(ctx context.Context, tokenHash string) (model.User, error)
	ResetPasswordWithToken(ctx context.Context, tokenHash, passwordHash string) error
	UpdateUserPassword(ctx context.Context, userID, passwordHash string) error
	SaveRefreshToken(ctx context.Context, userID, tokenHash string, expiresAt time.Time) error
	RotateRefreshToken(ctx context.Context, oldTokenHash, newTokenHash, userID string, newExpiresAt time.Time) error
	RevokeRefreshToken(ctx context.Context, tokenHash string) error
	GetBrandCloudUserPassword(ctx context.Context, tenantSlug, email string) (store.BrandCloudLoginResult, error)
	CreateBrandCloudLoginActivationTokenForEmail(ctx context.Context, tenantSlug, email, tokenHash string, expiresAt time.Time) (bool, error)
	ActivateBrandCloudLoginToken(ctx context.Context, tenantSlug, tokenHash string) (store.BrandCloudLoginResult, error)
	GetBrandCloudUser(ctx context.Context, brandCloudUserID string) (model.BrandCloudUser, error)
	GetBrandCloudMember(ctx context.Context, brandCloudID, brandCloudUserID string) (model.BrandCloudMember, error)
	SaveBrandCloudRefreshToken(ctx context.Context, brandCloudUserID, brandCloudID, tokenHash string, expiresAt time.Time) error
	RotateBrandCloudRefreshToken(ctx context.Context, oldTokenHash, newTokenHash, brandCloudUserID, brandCloudID string, newExpiresAt time.Time) error
	RevokeBrandCloudRefreshToken(ctx context.Context, tokenHash string) error
	CreateEndUser(ctx context.Context, in store.EndUserCreateInput) (model.EndUser, error)
	GetEndUserPassword(ctx context.Context, email string) (store.EndUserLoginResult, error)
	GetEndUser(ctx context.Context, endUserID string) (model.EndUser, error)
	SaveEndUserRefreshToken(ctx context.Context, endUserID, tokenHash string, expiresAt time.Time) error
	RotateEndUserRefreshToken(ctx context.Context, oldTokenHash, newTokenHash, endUserID string, newExpiresAt time.Time) error
	RevokeEndUserRefreshToken(ctx context.Context, tokenHash string) error
}

type userPersistence interface {
	GetUser(ctx context.Context, userID string) (model.User, error)
	GetUserByEmail(ctx context.Context, email string) (model.User, error)
	DisableCurrentUser(ctx context.Context, userID string) error
}

type organizationPersistence interface {
	ListOrganizations(ctx context.Context, userID string, limit, offset int) (store.OrganizationPage, error)
	CreateOrganization(ctx context.Context, userID, name string) (model.Organization, error)
	GetOrganization(ctx context.Context, orgID, userID string) (model.Organization, error)
	UpdateOrganization(ctx context.Context, orgID, userID, name string) (model.Organization, error)
}

type memberPersistence interface {
	GetRole(ctx context.Context, orgID, userID string) (model.Role, error)
	GetDeveloperBrandCloudMember(ctx context.Context, brandCloudID, userID string) (model.Member, error)
	ListMembers(ctx context.Context, orgID string, limit, offset int) (store.MemberPage, error)
	ListDeveloperBrandCloudMembers(ctx context.Context, brandCloudID string, limit, offset int) (store.MemberPage, error)
	AddMember(ctx context.Context, orgID, email string, role model.Role) (model.Member, error)
	UpdateMemberRole(ctx context.Context, orgID, userID string, role model.Role) (model.Member, error)
	DisableMemberUser(ctx context.Context, orgID, userID string) (model.Member, error)
	EnableMemberUser(ctx context.Context, orgID, userID string) (model.Member, error)
	RemoveMember(ctx context.Context, orgID, userID string) error
	DisableDeveloperBrandCloudMember(ctx context.Context, brandCloudID, userID string) (model.Member, error)
	EnableDeveloperBrandCloudMember(ctx context.Context, brandCloudID, userID string) (model.Member, error)
}

type devicePersistence interface {
	CreateDevice(ctx context.Context, orgID string, in store.DeviceInput) (model.Device, error)
	ListDevices(ctx context.Context, orgID string, limit, offset int) (store.DevicePage, error)
	ListDevicesFiltered(ctx context.Context, in store.DeviceListFilter) (store.DevicePage, error)
	FleetSummary(ctx context.Context, orgID string) (store.FleetSummary, error)
	FleetSummaryForBrandCloudUser(ctx context.Context, orgID, brandCloudUserID string) (store.FleetSummary, error)
	GetDevice(ctx context.Context, orgID, deviceID string) (model.Device, error)
	UpdateDevice(ctx context.Context, orgID, deviceID string, in store.DeviceInput) (model.Device, error)
	DeleteDevice(ctx context.Context, orgID, deviceID string) error
	UpdateDeviceStatus(ctx context.Context, orgID, deviceID string, status model.DeviceStatus, lastSeenAt *time.Time) (model.Device, error)
}

type deviceGroupPersistence interface {
	CreateDeviceGroup(ctx context.Context, orgID string, in store.DeviceGroupInput) (model.DeviceGroup, error)
	ListDeviceGroups(ctx context.Context, orgID string, limit, offset int) (store.DeviceGroupPage, error)
	GetDeviceGroup(ctx context.Context, orgID, groupID string) (model.DeviceGroup, error)
	UpdateDeviceGroup(ctx context.Context, orgID, groupID string, in store.DeviceGroupInput) (model.DeviceGroup, error)
	DeleteDeviceGroup(ctx context.Context, orgID, groupID string) error
	AddDeviceToGroup(ctx context.Context, orgID, groupID, deviceID string) error
	RemoveDeviceFromGroup(ctx context.Context, orgID, groupID, deviceID string) error
	ListDeviceGroupDevices(ctx context.Context, orgID, groupID string, limit, offset int) (store.DevicePage, error)
}

type deviceTagPersistence interface {
	AddDeviceTag(ctx context.Context, orgID, deviceID, tag string) (model.DeviceTag, error)
	DeleteDeviceTag(ctx context.Context, orgID, deviceID, tag string) error
	ListDeviceTags(ctx context.Context, orgID, deviceID string, limit, offset int) (store.DeviceTagPage, error)
	ListOrganizationTags(ctx context.Context, orgID string, limit, offset int) (store.DeviceTagSummaryPage, error)
}

type appCertificatePersistence interface {
	GetValidAppCertificateForUser(ctx context.Context, userID string, now time.Time) (model.AppCertificate, error)
	GetValidAppCertificateForSubject(ctx context.Context, subjectType, subjectID string, now time.Time) (model.AppCertificate, error)
	CreateAppCertificate(ctx context.Context, in store.AppCertificateCreateInput) (model.AppCertificate, error)
	RevokeValidAppCertificatesForBrandCloudUser(ctx context.Context, brandCloudID, brandCloudUserID string) (int64, error)
}

type provisioningPersistence interface {
	StartDeviceLifecycleOperation(ctx context.Context, in store.DeviceLifecycleOperationInput) (store.DeviceLifecycleOperationResult, error)
	StartDeviceDeactivationOperation(ctx context.Context, in store.DeviceDeactivationOperationInput) (store.DeviceLifecycleOperationResult, error)
	GetDeviceOperation(ctx context.Context, operationID string) (model.DeviceOperation, error)
	GetLatestDeviceOperationByType(ctx context.Context, orgID, deviceID string, operationType model.DeviceOperationType) (model.DeviceOperation, error)
	GetLatestOutboxMessageByOperationID(ctx context.Context, operationID string) (model.DeviceMessageOutbox, error)
	CreateOrGetInboxMessage(ctx context.Context, in store.DeviceMessageInboxCreateInput) (model.DeviceMessageInbox, bool, error)
	RecordInboxProcessTransition(ctx context.Context, in store.InboxProcessTransitionInput) (store.InboxProcessTransitionResult, error)
	UnprovisionDevice(ctx context.Context, in store.DeviceUnprovisionInput) (store.DeviceUnprovisionResult, error)
}

type deviceClaimPersistence interface {
	CreateDeviceClaimToken(ctx context.Context, in store.DeviceClaimTokenCreateInput) (model.DeviceClaimToken, error)
	ListDeviceClaimTokens(ctx context.Context, in store.DeviceClaimTokenListFilter) (store.DeviceClaimTokenPage, error)
	GetDeviceClaimToken(ctx context.Context, tokenID string) (model.DeviceClaimToken, error)
	RevokeDeviceClaimToken(ctx context.Context, tokenID string, now time.Time) (model.DeviceClaimToken, error)
	ResolveDeviceClaimToken(ctx context.Context, in store.DeviceClaimResolveInput) (store.DeviceClaimResolveResult, error)
	ResolveEndUserDeviceClaimToken(ctx context.Context, in store.EndUserDeviceClaimResolveInput) (store.EndUserDeviceClaimResolveResult, error)
	TransferDeviceClaim(ctx context.Context, in store.DeviceClaimTransferInput) (store.DeviceClaimOverrideResult, error)
	ReclaimDeviceClaimToken(ctx context.Context, in store.DeviceClaimReclaimInput) (store.DeviceClaimOverrideResult, error)
	AuthorizeUserForVideoDevice(ctx context.Context, userID, videoCloudDevid string) error
	AuthorizeBrandCloudUserForVideoDevice(ctx context.Context, brandCloudUserID, videoCloudDevid string) error
	AuthorizeEndUserForVideoDevice(ctx context.Context, endUserID, videoCloudDevid string) error
}

type metricsPersistence interface {
	CountEvaluationSignupEvents(ctx context.Context) (evaluation int64, commercial int64, err error)
	CountEmailVerificationEventsFromSignup(ctx context.Context) (int64, error)
	CountQuotaRaiseRequestStatuses(ctx context.Context) (pending, approved, declined int64, err error)
	ListEvaluationQuotaUsage(ctx context.Context) ([]store.EvaluationQuotaUsage, error)
	GetLifecycleMetrics(ctx context.Context) (store.LifecycleMetrics, error)
}

type evaluationPersistence interface {
	IsPlatformAdmin(ctx context.Context, userID string) (bool, error)
	CreateQuotaRaiseRequest(ctx context.Context, in store.QuotaRaiseRequestInput) (model.QuotaRaiseRequest, error)
	GetQuotaRaiseRequest(ctx context.Context, requestID string) (model.QuotaRaiseRequest, error)
	ListQuotaRaiseRequests(ctx context.Context, in store.QuotaRaiseRequestListFilter) (store.QuotaRaiseRequestPage, error)
	DecideQuotaRaiseRequest(ctx context.Context, in store.QuotaRaiseDecisionInput) (model.QuotaRaiseRequest, model.Organization, model.User, error)
}

type aclPersistence interface {
	HasPermission(ctx context.Context, userID, orgID, permission string) (bool, error)
	ListUserPlatformPermissions(ctx context.Context, userID string) ([]string, error)
	HasBrandCloudPermission(ctx context.Context, brandCloudUserID, orgID, permission string) (bool, error)
	HasBrandCloudPermissionForResource(ctx context.Context, brandCloudUserID, orgID, permission, scopeType, scopeID string) (bool, error)
	HasBrandCloudPermissionAnyResource(ctx context.Context, brandCloudUserID, orgID, permission string) (bool, error)
	HasBrandCloudDevicePermission(ctx context.Context, brandCloudUserID, orgID, permission, deviceID string) (bool, error)
	ListPermissions(ctx context.Context, limit, offset int) (store.PermissionPage, error)
	GetRoleByName(ctx context.Context, name string) (model.ProductRole, error)
	UpdateRole(ctx context.Context, in store.RoleUpdateInput) (model.ProductRole, error)
	DisableRole(ctx context.Context, name string, actorUserID *string) error
	ListRoles(ctx context.Context, limit, offset int) (store.ProductRolePage, error)
	CreateRole(ctx context.Context, in store.RoleCreateInput) (model.ProductRole, error)
	BindRolePermission(ctx context.Context, roleName, permissionName string, actorUserID *string) error
	CreateRoleAssignment(ctx context.Context, in store.RoleAssignmentCreateInput) (model.RoleAssignment, error)
	ListRoleAssignments(ctx context.Context, limit, offset int) (store.RoleAssignmentPage, error)
	ListRoleAssignmentsForOrganization(ctx context.Context, organizationID string, limit, offset int) (store.RoleAssignmentPage, error)
	DisableRoleAssignment(ctx context.Context, assignmentID string, actorUserID *string) error
	DisableRoleAssignmentForOrganization(ctx context.Context, organizationID, assignmentID string, actorUserID *string) error
	CreateExternalGroupMapping(ctx context.Context, in store.ExternalGroupMappingCreateInput) (model.ExternalGroupMapping, error)
	ListExternalGroupMappings(ctx context.Context, limit, offset int) (store.ExternalGroupMappingPage, error)
	DisableExternalGroupMapping(ctx context.Context, mappingID string, actorUserID *string) error
	ApplyExternalGroupMappings(ctx context.Context, userID, providerID string, groups []string, now time.Time) error
	ListACLAuditEvents(ctx context.Context, in store.ACLAuditEventListFilter) (store.ACLAuditEventPage, error)
}

type identityProviderPersistence interface {
	CreateIdentityProvider(ctx context.Context, in store.IdentityProviderCreateInput) (model.IdentityProvider, error)
	ListIdentityProviders(ctx context.Context, in store.IdentityProviderListFilter) (store.IdentityProviderPage, error)
	GetIdentityProviderByProviderID(ctx context.Context, providerID string) (model.IdentityProvider, error)
	GetEnabledIdentityProvider(ctx context.Context) (model.IdentityProvider, error)
	UpdateIdentityProvider(ctx context.Context, in store.IdentityProviderUpdateInput) (model.IdentityProvider, error)
	DisableIdentityProvider(ctx context.Context, providerID string, now time.Time) (model.IdentityProvider, error)
	CreateUserIdentity(ctx context.Context, in store.UserIdentityCreateInput) (model.UserIdentity, error)
	ListUserIdentities(ctx context.Context, userID string) ([]model.UserIdentity, error)
	GetUserIdentityByProviderSubject(ctx context.Context, providerID, subject string) (model.UserIdentity, error)
	UpdateUserIdentityLastLogin(ctx context.Context, identityID string, lastLoginAt time.Time) (model.UserIdentity, error)
	DeleteUserIdentity(ctx context.Context, userID, identityID string) error
	CreateOIDCLoginState(ctx context.Context, in store.OIDCLoginStateCreateInput) (model.OIDCLoginState, error)
	ConsumeOIDCLoginState(ctx context.Context, stateHash string, now time.Time) (model.OIDCLoginState, error)
}

type brandCloudPersistence interface {
	CreateBrandCloud(ctx context.Context, actorUserID string, in store.BrandCloudInput) (model.Organization, error)
	CreateDeveloperBrandCloud(ctx context.Context, userID string, in store.BrandCloudInput) (model.Organization, error)
	ListDeveloperBrandClouds(ctx context.Context, userID string, limit, offset int) (store.OrganizationPage, error)
	CreateBrandCloudOwnerTransfer(ctx context.Context, in store.BrandCloudOwnerTransferInput) (model.BrandCloudOwnerTransfer, error)
	AcceptBrandCloudOwnerTransfer(ctx context.Context, targetUserID, tokenHash string, now time.Time) (model.BrandCloudOwnerTransfer, error)
	GetBrandCloudOwnerTransfer(ctx context.Context, in store.BrandCloudOwnerTransferQuery, now time.Time) (model.BrandCloudOwnerTransfer, error)
	CancelBrandCloudOwnerTransfer(ctx context.Context, in store.BrandCloudOwnerTransferQuery, now time.Time) (model.BrandCloudOwnerTransfer, error)
	ListBrandClouds(ctx context.Context, limit, offset int) (store.OrganizationPage, error)
	GetBrandCloud(ctx context.Context, orgID string) (model.Organization, error)
	GetBrandCloudByTenantSlug(ctx context.Context, tenantSlug string) (model.Organization, error)
	UpdateBrandCloud(ctx context.Context, actorUserID, orgID string, in store.BrandCloudInput) (model.Organization, error)
	AssignBrandCloudMember(ctx context.Context, actorUserID, orgID, brandCloudUserID string, role model.Role) (model.BrandCloudMember, error)
	CreateBrandCloudUser(ctx context.Context, actorUserID, orgID string, in store.BrandCloudUserInput) (store.BrandCloudUserResult, error)
	ListBrandCloudUsers(ctx context.Context, in store.BrandCloudUserListFilter) (store.BrandCloudUserPage, error)
	DisableBrandCloudUser(ctx context.Context, actorUserID, brandCloudID, brandCloudUserID string) (model.BrandCloudUser, error)
	EnableBrandCloudUser(ctx context.Context, actorUserID, brandCloudID, brandCloudUserID string) (model.BrandCloudUser, error)
	ApproveBrandCloudUser(ctx context.Context, actorUserID, brandCloudID, brandCloudUserID string) (model.BrandCloudUser, error)
	DeleteBrandCloudUser(ctx context.Context, actorUserID, brandCloudID, brandCloudUserID string) error
	CreateDeviceItemProfile(ctx context.Context, in store.DeviceItemProfileCreateInput) (model.DeviceItemProfile, error)
	ListDeviceItemProfiles(ctx context.Context, in store.DeviceItemProfileListFilter) (store.DeviceItemProfilePage, error)
	GetDeviceItemProfile(ctx context.Context, brandCloudID, profileID string) (model.DeviceItemProfile, error)
	UpdateDeviceItemProfile(ctx context.Context, in store.DeviceItemProfileUpdateInput) (model.DeviceItemProfile, error)
	DisableDeviceItemProfile(ctx context.Context, brandCloudID, profileID string, actorUserID *string) (model.DeviceItemProfile, error)
	CreateProductionRun(ctx context.Context, in store.ProductionRunCreateInput) (model.ProductionRun, error)
	ListProductionRuns(ctx context.Context, brandCloudID, profileID string, limit, offset int) (store.ProductionRunPage, error)
}

type auditPersistence interface {
	CreateAuditEvent(ctx context.Context, in store.AuditEventInput) error
	ListAuditEvents(ctx context.Context, in store.AuditEventListFilter) (store.AuditEventPage, error)
}

type chipsetProviderPersistence interface {
	CreateChipsetProvider(context.Context, store.ChipsetProviderWriteInput) (model.ChipsetProvider, error)
	UpdateChipsetProvider(context.Context, string, store.ChipsetProviderWriteInput) (model.ChipsetProvider, error)
	GetChipsetProvider(context.Context, string) (model.ChipsetProvider, []model.DeveloperChipset, error)
	ListChipsetProviders(context.Context) ([]model.ChipsetProvider, error)
	ListPublishedChipsets(context.Context) ([]model.DeveloperChipset, error)
	CommitChipsetProviderRefresh(context.Context, store.ChipsetProviderRefreshInput) (model.ChipsetProvider, error)
	MarkChipsetProviderNotModified(context.Context, string, time.Time) (model.ChipsetProvider, error)
	MarkChipsetProviderRefreshFailed(context.Context, string, string, time.Time) (model.ChipsetProvider, error)
	SetChipsetProviderStatus(context.Context, string, model.ChipsetProviderStatus) (model.ChipsetProvider, error)
}
