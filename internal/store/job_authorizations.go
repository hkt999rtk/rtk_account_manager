package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrJobAuthorizationRevoked = errors.New("job authorization revoked")

type JobAuthorizationInput struct {
	JobID        string
	BrandCloudID string
	ActorUserID  string
	ScopeHash    string
	Capability   string
	ProductIDs   []string
	ExpiresAt    time.Time
}

type JobAuthorization struct {
	ID                   string     `json:"id"`
	JobID                string     `json:"job_id"`
	BrandCloudID         string     `json:"brand_cloud_id"`
	ActorUserID          string     `json:"actor_user_id"`
	ScopeHash            string     `json:"scope_hash"`
	Capability           string     `json:"capability"`
	ProductIDs           []string   `json:"product_ids"`
	AuthorizationVersion int64      `json:"authorization_version"`
	OwnershipVersion     int64      `json:"ownership_version"`
	Status               string     `json:"status"`
	ExpiresAt            time.Time  `json:"expires_at"`
	RevokedAt            *time.Time `json:"revoked_at,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
}

const jobAuthorizationColumns = `id::text,job_id::text,brand_cloud_id::text,actor_user_id::text,scope_hash,capability,product_ids::text[],authorization_version,ownership_version,status,expires_at,revoked_at,created_at,updated_at`

func scanJobAuthorization(row pgx.Row) (JobAuthorization, error) {
	var grant JobAuthorization
	err := row.Scan(&grant.ID, &grant.JobID, &grant.BrandCloudID, &grant.ActorUserID, &grant.ScopeHash, &grant.Capability, &grant.ProductIDs, &grant.AuthorizationVersion, &grant.OwnershipVersion, &grant.Status, &grant.ExpiresAt, &grant.RevokedAt, &grant.CreatedAt, &grant.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return JobAuthorization{}, ErrNotFound
	}
	return grant, err
}

func (s *Store) CreateJobAuthorization(ctx context.Context, in JobAuthorizationInput, now time.Time) (JobAuthorization, error) {
	now = now.UTC().Truncate(time.Microsecond)
	in.ExpiresAt = in.ExpiresAt.UTC().Truncate(time.Microsecond)
	in.JobID, in.BrandCloudID, in.ActorUserID = strings.TrimSpace(in.JobID), strings.TrimSpace(in.BrandCloudID), strings.TrimSpace(in.ActorUserID)
	in.ScopeHash, in.Capability = strings.ToLower(strings.TrimSpace(in.ScopeHash)), strings.TrimSpace(in.Capability)
	if in.Capability != "provisioning.create" || len(in.ScopeHash) != 64 || len(in.ProductIDs) == 0 || !in.ExpiresAt.After(now) || in.ExpiresAt.After(now.Add(7*24*time.Hour)) {
		return JobAuthorization{}, ErrConflict
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return JobAuthorization{}, err
	}
	defer tx.Rollback(ctx)
	var authorizationVersion, ownershipVersion int64
	err = tx.QueryRow(ctx, `
		SELECT o.authorization_version,o.ownership_version
		FROM organizations o
		JOIN organization_members m ON m.organization_id=o.id AND m.user_id::text=$2 AND m.disabled_at IS NULL
		JOIN users u ON u.id=m.user_id AND u.disabled_at IS NULL AND u.email_verified AND NOT u.signup_pending_verification
		WHERE o.id::text=$1 AND o.organization_kind='brand_cloud' AND o.deleted_at IS NULL
		  AND user_can_access_brand_cloud($2,$1)
		  AND EXISTS (
		    SELECT 1 FROM role_assignments ra JOIN roles r ON r.id=ra.role_id AND r.disabled_at IS NULL
		    JOIN role_permissions rp ON rp.role_id=r.id JOIN permissions p ON p.id=rp.permission_id
		    WHERE ra.actor_type='user' AND ra.actor_id=$2 AND ra.organization_id=o.id AND ra.disabled_at IS NULL
		      AND p.name='lifecycle_operation.provision' AND brand_cloud_permission_allowed($2,$1,p.name)
		  )
		FOR UPDATE OF o`, in.BrandCloudID, in.ActorUserID).Scan(&authorizationVersion, &ownershipVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return JobAuthorization{}, ErrNotFound
	}
	if err != nil {
		return JobAuthorization{}, err
	}
	var productCount int
	if err = tx.QueryRow(ctx, `SELECT count(*)::int FROM device_item_profiles WHERE brand_cloud_id::text=$1 AND id=ANY($2::uuid[]) AND disabled_at IS NULL`, in.BrandCloudID, in.ProductIDs).Scan(&productCount); err != nil {
		return JobAuthorization{}, err
	}
	if productCount != len(in.ProductIDs) {
		return JobAuthorization{}, ErrNotFound
	}
	grant, err := scanJobAuthorization(tx.QueryRow(ctx, `INSERT INTO job_authorizations(job_id,brand_cloud_id,actor_user_id,scope_hash,capability,product_ids,authorization_version,ownership_version,expires_at)
		VALUES($1,$2,$3,$4,$5,$6::uuid[],$7,$8,$9)
		ON CONFLICT(job_id,brand_cloud_id,actor_user_id) DO UPDATE SET updated_at=job_authorizations.updated_at
		RETURNING `+jobAuthorizationColumns, in.JobID, in.BrandCloudID, in.ActorUserID, in.ScopeHash, in.Capability, in.ProductIDs, authorizationVersion, ownershipVersion, in.ExpiresAt))
	if err != nil {
		return JobAuthorization{}, err
	}
	if grant.ScopeHash != in.ScopeHash || grant.Capability != in.Capability || !grant.ExpiresAt.Equal(in.ExpiresAt) {
		return JobAuthorization{}, ErrConflict
	}
	if err = createAuditEventTx(ctx, tx, AuditEventInput{EventType: "job_authorization_created", ActorUserID: &in.ActorUserID, OrganizationID: &in.BrandCloudID, SubjectType: "job_authorization", SubjectID: grant.ID, Payload: map[string]any{"job_id": grant.JobID, "scope_hash": grant.ScopeHash, "capability": grant.Capability}}); err != nil {
		return JobAuthorization{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return JobAuthorization{}, err
	}
	return grant, nil
}

// ValidateJobAuthorization is the sole exchange/use-time gate. It invalidates
// the grant when identity, membership, capability, cloud state, or versions drift.
func (s *Store) ValidateJobAuthorization(ctx context.Context, authorizationID string, now time.Time) (JobAuthorization, error) {
	grant, err := scanJobAuthorization(s.db.QueryRow(ctx, `SELECT `+jobAuthorizationColumns+` FROM job_authorizations j
		WHERE j.id::text=$1 AND j.status='active' AND j.expires_at>$2
		  AND EXISTS (SELECT 1 FROM organizations o JOIN organization_members m ON m.organization_id=o.id AND m.user_id=j.actor_user_id AND m.disabled_at IS NULL
		    JOIN users u ON u.id=m.user_id AND u.disabled_at IS NULL AND u.email_verified AND NOT u.signup_pending_verification
		    WHERE o.id=j.brand_cloud_id AND o.deleted_at IS NULL AND o.authorization_version=j.authorization_version AND o.ownership_version=j.ownership_version
		      AND user_can_access_brand_cloud(j.actor_user_id::text,j.brand_cloud_id::text)
		      AND EXISTS (SELECT 1 FROM role_assignments ra JOIN roles r ON r.id=ra.role_id AND r.disabled_at IS NULL JOIN role_permissions rp ON rp.role_id=r.id JOIN permissions p ON p.id=rp.permission_id
		        WHERE ra.actor_type='user' AND ra.actor_id=j.actor_user_id::text AND ra.organization_id=j.brand_cloud_id AND ra.disabled_at IS NULL
		          AND p.name=CASE j.capability WHEN 'provisioning.create' THEN 'lifecycle_operation.provision' END
		          AND brand_cloud_permission_allowed(j.actor_user_id::text,j.brand_cloud_id::text,p.name)))`, authorizationID, now))
	if errors.Is(err, ErrNotFound) {
		invalidated, updateErr := scanJobAuthorization(s.db.QueryRow(ctx, `UPDATE job_authorizations SET status=CASE WHEN expires_at<=$2 THEN 'expired' ELSE 'revoked' END,revoked_at=CASE WHEN expires_at>$2 THEN COALESCE(revoked_at,$2) ELSE revoked_at END,updated_at=$2 WHERE id::text=$1 AND status='active' RETURNING `+jobAuthorizationColumns, authorizationID, now))
		if updateErr == nil {
			_ = s.CreateAuditEvent(ctx, AuditEventInput{EventType: "job_authorization_invalidated", ActorUserID: &invalidated.ActorUserID, OrganizationID: &invalidated.BrandCloudID, SubjectType: "job_authorization", SubjectID: invalidated.ID, Payload: map[string]any{"job_id": invalidated.JobID, "status": invalidated.Status}})
		}
		return JobAuthorization{}, ErrJobAuthorizationRevoked
	}
	return grant, err
}

func (s *Store) RevokeJobAuthorization(ctx context.Context, authorizationID string, now time.Time) (JobAuthorization, error) {
	grant, err := scanJobAuthorization(s.db.QueryRow(ctx, `UPDATE job_authorizations SET status='revoked',revoked_at=COALESCE(revoked_at,$2),updated_at=$2 WHERE id::text=$1 AND status<>'expired' RETURNING `+jobAuthorizationColumns, authorizationID, now))
	if err != nil {
		return JobAuthorization{}, err
	}
	_ = s.CreateAuditEvent(ctx, AuditEventInput{EventType: "job_authorization_revoked", ActorUserID: &grant.ActorUserID, OrganizationID: &grant.BrandCloudID, SubjectType: "job_authorization", SubjectID: grant.ID, Payload: map[string]any{"job_id": grant.JobID}})
	return grant, nil
}
