package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"rtk_account_manager/internal/model"
)

func TestGlobalAuthorizationHonorsCurrentUserAndMembershipState(t *testing.T) {
	for _, tc := range []struct {
		name                                 string
		pending, disabled, suspended, absent bool
	}{
		{name: "active"},
		{name: "pending", pending: true},
		{name: "disabled-user", disabled: true},
		{name: "disabled-member", suspended: true},
		{name: "no-member", absent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newStoreIntegrationEnv(t)
			ctx := context.Background()
			orgID, userID, deviceID := createDeviceFixture(t, env)
			if _, err := env.db.Exec(ctx, `WITH anchor AS (
				INSERT INTO users(email,password_hash,email_verified) VALUES('anchor@example.com','fixture-hash',true) RETURNING id)
				INSERT INTO organization_members(organization_id,user_id,role) SELECT $1,id,'owner' FROM anchor`, orgID); err != nil {
				t.Fatal(err)
			}
			if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin=true,signup_pending_verification=$2,
				disabled_at=CASE WHEN $3 THEN now() ELSE NULL END WHERE id=$1`, userID, tc.pending, tc.disabled); err != nil {
				t.Fatal(err)
			}
			if _, err := env.db.Exec(ctx, `UPDATE devices SET metadata=metadata || jsonb_build_object($2::text,'fixture-video-device') WHERE id=$1`, deviceID, model.DeviceMetadataVideoCloudDevid); err != nil {
				t.Fatal(err)
			}
			if tc.absent {
				if _, err := env.db.Exec(ctx, `DELETE FROM organization_members WHERE user_id=$1 AND organization_id=$2`, userID, orgID); err != nil {
					t.Fatal(err)
				}
			} else if tc.suspended {
				if _, err := env.db.Exec(ctx, `UPDATE organization_members SET disabled_at=now() WHERE user_id=$1 AND organization_id=$2`, userID, orgID); err != nil {
					t.Fatal(err)
				}
			}
			// Deliberately retain a valid ACL assignment: authorization must not
			// mistake it for proof of a currently eligible membership or user.
			if _, err := env.db.Exec(ctx, `INSERT INTO role_assignments(role_id,actor_type,actor_id,scope_type,scope_id,organization_id)
				SELECT id,'user',$1,'organization',$2::uuid::text,$2::uuid FROM roles WHERE name='owner' ON CONFLICT DO NOTHING`, userID, orgID); err != nil {
				t.Fatal(err)
			}
			var activeAssignments int
			if err := env.db.QueryRow(ctx, `SELECT count(*) FROM role_assignments WHERE actor_type='user' AND actor_id=$1 AND disabled_at IS NULL`, userID).Scan(&activeAssignments); err != nil {
				t.Fatal(err)
			}
			if activeAssignments == 0 {
				t.Fatal("fixture requires an active assignment independent of membership state")
			}
			userEligible := !tc.pending && !tc.disabled
			memberEligible := userEligible && !tc.suspended && !tc.absent
			if _, err := env.store.GetRole(ctx, orgID, userID); (err == nil) != memberEligible || err != nil && !errors.Is(err, ErrNotFound) {
				t.Errorf("GetRole err=%v eligible=%t", err, memberEligible)
			}
			checks := map[string]func() (bool, error){
				"organization": func() (bool, error) { return env.store.HasPermission(ctx, userID, orgID, "organization.read") },
				"resource": func() (bool, error) {
					return env.store.HasUserPermissionForResource(ctx, userID, orgID, "registry_device.read", ScopeTypeDevice, deviceID)
				},
				"any-resource": func() (bool, error) {
					return env.store.HasUserPermissionAnyResource(ctx, userID, orgID, "registry_device.read")
				},
				"device": func() (bool, error) {
					return env.store.HasUserDevicePermission(ctx, userID, orgID, "registry_device.read", deviceID)
				},
			}
			for name, check := range checks {
				if allowed, err := check(); err != nil || allowed != memberEligible {
					t.Errorf("%s allowed=%t want=%t err=%v", name, allowed, memberEligible, err)
				}
			}
			// A platform capability is not an organization-scoped ACL grant,
			// even while the global user and their membership are eligible.
			if allowed, err := env.store.HasPermission(ctx, userID, orgID, PermissionACLRead); err != nil || allowed {
				t.Errorf("platform privilege leaked into organization: allowed=%t err=%v", allowed, err)
			}
			if permissions, err := env.store.ListUserOrganizationPermissions(ctx, userID, orgID); err != nil || (len(permissions) > 0) != memberEligible {
				t.Errorf("organization capabilities=%v eligible=%t err=%v", permissions, memberEligible, err)
			}
			if err := env.store.AuthorizeUserForVideoDevice(ctx, userID, "fixture-video-device"); (err == nil) != memberEligible || err != nil && !errors.Is(err, ErrNotFound) {
				t.Errorf("delegated device authorization err=%v eligible=%t", err, memberEligible)
			}
			if allowed, err := env.store.HasPermission(ctx, userID, "", PermissionPlatformRead); err != nil || allowed != userEligible {
				t.Errorf("platform authorization allowed=%t want=%t err=%v", allowed, userEligible, err)
			}
			if err := env.store.SaveRefreshToken(ctx, userID, "fixture-before-suspension", time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if _, err := env.store.RefreshTokenActive(ctx, "fixture-before-suspension"); (err == nil) != userEligible || err != nil && !errors.Is(err, ErrNotFound) {
				t.Errorf("refresh active err=%v eligible=%t", err, userEligible)
			}
			if err := env.store.RotateRefreshToken(ctx, "fixture-before-suspension", "fixture-after-suspension", userID, time.Now().Add(time.Hour)); (err == nil) != userEligible || err != nil && !errors.Is(err, ErrNotFound) {
				t.Errorf("refresh rotation err=%v eligible=%t", err, userEligible)
			}
			var revoked, replacements int
			if err := env.db.QueryRow(ctx, `SELECT count(*) FILTER(WHERE token_hash='fixture-before-suspension' AND revoked_at IS NOT NULL),
				count(*) FILTER(WHERE token_hash='fixture-after-suspension') FROM refresh_tokens`).Scan(&revoked, &replacements); err != nil {
				t.Fatal(err)
			}
			if userEligible && (revoked != 1 || replacements != 1) || !userEligible && (revoked != 0 || replacements != 0) {
				t.Errorf("refresh transaction revoked=%d replacements=%d eligible=%t", revoked, replacements, userEligible)
			}
		})
	}
}

func TestAppCertificateHonorsCurrentGlobalUserState(t *testing.T) {
	for _, state := range []string{"active", "pending", "disabled"} {
		t.Run(state, func(t *testing.T) {
			env := newStoreIntegrationEnv(t)
			ctx := context.Background()
			_, userID, _ := createDeviceFixture(t, env)
			now := time.Now()
			input := AppCertificateCreateInput{
				UserID: userID, Subject: "app-user:" + userID,
				CSRSHA256: "fixture-csr", CertificatePEM: "fixture-cert", CertificateChainPEM: "fixture-chain",
				FingerprintSHA256: "fixture-first", SerialNumber: "fixture-1", IssuerRequestID: "fixture-issuer-1",
				NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
			}
			first, err := env.store.CreateAppCertificate(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := env.db.Exec(ctx, `UPDATE users SET signup_pending_verification=$2,
				disabled_at=CASE WHEN $3 THEN now() ELSE NULL END WHERE id=$1`, userID, state == "pending", state == "disabled"); err != nil {
				t.Fatal(err)
			}
			eligible := state == "active"
			if err := env.store.AuthorizeActiveAppCertificateForSubject(ctx, "user", userID, input.FingerprintSHA256, now); (err == nil) != eligible || err != nil && !errors.Is(err, ErrNotFound) {
				t.Errorf("existing certificate authorization: err=%v eligible=%t", err, eligible)
			}
			if _, err := env.store.GetValidAppCertificateForUser(ctx, userID, now); (err == nil) != eligible || err != nil && !errors.Is(err, ErrNotFound) {
				t.Errorf("existing certificate lookup: err=%v eligible=%t", err, eligible)
			}
			input.FingerprintSHA256, input.SerialNumber, input.IssuerRequestID = "fixture-second", "fixture-2", "fixture-issuer-2"
			if _, err := env.store.CreateAppCertificate(ctx, input); (err == nil) != eligible || err != nil && !errors.Is(err, ErrNotFound) {
				t.Errorf("certificate enrollment: err=%v eligible=%t", err, eligible)
			}
			original, err := env.store.GetAppCertificateByIssuerRequestID(ctx, first.IssuerRequestID)
			if err != nil || (original.RevokedAt != nil) != eligible {
				t.Errorf("certificate rotation must be atomic: revoked=%t eligible=%t err=%v", original.RevokedAt != nil, eligible, err)
			}
			var certificates int
			if err := env.db.QueryRow(ctx, `SELECT count(*) FROM app_certificates WHERE subject_id=$1`, userID).Scan(&certificates); err != nil {
				t.Fatal(err)
			}
			if eligible && certificates != 2 || !eligible && certificates != 1 {
				t.Errorf("certificate count=%d eligible=%t", certificates, eligible)
			}
		})
	}
}

func TestPlatformACLWithoutMembershipRequiresEligibleGlobalUser(t *testing.T) {
	for _, state := range []string{"active", "pending", "disabled"} {
		t.Run(state, func(t *testing.T) {
			env := newStoreIntegrationEnv(t)
			ctx := context.Background()
			var userID string
			if err := env.db.QueryRow(ctx, `INSERT INTO users(email,password_hash,email_verified,signup_pending_verification,disabled_at)
				VALUES('platform-acl@example.com','fixture-hash',true,$1,CASE WHEN $2 THEN now() ELSE NULL END) RETURNING id::text`, state == "pending", state == "disabled").Scan(&userID); err != nil {
				t.Fatal(err)
			}
			if _, err := env.db.Exec(ctx, `INSERT INTO role_assignments(role_id,actor_type,actor_id,scope_type)
				SELECT id,'user',$1,'platform' FROM roles WHERE name='platform_admin'`, userID); err != nil {
				t.Fatal(err)
			}
			eligible := state == "active"
			if allowed, err := env.store.HasPermission(ctx, userID, "", PermissionPlatformRead); err != nil || allowed != eligible {
				t.Errorf("platform-only ACL allowed=%t want=%t err=%v", allowed, eligible, err)
			}
			if permissions, err := env.store.ListUserPlatformPermissions(ctx, userID); err != nil || (len(permissions) > 0) != eligible {
				t.Errorf("platform capabilities=%v eligible=%t err=%v", permissions, eligible, err)
			}
		})
	}
}
