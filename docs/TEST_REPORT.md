# Test Report

Generated: 2026-04-27T04:34:09Z

## Summary

| Check | Result |
| --- | --- |
| Overall | PASS |
| Formatting | PASS |
| Tests | PASS |
| Build | PASS |
| Coverage threshold | PASS |

## Coverage

| Metric | Value |
| --- | --- |
| Total statement coverage | 80.5% |
| Minimum required coverage | 80.0% |
| Coverage mode | atomic |
| Coverage scope | ./internal/... |

## Test Execution

| Metric | Value |
| --- | --- |
| Go packages | 10 |
| Test cases started | 31 |
| JSON pass events | 37 |
| JSON fail events | 0 |
| Integration database | Postgres via TEST_DATABASE_URL |

## Correctness Validation

Coverage is only a signal that code executed. Correctness is validated by assertions in the automated tests. This report confirms the following behavior groups were exercised:

| Behavior group | Evidence |
| --- | --- |
| Auth and sessions | Register, login, invalid login, refresh rotation, old refresh rejection, logout revocation, expired token parsing, wrong-secret parsing. |
| Disabled users | Disabled users cannot use existing access tokens, refresh tokens, or login until re-enabled. |
| Organization access | Current-user organization listing, organization create/get/update, cross-organization organization access rejection. |
| Member management | Owner add/update/remove/disable/enable member flows, admin/member forbidden paths, last-owner downgrade/remove/disable protection. |
| Device lifecycle | Device create/list/get/update/status update/soft-delete, disabled-device read-only behavior, duplicate serial rejection, same serial in another org allowed. |
| Authorization boundaries | owner/admin/member role permissions and cross-organization device/member access rejection. |
| Database invariants | Idempotent migrations, normalized email constraint, non-blank organization/device names, owner invariant, automatic updated_at triggers. |
| OpenAPI contract | OpenAPI schema validation and representative response validation against `openapi.yaml`. |
| Configuration and maintenance | `.env` loading, TTL parsing/fallbacks, required JWT secrets, refresh-token cleanup behavior. |

## Executed Test Cases

- `rtk_account_manager/internal/api`: `TestIntegrationCleanupRefreshTokensRemovesExpiredAndRevokedRows`
- `rtk_account_manager/internal/api`: `TestIntegrationDatabaseMaintainsUpdatedAt`
- `rtk_account_manager/internal/api`: `TestIntegrationDatabaseRejectsInvalidCoreData`
- `rtk_account_manager/internal/api`: `TestIntegrationDisabledUserCannotUseExistingTokens`
- `rtk_account_manager/internal/api`: `TestIntegrationLastOwnerCannotBeRemovedOrDowngraded`
- `rtk_account_manager/internal/api`: `TestIntegrationListPaginationMetadata`
- `rtk_account_manager/internal/api`: `TestIntegrationMigrationsAreIdempotent`
- `rtk_account_manager/internal/api`: `TestIntegrationOwnerCanDisableAndEnableMemberUser`
- `rtk_account_manager/internal/api`: `TestIntegrationOwnerCanUpdateAndRemoveMember`
- `rtk_account_manager/internal/api`: `TestIntegrationOwnerCanUpdateOrganization`
- `rtk_account_manager/internal/api`: `TestIntegrationRegisterLoginRefreshAndLogout`
- `rtk_account_manager/internal/api`: `TestIntegrationRejectsBlankNames`
- `rtk_account_manager/internal/api`: `TestIntegrationResponsesMatchOpenAPIContract`
- `rtk_account_manager/internal/api`: `TestIntegrationRoleAuthorizationDeviceScopeAndSerialUniqueness`
- `rtk_account_manager/internal/api`: `TestIntegrationStoreRefreshTokenHelpers`
- `rtk_account_manager/internal/api`: `TestIntegrationValidationAndNotFoundErrors`
- `rtk_account_manager/internal/api`: `TestPaginationClampsAndDefaultsValues`
- `rtk_account_manager/internal/api`: `TestRequireAuthRejectsInvalidToken`
- `rtk_account_manager/internal/api`: `TestRequireAuthRejectsMissingToken`
- `rtk_account_manager/internal/api`: `TestRequireAuthRejectsRefreshTokenAsBearer`
- `rtk_account_manager/internal/api`: `TestTrimPtrNormalizesOptionalStrings`
- `rtk_account_manager/internal/api`: `TestValidationHelpersWriteErrors`
- `rtk_account_manager/internal/auth`: `TestExpiredAndWrongSecretTokensFailParsing`
- `rtk_account_manager/internal/auth`: `TestPasswordHashAndCheck`
- `rtk_account_manager/internal/auth`: `TestTokenKindValidation`
- `rtk_account_manager/internal/config`: `TestLoadDotEnvSetsMissingValuesAndPreservesExistingEnv`
- `rtk_account_manager/internal/config`: `TestLoadFallsBackForInvalidDurations`
- `rtk_account_manager/internal/config`: `TestLoadReadsEnvironmentAndDurations`
- `rtk_account_manager/internal/config`: `TestLoadRequiresJWTSecrets`
- `rtk_account_manager/internal/database`: `TestFindMigrationDirMissing`
- `rtk_account_manager/internal/openapi`: `TestOpenAPIContractIsValid`

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

## Coverage Gaps To Watch

- Command entry points under `cmd/*` are intentionally validated by `go build ./...`, not unit coverage.
- Store and database behavior are primarily covered through API integration tests.
- Add or update integration tests whenever authorization, membership, token, migration, or device lifecycle behavior changes.
