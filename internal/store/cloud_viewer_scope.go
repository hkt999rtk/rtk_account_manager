package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"rtk_account_manager/internal/model"
)

func normalizeCloudMemberScope(role model.Role, scope *model.CloudViewerScope) (*model.CloudViewerScope, error) {
	if role != model.RoleViewer {
		if scope != nil {
			return nil, model.ErrInvalidCloudViewerScope
		}
		return nil, nil
	}
	if scope == nil {
		return nil, model.ErrInvalidCloudViewerScope
	}
	normalized, err := scope.Normalized()
	if err != nil {
		return nil, err
	}
	return &normalized, nil
}

func sameCloudScope(a, b *model.CloudViewerScope) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

type CloudMemberUpdateInput struct {
	BrandCloudID, ActorUserID, TargetUserID string
	Role                                    model.Role
	AccessScope                             *model.CloudViewerScope
}

func (s *Store) UpdateDeveloperBrandCloudMember(ctx context.Context, in CloudMemberUpdateInput) (model.Member, error) {
	if in.Role == "" && in.AccessScope == nil {
		return model.Member{}, model.ErrInvalidCloudViewerScope
	}
	if in.Role != "" && in.Role != model.RoleAdmin && in.Role != model.RoleMember && in.Role != model.RoleViewer {
		return model.Member{}, ErrConflict
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.Member{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockBrandCloudCollaborationTx(ctx, tx, in.BrandCloudID, in.ActorUserID, in.TargetUserID); err != nil {
		return model.Member{}, err
	}
	if err := requireBrandCloudOwnerTx(ctx, tx, in.BrandCloudID, in.ActorUserID); err != nil {
		return model.Member{}, err
	}
	var role model.Role
	if err := tx.QueryRow(ctx, `SELECT role FROM organization_members WHERE organization_id::text=$1 AND user_id::text=$2`, in.BrandCloudID, in.TargetUserID).Scan(&role); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrNotFound
		}
		return model.Member{}, err
	}
	if role == model.RoleOwner {
		return model.Member{}, ErrLastOwner
	}
	if in.Role != "" {
		role = in.Role
	}
	scope, err := normalizeCloudMemberScope(role, in.AccessScope)
	if err != nil {
		return model.Member{}, err
	}
	if err := validateCloudViewerProductsTx(ctx, tx, in.BrandCloudID, scope); err != nil {
		return model.Member{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE organization_members SET role=$3,access_scope=$4,updated_at=now() WHERE organization_id::text=$1 AND user_id::text=$2`, in.BrandCloudID, in.TargetUserID, role, scope); err != nil {
		return model.Member{}, err
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{EventType: "brand_cloud_member_scope_changed", ActorUserID: &in.ActorUserID, OrganizationID: &in.BrandCloudID, SubjectType: "user", SubjectID: in.TargetUserID, Payload: map[string]any{"role": role, "access_scope": scope}}); err != nil {
		return model.Member{}, err
	}
	member, err := s.getMemberTx(ctx, tx, in.BrandCloudID, in.TargetUserID)
	if err != nil {
		return model.Member{}, err
	}
	return member, tx.Commit(ctx)
}

func validateCloudViewerProductsTx(ctx context.Context, tx pgx.Tx, cloud string, scope *model.CloudViewerScope) error {
	if scope == nil || scope.Kind == "all_products" {
		return nil
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM device_item_profiles WHERE brand_cloud_id::text=$1 AND id::text=ANY($2::text[])`, cloud, scope.ProductIDs).Scan(&count); err != nil {
		return err
	}
	if count != len(scope.ProductIDs) {
		return ErrNotFound
	}
	return nil
}
