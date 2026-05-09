# Test Report

Generated: ci-candidate

## Summary

| Check | Result |
| --- | --- |
| Overall | PASS |
| Formatting | PASS |
| Tests | PASS |
| Build | PASS |
| Coverage threshold | PASS |
| Correctness gates | PASS |

## Coverage

| Metric | Value |
| --- | --- |
| Total statement coverage | recorded in reports/coverage.txt |
| Minimum required coverage | 80.0% |
| Coverage mode | atomic |
| Coverage scope | ./internal/... |

## Test Execution

| Metric | Value |
| --- | --- |
| Go packages | 20 |
| Test cases started | recorded in reports/test-events.json |
| JSON pass events | recorded in reports/test-events.json |
| JSON fail events | recorded in reports/test-events.json |
| Integration database | Postgres via TEST_DATABASE_URL |

## Correctness Gates

`make test-report` fails when any required behavior group below is missing a passing representative test. These gates protect the maintained report from drifting away from the executable suite.

| Behavior group | Required test | Result |
| --- | --- | --- |
| Auth and sessions | `TestIntegrationRegisterLoginRefreshAndLogout` | PASS |
| Disabled users | `TestIntegrationDisabledUserCannotUseExistingTokens` | PASS |
| Organization access | `TestIntegrationOwnerCanUpdateOrganization` | PASS |
| Member management | `TestIntegrationLastOwnerCannotBeRemovedOrDowngraded` | PASS |
| Device lifecycle | `TestIntegrationRoleAuthorizationDeviceScopeAndSerialUniqueness` | PASS |
| Authorization and tenancy matrix | `TestIntegrationAuthorizationAndTenancyMatrix` | PASS |
| Provisioning API | `TestIntegrationProvisioningEndpoints` | PASS |
| Claim Token persistence | `TestResolveDeviceClaimTokenCreatesDeviceAndClaim` | PASS |
| Claim Token admin workflow | `TestDeviceClaimTokenAdminLifecycle` | PASS |
| Claim resolve API | `TestIntegrationClaimResolveEndpoint` | PASS |
| Deactivation API | `TestIntegrationDeactivateEndpointUsesProjectedVideoMetadata` | PASS |
| Message validation | `TestValidateRejectsEnvelopeContractMismatches` | PASS |
| Contract parser fuzz seeds | `FuzzEnvelopeStrictJSONAndValidation` | PASS |
| Strict API bind fuzz seeds | `FuzzBindStrictRequestShape` | PASS |
| Outbox worker | `TestRunOnceMarksSuccessfulPublishes` | PASS |
| Inbox worker | `TestRunOnceSkipsPreviouslyProcessedDuplicates` | PASS |
| Projection idempotency and metadata merge | `TestApplyProjectionMetadataPreservesExistingFieldsAndClearsNil` | PASS |
| Account readiness projection | `TestReadinessFromProjectionStates` | PASS |
| Registry-only readiness | `TestIntegrationProvisioningStateReturnsRegistryOnlyReadiness` | PASS |
| Admin quota and audit visibility | `TestIntegrationSignupEvaluationQuotaAndRaiseWorkflow` | PASS |
| Broker adapters | `TestAzureEventHubsPublisherPublishesJSONRecord` | PASS |
| Database invariants | `TestIntegrationDatabaseSchemaInvariants` | PASS |
| OpenAPI contract | `TestIntegrationResponsesMatchOpenAPIContract` | PASS |
| Configuration and maintenance | `TestLoadReadsEnvironmentAndDurations` | PASS |

## Correctness Validation

Coverage is only a signal that code executed. Correctness is validated by assertions in the automated tests. This report confirms the following behavior groups were exercised:

| Behavior group | Evidence |
| --- | --- |
| Auth and sessions | Register, login, invalid login, password change with current-password validation, refresh-token revocation after password change, refresh rotation, old refresh rejection, logout revocation, expired token parsing, wrong-secret parsing. |
| Disabled users | Disabled users cannot use existing access tokens, refresh tokens, or login until re-enabled. |
| Organization access | Current-user organization listing, organization create/get/update, cross-organization organization access rejection. |
| Member management | Owner add/update/remove/disable/enable member flows, admin/member forbidden paths, last-owner downgrade/remove/disable protection. |
| Device lifecycle | Device create/list/get/update/status update/soft-delete, disabled-device read-only behavior, duplicate serial rejection, same serial in another org allowed. |
| Authorization and tenancy matrix | `TestIntegrationAuthorizationAndTenancyMatrix` verifies owner/admin/member/platform-admin/outsider/disabled-user behavior across device reads/writes, claim resolve, provisioning, deactivation, quota visibility, audit visibility, and foreign organization access. |
| Provisioning API | `TestIntegrationProvisioningEndpoints` verifies owner/admin initiation, member read-only access, raw claim-material rejection, transactional `device_operations` plus `device_message_outbox` writes, projected command payload shape, account-side readiness source facts, disabled-device rejection, and idempotent `operation_id` reuse. |
| Claim Token persistence | `TestResolveDeviceClaimTokenCreatesDeviceAndClaim`, `TestResolveDeviceClaimTokenMatchesExistingDevice`, `TestResolveDeviceClaimTokenRejectsInvalidToken`, `TestResolveDeviceClaimTokenRejectsExpiredToken`, `TestResolveDeviceClaimTokenRejectsAlreadyClaimedToken`, `TestResolveDeviceClaimTokenRejectsCrossOrganizationToken`, and `TestResolveDeviceClaimTokenRejectsUnsupportedCategory` verify account-manager-owned Claim Token storage, raw-token non-persistence, hashed-token lookup, expiry, idempotent ownership matching, category policy, and organization boundaries. |
| Claim Token admin workflow | `TestDeviceClaimTokenAdminLifecycle` and `TestIntegrationAdminDeviceClaimTokenWorkflow` verify platform-admin token creation/import/list/show/revoke, raw-token non-persistence, generated raw-token one-time return, platform-admin-only access, and revoked-token resolve rejection. |
| Claim resolve API | `TestIntegrationClaimResolveEndpoint` verifies owner/admin claim resolution, member rejection, invalid/expired/already-claimed/cross-organization/unsupported-category/quota error codes, returned provisioning input, and that resolve does not create provisioning operations or outbox messages. |
| Claim resolve retryability | `TestWriteClaimResolveErrorIncludesRetryability` and `TestIntegrationClaimResolveEndpoint` verify machine-readable `retryable` and `resolution_action` hints for non-retryable policy failures, quota failures, and retryable service-unavailable errors. |
| Deactivation API | `TestIntegrationDeactivateEndpointUsesProjectedVideoMetadata` verifies projected metadata is required for new deactivation work, disabled account devices may still enqueue deactivation, default reason propagation, and transactional outbox creation from projected video metadata. |
| Device scoping | Lifecycle endpoints reject cross-organization reads and writes without leaking foreign device access. |
| Authorization boundaries | owner/admin/member role permissions are enforced across device CRUD, provisioning, deactivation, and member management paths. |
| Operation idempotency | Reusing the same lifecycle `operation_id` returns the existing operation and preserves the original outbox `message_id`, including retries after device disablement or missing live metadata. |
| Operation conflicts | Reusing a lifecycle `operation_id` with conflicting provision activity data or deactivation reason returns `409 Conflict`. |
| Message validation | Envelope fields, supported `schema_version`, message-type/stream/service pairing, lifecycle UUIDs, UTC timestamps, and `partition_key` validation for cross-service messages. |
| Contract parser fuzz seeds | `FuzzEnvelopeStrictJSONAndValidation` and `FuzzBindStrictRequestShape` run seeded malformed JSON, unknown-field, wrong stream/message type, wrong partition key, and strict bind request-shape cases during normal `go test`. |
| Outbox worker | `TestRunOnceMarksSuccessfulPublishes`, `TestRunOnceSchedulesTransientRetry`, `TestRunOnceDeadLettersExhaustedPublishFailures`, `TestRunOnceIgnoresStaleLeaseTransitionConflict`, and `TestRunOnceIgnoresConflictWhenRetryLosesToPublished` verify publish success, retry, dead-letter, and stale-lease conflict handling when another worker already won the publish race. |
| Outbox publish race recovery | `TestRecordOutboxPublishTransitionRejectsStaleLease`, `TestRecordOutboxPublishTransitionLetsPublishedOutcomeOverrideLaterFailure`, and `TestRecordOutboxPublishTransitionPreservesInboxCompletedOperation` verify stale workers cannot roll back a published outbox row or overwrite an inbox-completed device operation. |
| Inbox worker | `TestRunOnceSkipsPreviouslyProcessedDuplicates`, `TestRunOnceDeadLettersInvalidMessages`, and `TestRunOnceRetriesTransientProjectionFailures` verify message-id dedupe, dead-lettering, and transient projection retry behavior. |
| Inbox replay guards and dead-letter payloads | `TestRunOnceSkipsCompletedLifecycleReplayWithNewMessageID`, `TestRunOnceSkipsCompletedLifecycleReplayForRetryingMessage`, `TestRunOnceDeadLettersMalformedAndUnmappedMessages/malformed_payload_keeps_inspectable_inbox_row`, and `TestCreateOrGetInboxMessagePreservesDeadLetterPayloadSnapshot` verify terminal lifecycle replays stay side-effect free and malformed payload bytes remain inspectable in persisted inbox rows. |
| Projection idempotency and metadata merge | `TestRunOnceProcessesProvisionSuccess`, `TestRunOnceProcessesFailureAndProjectionEvents`, `TestApplyProjectionMetadataPreservesExistingFieldsAndClearsNil`, and `TestMetadataChangedProjectionFiltersNonVideoCloudKeys` cover replay-safe projection and selective `video_cloud_*` metadata updates. |
| Activation and online projection | `TestProjectDeviceProvisioningAndOnlineRules` proves provisioning success does not set account-manager `status=online`, while `DeviceOnlineChanged` remains the only event that updates `status` and `last_seen_at`. |
| Account readiness projection | `TestReadinessFromProjectionStates` verifies activation pending, activation failed, activation succeeded but offline, ready, deactivation pending, deactivation failed, and deactivated aggregate states from explicit source facts. |
| Failure projection | `TestProjectDeviceRejectsDisabledDevicesExceptDeactivateResults` and `TestRunOnceProcessesFailureAndProjectionEvents` verify provision/deactivation failures keep stable error metadata and terminal operation state, including disabled-device deactivation results. |
| Readiness failure attribution | `TestReadinessFromProjectionStates`, `TestIntegrationProvisioningEndpoints`, and `TestIntegrationDeactivateEndpointUsesProjectedVideoMetadata` verify failed/dead-lettered provisioning and deactivation responses include `readiness.failure` with layer, source state, retryability, error fields, operation id, and occurrence time while pending states omit false failure details. |
| Registry-only readiness | `TestIntegrationProvisioningStateReturnsRegistryOnlyReadiness` verifies enabled and disabled registry-only devices return `200 OK`, `operation: null`, account-side readiness, `product_state=registered`, and preserve `404 Not Found` for truly missing devices. |
| Admin quota and audit visibility | `TestIntegrationSignupEvaluationQuotaAndRaiseWorkflow`, `TestIntegrationResponsesMatchOpenAPIContract`, and `TestListAuditEventsReturnsRecordedLifecycleEvents` verify platform-admin-only quota request list/show, audit event filters, pagination metadata, and existing approve/decline behavior. |
| Broker adapters | `TestNewPublisherCreatesLogPublisherAndRejectsUnsupportedKinds`, `TestNewConsumerCreatesLogConsumerAndRejectsUnsupportedKinds`, `TestLogPublisherWritesEnvelopeJSON`, `TestLogConsumerReadsEnvelopeJSON`, `TestAzureEventHubsPublisherPublishesJSONRecord`, `TestAzureEventHubsConsumerReadsAcrossPartitions`, `TestAzureEventHubsConsumerAcknowledgesAndResumesFromCheckpoint`, and `TestOpenAzurePartitionsUsesStoredCheckpointWhenPresent` cover the deterministic local default adapter plus Azure Event Hubs publish/consume and durable checkpoint resume behavior without requiring live Azure. |
| Database invariants | `TestIntegrationDatabaseSchemaInvariants` plus existing migration tests verify idempotent migrations, normalized email constraint, non-blank organization/device names, owner invariant, critical tables/columns/constraints/indexes, and automatic `updated_at` triggers. |
| OpenAPI contract | `TestIntegrationResponsesMatchOpenAPIContract` plus OpenAPI schema validation cover representative Claim Token resolve/admin, registry-only provisioning-state with nullable `operation`, provisioned/failed provisioning-state, provisioning, deactivation, quota visibility, and audit visibility responses against `openapi.yaml`. |
| Configuration and maintenance | `.env` loading, TTL parsing/fallbacks, worker-specific broker config defaults, required JWT secrets, and refresh-token cleanup behavior. |

## Executed Test Cases

- `rtk_account_manager/internal/api`: `FuzzBindStrictRequestShape/seed#0`
- `rtk_account_manager/internal/api`: `FuzzBindStrictRequestShape/seed#1`
- `rtk_account_manager/internal/api`: `FuzzBindStrictRequestShape/seed#2`
- `rtk_account_manager/internal/api`: `FuzzBindStrictRequestShape/seed#3`
- `rtk_account_manager/internal/api`: `FuzzBindStrictRequestShape/seed#4`
- `rtk_account_manager/internal/api`: `FuzzBindStrictRequestShape/seed#5`
- `rtk_account_manager/internal/api`: `FuzzBindStrictRequestShape`
- `rtk_account_manager/internal/api`: `TestAllowSignupEnforcesCaptchaDisposableAndRateLimit`
- `rtk_account_manager/internal/api`: `TestAuthRecoveryValidationRejectsBlankTokens/reset_password_blank_token`
- `rtk_account_manager/internal/api`: `TestAuthRecoveryValidationRejectsBlankTokens/verify_email_blank_token`
- `rtk_account_manager/internal/api`: `TestAuthRecoveryValidationRejectsBlankTokens`
- `rtk_account_manager/internal/api`: `TestAuthRecoveryValidationRejectsInvalidRequests/forgot_password_invalid_email`
- `rtk_account_manager/internal/api`: `TestAuthRecoveryValidationRejectsInvalidRequests/resend_verification_invalid_email`
- `rtk_account_manager/internal/api`: `TestAuthRecoveryValidationRejectsInvalidRequests/reset_password_short_new_password`
- `rtk_account_manager/internal/api`: `TestAuthRecoveryValidationRejectsInvalidRequests/verify_email_missing_token`
- `rtk_account_manager/internal/api`: `TestAuthRecoveryValidationRejectsInvalidRequests`
- `rtk_account_manager/internal/api`: `TestAuthTokenDeliveryHook`
- `rtk_account_manager/internal/api`: `TestBindStrictRejectsUnknownFields`
- `rtk_account_manager/internal/api`: `TestHealthRoute`
- `rtk_account_manager/internal/api`: `TestIntegrationAdminDeviceClaimTokenWorkflow`
- `rtk_account_manager/internal/api`: `TestIntegrationAdminMetricsReportsEmptySnapshot`
- `rtk_account_manager/internal/api`: `TestIntegrationAuthorizationAndTenancyMatrix/admin_can_create_device`
- `rtk_account_manager/internal/api`: `TestIntegrationAuthorizationAndTenancyMatrix/admin_can_list_devices`
- `rtk_account_manager/internal/api`: `TestIntegrationAuthorizationAndTenancyMatrix/admin_can_resolve_claim`
- `rtk_account_manager/internal/api`: `TestIntegrationAuthorizationAndTenancyMatrix/admin_can_start_provision`
- `rtk_account_manager/internal/api`: `TestIntegrationAuthorizationAndTenancyMatrix/disabled_user_cannot_list_own_devices`
- `rtk_account_manager/internal/api`: `TestIntegrationAuthorizationAndTenancyMatrix/member_can_list_devices`
- `rtk_account_manager/internal/api`: `TestIntegrationAuthorizationAndTenancyMatrix/member_cannot_create_device`
- `rtk_account_manager/internal/api`: `TestIntegrationAuthorizationAndTenancyMatrix/member_cannot_list_quota_requests_as_platform_admin`
- `rtk_account_manager/internal/api`: `TestIntegrationAuthorizationAndTenancyMatrix/member_cannot_resolve_claim`
- `rtk_account_manager/internal/api`: `TestIntegrationAuthorizationAndTenancyMatrix/member_cannot_start_deactivation`
- `rtk_account_manager/internal/api`: `TestIntegrationAuthorizationAndTenancyMatrix/member_cannot_start_provision`
- `rtk_account_manager/internal/api`: `TestIntegrationAuthorizationAndTenancyMatrix/outsider_cannot_create_device_in_foreign_org`
- `rtk_account_manager/internal/api`: `TestIntegrationAuthorizationAndTenancyMatrix/outsider_cannot_list_org_devices`
- `rtk_account_manager/internal/api`: `TestIntegrationAuthorizationAndTenancyMatrix/outsider_cannot_resolve_claim_in_foreign_org`
- `rtk_account_manager/internal/api`: `TestIntegrationAuthorizationAndTenancyMatrix/outsider_cannot_start_deactivation_in_foreign_org`
- `rtk_account_manager/internal/api`: `TestIntegrationAuthorizationAndTenancyMatrix/outsider_cannot_start_provision_in_foreign_org`
- `rtk_account_manager/internal/api`: `TestIntegrationAuthorizationAndTenancyMatrix/owner_can_create_device`
- `rtk_account_manager/internal/api`: `TestIntegrationAuthorizationAndTenancyMatrix/owner_can_list_devices`
- `rtk_account_manager/internal/api`: `TestIntegrationAuthorizationAndTenancyMatrix/owner_can_resolve_claim`
- `rtk_account_manager/internal/api`: `TestIntegrationAuthorizationAndTenancyMatrix/owner_can_start_deactivation`
- `rtk_account_manager/internal/api`: `TestIntegrationAuthorizationAndTenancyMatrix/owner_can_start_provision`
- `rtk_account_manager/internal/api`: `TestIntegrationAuthorizationAndTenancyMatrix/owner_cannot_list_audit_events`
- `rtk_account_manager/internal/api`: `TestIntegrationAuthorizationAndTenancyMatrix/owner_cannot_list_claim_tokens_as_platform_admin`
- `rtk_account_manager/internal/api`: `TestIntegrationAuthorizationAndTenancyMatrix/platform_admin_can_list_audit_events`
- `rtk_account_manager/internal/api`: `TestIntegrationAuthorizationAndTenancyMatrix/platform_admin_can_list_claim_tokens`
- `rtk_account_manager/internal/api`: `TestIntegrationAuthorizationAndTenancyMatrix/platform_admin_can_list_quota_requests`
- `rtk_account_manager/internal/api`: `TestIntegrationAuthorizationAndTenancyMatrix/platform_admin_can_show_quota_request`
- `rtk_account_manager/internal/api`: `TestIntegrationAuthorizationAndTenancyMatrix`
- `rtk_account_manager/internal/api`: `TestIntegrationClaimResolveEndpoint`
- `rtk_account_manager/internal/api`: `TestIntegrationCleanupRefreshTokensRemovesExpiredAndRevokedRows`
- `rtk_account_manager/internal/api`: `TestIntegrationCurrentUserCanChangePassword`
- `rtk_account_manager/internal/api`: `TestIntegrationCurrentUserCanDisableSelfWithOwnerSafety`
- `rtk_account_manager/internal/api`: `TestIntegrationDatabaseMaintainsUpdatedAt`
- `rtk_account_manager/internal/api`: `TestIntegrationDatabaseRejectsInvalidCoreData`
- `rtk_account_manager/internal/api`: `TestIntegrationDeactivateEndpointUsesProjectedVideoMetadata`
- `rtk_account_manager/internal/api`: `TestIntegrationDisabledUserCannotUseExistingTokens`
- `rtk_account_manager/internal/api`: `TestIntegrationEmailVerificationAndPasswordRecovery`
- `rtk_account_manager/internal/api`: `TestIntegrationFleetGroupsAndTags`
- `rtk_account_manager/internal/api`: `TestIntegrationLastOwnerCannotBeRemovedOrDowngraded`
- `rtk_account_manager/internal/api`: `TestIntegrationListPaginationMetadata`
- `rtk_account_manager/internal/api`: `TestIntegrationMigrationsAreIdempotent`
- `rtk_account_manager/internal/api`: `TestIntegrationOwnerCanDisableAndEnableMemberUser`
- `rtk_account_manager/internal/api`: `TestIntegrationOwnerCanUpdateAndRemoveMember`
- `rtk_account_manager/internal/api`: `TestIntegrationOwnerCanUpdateOrganization`
- `rtk_account_manager/internal/api`: `TestIntegrationProvisioningEndpoints`
- `rtk_account_manager/internal/api`: `TestIntegrationProvisioningStateReturnsRegistryOnlyReadiness`
- `rtk_account_manager/internal/api`: `TestIntegrationQuotaRaiseValidationAndDefaultApproval`
- `rtk_account_manager/internal/api`: `TestIntegrationRegisterLoginRefreshAndLogout`
- `rtk_account_manager/internal/api`: `TestIntegrationRejectsBlankNames`
- `rtk_account_manager/internal/api`: `TestIntegrationResponsesMatchOpenAPIContract`
- `rtk_account_manager/internal/api`: `TestIntegrationRoleAuthorizationDeviceScopeAndSerialUniqueness`
- `rtk_account_manager/internal/api`: `TestIntegrationSignupEvaluationQuotaAndRaiseWorkflow`
- `rtk_account_manager/internal/api`: `TestIntegrationStoreRefreshTokenHelpers`
- `rtk_account_manager/internal/api`: `TestIntegrationValidationAndNotFoundErrors`
- `rtk_account_manager/internal/api`: `TestIsDisposableSignupEmail`
- `rtk_account_manager/internal/api`: `TestLoadSignupPolicyHonorsEnvironmentOverrides`
- `rtk_account_manager/internal/api`: `TestLogAuthTokenSinkWritesDelivery`
- `rtk_account_manager/internal/api`: `TestLogQuotaRaiseNotificationSinkWritesDelivery`
- `rtk_account_manager/internal/api`: `TestMatchExistingDeactivateOperation`
- `rtk_account_manager/internal/api`: `TestMatchExistingProvisionOperation`
- `rtk_account_manager/internal/api`: `TestNewAuthTokenAndUnsupportedPurpose`
- `rtk_account_manager/internal/api`: `TestPaginationClampsAndDefaultsValues`
- `rtk_account_manager/internal/api`: `TestReadinessFromProjectionStates/accepted_provisioning_waits_for_activation`
- `rtk_account_manager/internal/api`: `TestReadinessFromProjectionStates/activation_failure_stays_visible`
- `rtk_account_manager/internal/api`: `TestReadinessFromProjectionStates/activation_succeeded_and_online_is_ready`
- `rtk_account_manager/internal/api`: `TestReadinessFromProjectionStates/activation_succeeded_but_offline_waits_for_transport`
- `rtk_account_manager/internal/api`: `TestReadinessFromProjectionStates/deactivation_failure_is_attributed`
- `rtk_account_manager/internal/api`: `TestReadinessFromProjectionStates/deactivation_pending_takes_precedence`
- `rtk_account_manager/internal/api`: `TestReadinessFromProjectionStates/deactivation_success_is_deactivated`
- `rtk_account_manager/internal/api`: `TestReadinessFromProjectionStates/registry_disabled_stays_account-side_only`
- `rtk_account_manager/internal/api`: `TestReadinessFromProjectionStates`
- `rtk_account_manager/internal/api`: `TestRejectUnsupportedClaimMaterial/activation_code`
- `rtk_account_manager/internal/api`: `TestRejectUnsupportedClaimMaterial/factory_identity`
- `rtk_account_manager/internal/api`: `TestRejectUnsupportedClaimMaterial/mac_address`
- `rtk_account_manager/internal/api`: `TestRejectUnsupportedClaimMaterial/qr_code_synonym`
- `rtk_account_manager/internal/api`: `TestRejectUnsupportedClaimMaterial/qr_payload`
- `rtk_account_manager/internal/api`: `TestRejectUnsupportedClaimMaterial/serial_number`
- `rtk_account_manager/internal/api`: `TestRejectUnsupportedClaimMaterial/standalone_claim_material_object`
- `rtk_account_manager/internal/api`: `TestRejectUnsupportedClaimMaterial`
- `rtk_account_manager/internal/api`: `TestRequireAuthRejectsInvalidToken`
- `rtk_account_manager/internal/api`: `TestRequireAuthRejectsMissingToken`
- `rtk_account_manager/internal/api`: `TestRequireAuthRejectsRefreshTokenAsBearer`
- `rtk_account_manager/internal/api`: `TestSMTPQuotaRaiseNotificationSinkWritesDelivery`
- `rtk_account_manager/internal/api`: `TestTrimPtrNormalizesOptionalStrings`
- `rtk_account_manager/internal/api`: `TestValidationHelpersWriteErrors`
- `rtk_account_manager/internal/api`: `TestWriteClaimResolveErrorIncludesRetryability/invalid_token`
- `rtk_account_manager/internal/api`: `TestWriteClaimResolveErrorIncludesRetryability/quota_exceeded`
- `rtk_account_manager/internal/api`: `TestWriteClaimResolveErrorIncludesRetryability/service_unavailable`
- `rtk_account_manager/internal/api`: `TestWriteClaimResolveErrorIncludesRetryability`
- `rtk_account_manager/internal/auth`: `TestExpiredAndWrongSecretTokensFailParsing`
- `rtk_account_manager/internal/auth`: `TestPasswordHashAndCheck`
- `rtk_account_manager/internal/auth`: `TestRandomTokenProducesHashableDistinctValues`
- `rtk_account_manager/internal/auth`: `TestTokenKindValidation`
- `rtk_account_manager/internal/broker`: `TestAzureEventHubsConsumerAcknowledgesAndResumesFromCheckpoint`
- `rtk_account_manager/internal/broker`: `TestAzureEventHubsConsumerCloseClosesPartitions`
- `rtk_account_manager/internal/broker`: `TestAzureEventHubsConsumerDoesNotAdvanceCheckpointPastEarlierUnacknowledgedMessage`
- `rtk_account_manager/internal/broker`: `TestAzureEventHubsConsumerMarksConnectionLossTransient`
- `rtk_account_manager/internal/broker`: `TestAzureEventHubsConsumerReadsAcrossPartitions`
- `rtk_account_manager/internal/broker`: `TestAzureEventHubsConsumerTreatsReceiveTimeoutAsEmptyPoll`
- `rtk_account_manager/internal/broker`: `TestAzureEventHubsPublisherCloseClosesClient`
- `rtk_account_manager/internal/broker`: `TestAzureEventHubsPublisherMarksBatchErrorsTransient/add_event`
- `rtk_account_manager/internal/broker`: `TestAzureEventHubsPublisherMarksBatchErrorsTransient/new_batch`
- `rtk_account_manager/internal/broker`: `TestAzureEventHubsPublisherMarksBatchErrorsTransient`
- `rtk_account_manager/internal/broker`: `TestAzureEventHubsPublisherMarksConnectionLossTransient`
- `rtk_account_manager/internal/broker`: `TestAzureEventHubsPublisherPublishesJSONRecord`
- `rtk_account_manager/internal/broker`: `TestAzureEventHubsPublisherRejectsUnexpectedStream`
- `rtk_account_manager/internal/broker`: `TestAzureMessageDecodeRejectsInvalidJSON`
- `rtk_account_manager/internal/broker`: `TestAzureMessageDecodeRejectsNilEvent`
- `rtk_account_manager/internal/broker`: `TestClassifyAzurePublishErrorLeavesContextErrorsUntouched`
- `rtk_account_manager/internal/broker`: `TestClassifyAzurePublishErrorLeavesUnauthorizedPermanent`
- `rtk_account_manager/internal/broker`: `TestClassifyAzureReceiveErrorLeavesContextErrorsUntouched`
- `rtk_account_manager/internal/broker`: `TestClassifyAzureReceiveErrorLeavesUnauthorizedPermanent`
- `rtk_account_manager/internal/broker`: `TestLogConsumerReadsEnvelopeJSON`
- `rtk_account_manager/internal/broker`: `TestLogPublisherWritesEnvelopeJSON`
- `rtk_account_manager/internal/broker`: `TestNewAzureEventHubsConstructorsRequireConfig`
- `rtk_account_manager/internal/broker`: `TestNewAzureEventHubsConsumerClosesClientWhenPartitionOpenFails`
- `rtk_account_manager/internal/broker`: `TestNewConsumerCreatesLogConsumerAndRejectsUnsupportedKinds`
- `rtk_account_manager/internal/broker`: `TestNewPublisherCreatesLogPublisherAndRejectsUnsupportedKinds`
- `rtk_account_manager/internal/broker`: `TestOpenAzurePartitionsClosesEarlierPartitionsOnFailure`
- `rtk_account_manager/internal/broker`: `TestOpenAzurePartitionsUsesStoredCheckpointWhenPresent`
- `rtk_account_manager/internal/broker`: `TestTransientHelpersExposeWrappedError`
- `rtk_account_manager/internal/broker`: `TestTransientMarker`
- `rtk_account_manager/internal/channel`: `FuzzEnvelopeStrictJSONAndValidation/seed#0`
- `rtk_account_manager/internal/channel`: `FuzzEnvelopeStrictJSONAndValidation/seed#1`
- `rtk_account_manager/internal/channel`: `FuzzEnvelopeStrictJSONAndValidation/seed#2`
- `rtk_account_manager/internal/channel`: `FuzzEnvelopeStrictJSONAndValidation/seed#3`
- `rtk_account_manager/internal/channel`: `FuzzEnvelopeStrictJSONAndValidation/seed#4`
- `rtk_account_manager/internal/channel`: `FuzzEnvelopeStrictJSONAndValidation/seed#5`
- `rtk_account_manager/internal/channel`: `FuzzEnvelopeStrictJSONAndValidation/seed#6`
- `rtk_account_manager/internal/channel`: `FuzzEnvelopeStrictJSONAndValidation`
- `rtk_account_manager/internal/channel`: `TestDecodeStrictJSONRejectsMultipleJSONValues`
- `rtk_account_manager/internal/channel`: `TestEnvelopeUnmarshalRejectsUnknownFields`
- `rtk_account_manager/internal/channel`: `TestValidateAcceptsExplicitFalseRetryable/deactivate_failed`
- `rtk_account_manager/internal/channel`: `TestValidateAcceptsExplicitFalseRetryable/provision_failed`
- `rtk_account_manager/internal/channel`: `TestValidateAcceptsExplicitFalseRetryable`
- `rtk_account_manager/internal/channel`: `TestValidateAndDecodeAcceptsEachMessageType/DeviceDeactivateFailed`
- `rtk_account_manager/internal/channel`: `TestValidateAndDecodeAcceptsEachMessageType/DeviceDeactivateRequested`
- `rtk_account_manager/internal/channel`: `TestValidateAndDecodeAcceptsEachMessageType/DeviceDeactivateSucceeded`
- `rtk_account_manager/internal/channel`: `TestValidateAndDecodeAcceptsEachMessageType/DeviceMetadataChanged`
- `rtk_account_manager/internal/channel`: `TestValidateAndDecodeAcceptsEachMessageType/DeviceOnlineChanged`
- `rtk_account_manager/internal/channel`: `TestValidateAndDecodeAcceptsEachMessageType/DeviceProvisionFailed`
- `rtk_account_manager/internal/channel`: `TestValidateAndDecodeAcceptsEachMessageType/DeviceProvisionRequested`
- `rtk_account_manager/internal/channel`: `TestValidateAndDecodeAcceptsEachMessageType/DeviceProvisionSucceeded`
- `rtk_account_manager/internal/channel`: `TestValidateAndDecodeAcceptsEachMessageType`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsEnvelopeContractMismatches/missing_retryable_field/deactivate_failed`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsEnvelopeContractMismatches/missing_retryable_field/provision_failed`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsEnvelopeContractMismatches/missing_retryable_field`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsEnvelopeContractMismatches/non-UTC_occurred_at`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsEnvelopeContractMismatches/partition_key_mismatch`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsEnvelopeContractMismatches/payload_timestamps_must_use_UTC/deactivate_failed`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsEnvelopeContractMismatches/payload_timestamps_must_use_UTC/deactivate_succeeded`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsEnvelopeContractMismatches/payload_timestamps_must_use_UTC/online_changed`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsEnvelopeContractMismatches/payload_timestamps_must_use_UTC/provision_failed`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsEnvelopeContractMismatches/payload_timestamps_must_use_UTC/provision_succeeded`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsEnvelopeContractMismatches/payload_timestamps_must_use_UTC`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsEnvelopeContractMismatches/payload_unknown_field`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsEnvelopeContractMismatches/source_service_mismatch`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsEnvelopeContractMismatches/stream_mismatch`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsEnvelopeContractMismatches/target_service_mismatch`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsEnvelopeContractMismatches/unknown_message_type`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsEnvelopeContractMismatches/unsupported_schema_version`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsEnvelopeContractMismatches`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsInvalidMessagesForEachType/DeviceDeactivateFailed`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsInvalidMessagesForEachType/DeviceDeactivateRequested`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsInvalidMessagesForEachType/DeviceDeactivateSucceeded`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsInvalidMessagesForEachType/DeviceMetadataChanged`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsInvalidMessagesForEachType/DeviceOnlineChanged`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsInvalidMessagesForEachType/DeviceProvisionFailed`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsInvalidMessagesForEachType/DeviceProvisionRequested`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsInvalidMessagesForEachType/DeviceProvisionSucceeded`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsInvalidMessagesForEachType`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsLifecycleIDsThatAreNotUUIDs/account_device_id/DeviceDeactivateFailed`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsLifecycleIDsThatAreNotUUIDs/account_device_id/DeviceDeactivateRequested`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsLifecycleIDsThatAreNotUUIDs/account_device_id/DeviceDeactivateSucceeded`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsLifecycleIDsThatAreNotUUIDs/account_device_id/DeviceMetadataChanged`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsLifecycleIDsThatAreNotUUIDs/account_device_id/DeviceOnlineChanged`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsLifecycleIDsThatAreNotUUIDs/account_device_id/DeviceProvisionFailed`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsLifecycleIDsThatAreNotUUIDs/account_device_id/DeviceProvisionRequested`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsLifecycleIDsThatAreNotUUIDs/account_device_id/DeviceProvisionSucceeded`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsLifecycleIDsThatAreNotUUIDs/account_device_id`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsLifecycleIDsThatAreNotUUIDs/org_id`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsLifecycleIDsThatAreNotUUIDs`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsMissingEnvelopeFields/correlation_id`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsMissingEnvelopeFields/message_id`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsMissingEnvelopeFields/message_type`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsMissingEnvelopeFields/occurred_at`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsMissingEnvelopeFields/operation_id`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsMissingEnvelopeFields/partition_key`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsMissingEnvelopeFields/payload`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsMissingEnvelopeFields/schema_version`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsMissingEnvelopeFields/source_service`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsMissingEnvelopeFields/target_service`
- `rtk_account_manager/internal/channel`: `TestValidateRejectsMissingEnvelopeFields`
- `rtk_account_manager/internal/config`: `TestLoadDotEnvSetsMissingValuesAndPreservesExistingEnv`
- `rtk_account_manager/internal/config`: `TestLoadFallsBackForInvalidDurations`
- `rtk_account_manager/internal/config`: `TestLoadReadsEnvironmentAndDurations`
- `rtk_account_manager/internal/config`: `TestLoadRequiresJWTSecrets`
- `rtk_account_manager/internal/config`: `TestLoadWorkerAllowsMissingJWTSecrets`
- `rtk_account_manager/internal/config`: `TestLoadWorkerFallsBackForInvalidMaxAttempts`
- `rtk_account_manager/internal/database`: `TestFindMigrationDirMissing`
- `rtk_account_manager/internal/openapi`: `TestOpenAPIContractIsValid`
- `rtk_account_manager/internal/readiness`: `TestOptionsFromEnvReadsSmokeSettings`
- `rtk_account_manager/internal/readiness`: `TestReadinessFallbackLookupsAndSuccessPaths`
- `rtk_account_manager/internal/readiness`: `TestReadinessSkipAndFailurePaths`
- `rtk_account_manager/internal/readiness`: `TestRunDryRunProducesRedactedSkips`
- `rtk_account_manager/internal/readiness`: `TestRunMigrationCheckWithAppliedMigrations`
- `rtk_account_manager/internal/readiness`: `TestRunSmokeDiscoversOrgAndDeviceFromLists`
- `rtk_account_manager/internal/readiness`: `TestRunSmokeFailsLoginAndSkipsDependentChecks`
- `rtk_account_manager/internal/readiness`: `TestRunSmokeFailsOnMissingMigration`
- `rtk_account_manager/internal/readiness`: `TestRunSmokeReadsHealthAuthOrgDeviceAndProvisioning`
- `rtk_account_manager/internal/readiness`: `TestRunSmokeSkipsWhenNoOrganizationsAreVisible`
- `rtk_account_manager/internal/readiness`: `TestUtilityHelpersAndEnvFallbacks`
- `rtk_account_manager/internal/store`: `TestApplyProjectionMetadataPreservesExistingFieldsAndClearsNil`
- `rtk_account_manager/internal/store`: `TestClaimOutboxMessagesReadyLeasesRows`
- `rtk_account_manager/internal/store`: `TestCompareInboxCreateAcceptsLegacyMalformedPayloadSnapshotWithLossyUTF8`
- `rtk_account_manager/internal/store`: `TestCompareInboxCreateAcceptsLegacyMalformedPayloadSnapshot`
- `rtk_account_manager/internal/store`: `TestCompareOperationCreate`
- `rtk_account_manager/internal/store`: `TestCreateOrGetDeviceOperationIsIdempotent`
- `rtk_account_manager/internal/store`: `TestCreateOrGetDeviceOperationRejectsMismatchedDeviceOrganization`
- `rtk_account_manager/internal/store`: `TestCreateOrGetInboxMessageDeduplicates`
- `rtk_account_manager/internal/store`: `TestCreateOrGetInboxMessagePreservesDeadLetterPayloadSnapshot`
- `rtk_account_manager/internal/store`: `TestDeviceClaimTokenAdminLifecycle`
- `rtk_account_manager/internal/store`: `TestDeviceMessagePersistenceRejectsInvalidSchemaValues`
- `rtk_account_manager/internal/store`: `TestEvaluationQuotaUsageUtilizationHandlesZeroAndNonZeroQuotas`
- `rtk_account_manager/internal/store`: `TestGetOutboxMessageDetailIncludesOperation`
- `rtk_account_manager/internal/store`: `TestIntegrationDatabaseSchemaInvariants`
- `rtk_account_manager/internal/store`: `TestJSONHelpers`
- `rtk_account_manager/internal/store`: `TestListAuditEventsReturnsRecordedLifecycleEvents`
- `rtk_account_manager/internal/store`: `TestListInboxMessagesByStatusAndShowDetail`
- `rtk_account_manager/internal/store`: `TestListOutboxMessagesByStatusFiltersLifecycleRows`
- `rtk_account_manager/internal/store`: `TestMergeDeviceMetadataPreservesUnrelatedFields`
- `rtk_account_manager/internal/store`: `TestMetadataChangedProjectionFiltersNonVideoCloudKeys`
- `rtk_account_manager/internal/store`: `TestOnlineChangedProjectionSetsStatusAndLastSeenAt`
- `rtk_account_manager/internal/store`: `TestOutboxMessagePersistenceAndReadyList`
- `rtk_account_manager/internal/store`: `TestProjectDeviceProvisioningAndOnlineRules`
- `rtk_account_manager/internal/store`: `TestProjectDeviceRejectsDisabledDevicesExceptDeactivateResults`
- `rtk_account_manager/internal/store`: `TestRecordInboxProcessTransitionUpdatesOperationAndProjection`
- `rtk_account_manager/internal/store`: `TestRecordOutboxPublishTransitionLetsPublishedOutcomeOverrideLaterFailure`
- `rtk_account_manager/internal/store`: `TestRecordOutboxPublishTransitionPreservesInboxCompletedOperation`
- `rtk_account_manager/internal/store`: `TestRecordOutboxPublishTransitionRejectsStaleLease`
- `rtk_account_manager/internal/store`: `TestRecordOutboxPublishTransitionUpdatesOperationState`
- `rtk_account_manager/internal/store`: `TestRequeueInboxMessageReopensDeadLetteredRow`
- `rtk_account_manager/internal/store`: `TestRequeueOutboxMessageRejectsCompletedLifecycleOperation`
- `rtk_account_manager/internal/store`: `TestRequeueOutboxMessageResetsRetryState`
- `rtk_account_manager/internal/store`: `TestResolveDeviceClaimTokenCreatesDeviceAndClaim`
- `rtk_account_manager/internal/store`: `TestResolveDeviceClaimTokenMatchesExistingDevice`
- `rtk_account_manager/internal/store`: `TestResolveDeviceClaimTokenRejectsAlreadyClaimedToken`
- `rtk_account_manager/internal/store`: `TestResolveDeviceClaimTokenRejectsCrossOrganizationToken`
- `rtk_account_manager/internal/store`: `TestResolveDeviceClaimTokenRejectsExpiredToken`
- `rtk_account_manager/internal/store`: `TestResolveDeviceClaimTokenRejectsInvalidToken`
- `rtk_account_manager/internal/store`: `TestResolveDeviceClaimTokenRejectsUnsupportedCategory`
- `rtk_account_manager/internal/store`: `TestStartDeviceDeactivationOperationRejectsMissingProjectedMetadata`
- `rtk_account_manager/internal/store`: `TestStartDeviceDeactivationOperationUsesProjectedMetadata`
- `rtk_account_manager/internal/store`: `TestStartDeviceLifecycleOperationPersistsPendingProvisionMetadata`
- `rtk_account_manager/internal/store`: `TestValidatePartitionKeyMatchesOperationSkipsBlankValues`
- `rtk_account_manager/internal/worker/inbox`: `TestRunOnceDeadLettersInvalidMessages`
- `rtk_account_manager/internal/worker/inbox`: `TestRunOnceDeadLettersInvalidPartitionKeysAfterPersistingInboxRow/blank_partition_key_is_normalized_for_storage_and_dead-lettered`
- `rtk_account_manager/internal/worker/inbox`: `TestRunOnceDeadLettersInvalidPartitionKeysAfterPersistingInboxRow/nonblank_mismatched_partition_key_dead-letters`
- `rtk_account_manager/internal/worker/inbox`: `TestRunOnceDeadLettersInvalidPartitionKeysAfterPersistingInboxRow`
- `rtk_account_manager/internal/worker/inbox`: `TestRunOnceDeadLettersLifecycleMessagesWithMismatchedPartitionKeys`
- `rtk_account_manager/internal/worker/inbox`: `TestRunOnceDeadLettersMalformedAndUnmappedMessages/command-only_message_on_events_stream_dead-letters`
- `rtk_account_manager/internal/worker/inbox`: `TestRunOnceDeadLettersMalformedAndUnmappedMessages/malformed_payload_keeps_inspectable_inbox_row`
- `rtk_account_manager/internal/worker/inbox`: `TestRunOnceDeadLettersMalformedAndUnmappedMessages/malformed_payload_with_invalid_utf-8_keeps_exact_bytes`
- `rtk_account_manager/internal/worker/inbox`: `TestRunOnceDeadLettersMalformedAndUnmappedMessages`
- `rtk_account_manager/internal/worker/inbox`: `TestRunOnceDeadLettersTransientProjectionFailureAtAttemptLimit`
- `rtk_account_manager/internal/worker/inbox`: `TestRunOnceDoesNotAcknowledgeRetryingMessages`
- `rtk_account_manager/internal/worker/inbox`: `TestRunOnceDoesNotAcknowledgeWhenTransitionFails`
- `rtk_account_manager/internal/worker/inbox`: `TestRunOnceProcessesFailureAndProjectionEvents/deactivate_failure_records_retryable_error`
- `rtk_account_manager/internal/worker/inbox`: `TestRunOnceProcessesFailureAndProjectionEvents/deactivate_success_keeps_deactivation_projection`
- `rtk_account_manager/internal/worker/inbox`: `TestRunOnceProcessesFailureAndProjectionEvents/metadata_changed_filters_non_video-cloud_keys`
- `rtk_account_manager/internal/worker/inbox`: `TestRunOnceProcessesFailureAndProjectionEvents/online_changed_updates_device_status_projection_only`
- `rtk_account_manager/internal/worker/inbox`: `TestRunOnceProcessesFailureAndProjectionEvents/provision_failure_marks_operation_failed`
- `rtk_account_manager/internal/worker/inbox`: `TestRunOnceProcessesFailureAndProjectionEvents`
- `rtk_account_manager/internal/worker/inbox`: `TestRunOnceProcessesOnlineProjectionWhenOperationAlreadyCompleted`
- `rtk_account_manager/internal/worker/inbox`: `TestRunOnceProcessesProvisionSuccess`
- `rtk_account_manager/internal/worker/inbox`: `TestRunOnceRetriesTransientProjectionFailures`
- `rtk_account_manager/internal/worker/inbox`: `TestRunOnceSkipsCompletedLifecycleReplayForRetryingMessage`
- `rtk_account_manager/internal/worker/inbox`: `TestRunOnceSkipsCompletedLifecycleReplayWithNewMessageID`
- `rtk_account_manager/internal/worker/inbox`: `TestRunOnceSkipsPreviouslyProcessedDuplicates`
- `rtk_account_manager/internal/worker/inbox`: `TestRunOnceUsesWorkerClockWhenEnvelopeTimeIsMissing`
- `rtk_account_manager/internal/worker/inbox`: `TestRunRetriesTransientReceiveErrors`
- `rtk_account_manager/internal/worker/outbox`: `TestRunOnceDeadLettersExhaustedPublishFailures`
- `rtk_account_manager/internal/worker/outbox`: `TestRunOnceDeadLettersInvalidOutboxPayload`
- `rtk_account_manager/internal/worker/outbox`: `TestRunOnceIgnoresConflictWhenRetryLosesToPublished`
- `rtk_account_manager/internal/worker/outbox`: `TestRunOnceIgnoresStaleLeaseTransitionConflict`
- `rtk_account_manager/internal/worker/outbox`: `TestRunOnceMarksSuccessfulPublishes`
- `rtk_account_manager/internal/worker/outbox`: `TestRunOnceSchedulesTransientRetry`

## Commands

```sh
gofmt -l .
TEST_DATABASE_URL='***' go test -json ./... -coverpkg=./internal/... -coverprofile=reports/coverage.out -covermode=atomic
go tool cover -func=reports/coverage.out
go tool cover -html=reports/coverage.out -o reports/coverage.html
go build ./...
```

## Artifacts

| Artifact | Purpose |
| --- | --- |
| reports/test-events.json | Machine-readable Go test event log. |
| reports/coverage.out | Go coverage profile. |
| reports/coverage.txt | Function-level coverage summary. |
| reports/coverage.html | HTML coverage report. |
| reports/gofmt.txt | Files requiring gofmt, empty when formatting passes. |
| reports/build.txt | Build output, empty when build passes. |
| reports/test-cases.md | Markdown list of passing test cases captured from Go JSON events. |
| reports/correctness-gates.md | Required correctness behavior gates and pass/fail status. |

## Coverage Gaps To Watch

- Command entry points under `cmd/*` are intentionally validated by `go build ./...`, not unit coverage.
- Store and database behavior are primarily covered through API integration tests.
- Add or update tests whenever authorization, membership, token, migration, device lifecycle, or cross-service channel validation behavior changes.
