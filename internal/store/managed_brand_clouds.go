package store

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5"

	"rtk_account_manager/internal/model"
)

type ManagedBrandCloud struct {
	model.Organization
	Description      string     `json:"description"`
	OwnerUserID      string     `json:"owner_user_id"`
	MyRole           model.Role `json:"my_role"`
	OwnershipVersion int64      `json:"ownership_version"`
	Capabilities     []string   `json:"capabilities"`
	Operational      bool       `json:"-"`
}

type ManagedBrandCloudPage struct {
	BrandClouds []ManagedBrandCloud `json:"brand_clouds"`
	Page
	OwnedCount int `json:"owned_count"`
	OwnedLimit int `json:"owned_limit"`
}

// Quota is independent from the selected view and includes pending/disabled
// ownership, but not shared clouds or tombstones. One snapshot covers counts
// and rows so concurrent admission/deletion cannot produce mismatched pages.
func (s *Store) ListManagedBrandClouds(ctx context.Context, userID, view string, limit, offset int) (ManagedBrandCloudPage, error) {
	view = strings.TrimSpace(view)
	if view == "" {
		view = "all"
	}
	if view != "all" && view != "owned" && view != "shared" {
		return ManagedBrandCloudPage{}, ErrConflict
	}
	if limit < 1 || limit > 100 || offset < 0 {
		return ManagedBrandCloudPage{}, ErrConflict
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ManagedBrandCloudPage{}, err
	}
	defer tx.Rollback(ctx)
	page := ManagedBrandCloudPage{BrandClouds: []ManagedBrandCloud{}, Page: Page{Limit: limit, Offset: offset}}
	err = tx.QueryRow(ctx, `SELECT developer_cloud_limit FROM users WHERE id::text=$1 AND disabled_at IS NULL`, userID).Scan(&page.OwnedLimit)
	if err == pgx.ErrNoRows {
		return ManagedBrandCloudPage{}, ErrNotFound
	}
	if err != nil {
		return ManagedBrandCloudPage{}, err
	}
	page.OwnedCount, err = countDeveloperBrandCloudsTx(ctx, tx, userID)
	if err != nil {
		return ManagedBrandCloudPage{}, err
	}
	const filter = ` FROM organizations o JOIN organization_members m ON m.organization_id=o.id
        WHERE m.user_id::text=$1 AND m.disabled_at IS NULL
          AND o.organization_kind='brand_cloud' AND o.deleted_at IS NULL
          AND ($2='all' OR ($2='owned' AND m.role='owner') OR ($2='shared' AND m.role<>'owner'))`
	if err := tx.QueryRow(ctx, `SELECT count(*)`+filter, userID, view).Scan(&page.Total); err != nil {
		return ManagedBrandCloudPage{}, err
	}
	rows, err := tx.Query(ctx, `SELECT o.id::text,o.name,o.tenant_slug,m.role,o.organization_kind,
        CASE WHEN o.status='disabled' THEN 'disabled'
             WHEN NOT EXISTS(SELECT 1 FROM organization_members om JOIN users u ON u.id=om.user_id
                 WHERE om.organization_id=o.id AND om.role='owner' AND om.disabled_at IS NULL AND u.disabled_at IS NULL)
                 THEN 'disabled'
             WHEN NOT EXISTS(SELECT 1 FROM organization_members om JOIN users u ON u.id=om.user_id
                 WHERE om.organization_id=o.id AND om.role='owner' AND u.email_verified AND NOT u.signup_pending_verification)
                 THEN 'pending_activation' ELSE o.status END,
        o.tier,o.evaluation_device_quota,o.metadata,o.created_at,o.updated_at,o.description,
        (SELECT om.user_id::text FROM organization_members om WHERE om.organization_id=o.id AND om.role='owner'),
        o.ownership_version,user_can_access_brand_cloud($1,o.id::text)`+filter+`
        ORDER BY o.created_at,o.id LIMIT $3 OFFSET $4`, userID, view, limit, offset)
	if err != nil {
		return ManagedBrandCloudPage{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var cloud ManagedBrandCloud
		var metadata []byte
		err := rows.Scan(&cloud.ID, &cloud.Name, &cloud.TenantSlug, &cloud.Role, &cloud.OrganizationKind, &cloud.Status,
			&cloud.Tier, &cloud.EvaluationDeviceQuota, &metadata, &cloud.CreatedAt, &cloud.UpdatedAt, &cloud.Description,
			&cloud.OwnerUserID, &cloud.OwnershipVersion, &cloud.Operational)
		if err != nil {
			return ManagedBrandCloudPage{}, err
		}
		if err := json.Unmarshal(metadata, &cloud.Metadata); err != nil {
			return ManagedBrandCloudPage{}, err
		}
		cloud.MyRole = cloud.Role
		cloud.Capabilities = []string{}
		page.BrandClouds = append(page.BrandClouds, cloud)
	}
	if err := rows.Err(); err != nil {
		return ManagedBrandCloudPage{}, err
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return ManagedBrandCloudPage{}, err
	}
	return page, nil
}
