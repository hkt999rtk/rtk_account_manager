package store

import (
	"context"
	"github.com/jackc/pgx/v5"
	"rtk_account_manager/internal/model"
)

// Reuse the device permission predicate for every aggregate. Group membership
// and tag counts never include devices excluded by the owner's Product scope.
func scopedDevicesCTE(user, permission string) (string, []any) {
	clause, args := fleetAccessPredicate(user, ActorTypeUser, permission)
	return `WITH visible_devices AS (SELECT d.id FROM devices d WHERE d.organization_id::text=$1` + clause + `) `, args
}

func (s *Store) ListDeviceGroupsForUser(ctx context.Context, org, user, groupID string, limit, offset int) (DeviceGroupPage, error) {
	cte, actorArgs := scopedDevicesCTE(user, "device_group.read")
	args := append([]any{org}, actorArgs...)
	args = append(args, groupID)
	visible := ` FROM device_groups g WHERE g.organization_id::text=$1 AND ($5='' OR g.id::text=$5) AND (
        EXISTS(SELECT 1 FROM device_group_members gm JOIN visible_devices vd ON vd.id=gm.device_id WHERE gm.group_id=g.id AND gm.organization_id=g.organization_id)
        OR EXISTS(SELECT 1 FROM organizations o JOIN organization_members om ON om.organization_id=o.id
            WHERE o.id=g.organization_id AND om.user_id::text=$2 AND user_can_access_brand_cloud($2,$1) AND (o.organization_kind<>'brand_cloud' OR om.role='owner')))`
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return DeviceGroupPage{}, err
	}
	defer tx.Rollback(ctx)
	var total int
	if err := tx.QueryRow(ctx, cte+`SELECT count(*)`+visible, args...).Scan(&total); err != nil {
		return DeviceGroupPage{}, err
	}
	args = append(args, limit, offset)
	rows, err := tx.Query(ctx, cte+`SELECT g.id::text,g.organization_id::text,g.name,g.description,g.created_at,g.updated_at,
        (SELECT count(*)::int FROM device_group_members gm JOIN visible_devices vd ON vd.id=gm.device_id WHERE gm.group_id=g.id AND gm.organization_id=g.organization_id)`+visible+` ORDER BY g.name,g.id LIMIT $6 OFFSET $7`, args...)
	if err != nil {
		return DeviceGroupPage{}, err
	}
	items := []model.DeviceGroup{}
	for rows.Next() {
		item, err := scanDeviceGroup(rows)
		if err != nil {
			rows.Close()
			return DeviceGroupPage{}, err
		}
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return DeviceGroupPage{}, err
	}
	return DeviceGroupPage{Groups: items, Page: Page{Limit: limit, Offset: offset, Total: total}}, tx.Commit(ctx)
}

func (s *Store) GetDeviceGroupForUser(ctx context.Context, org, user, group string) (model.DeviceGroup, error) {
	page, err := s.ListDeviceGroupsForUser(ctx, org, user, group, 1, 0)
	if err != nil {
		return model.DeviceGroup{}, err
	}
	if len(page.Groups) != 1 {
		return model.DeviceGroup{}, ErrNotFound
	}
	return page.Groups[0], nil
}

func (s *Store) ListOrganizationTagsForUser(ctx context.Context, org, user string, limit, offset int) (DeviceTagSummaryPage, error) {
	cte, actorArgs := scopedDevicesCTE(user, "device_tag.read")
	args := append([]any{org}, actorArgs...)
	visible := ` FROM device_tags tags JOIN visible_devices vd ON vd.id=tags.device_id WHERE tags.organization_id::text=$1`
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return DeviceTagSummaryPage{}, err
	}
	defer tx.Rollback(ctx)
	var total int
	if err := tx.QueryRow(ctx, cte+`SELECT count(DISTINCT tags.tag)`+visible, args...).Scan(&total); err != nil {
		return DeviceTagSummaryPage{}, err
	}
	args = append(args, limit, offset)
	rows, err := tx.Query(ctx, cte+`SELECT tags.tag,count(DISTINCT tags.device_id)::int`+visible+` GROUP BY tags.tag ORDER BY tags.tag LIMIT $5 OFFSET $6`, args...)
	if err != nil {
		return DeviceTagSummaryPage{}, err
	}
	items := []DeviceTagSummary{}
	for rows.Next() {
		var item DeviceTagSummary
		if err := rows.Scan(&item.Tag, &item.DeviceCount); err != nil {
			rows.Close()
			return DeviceTagSummaryPage{}, err
		}
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return DeviceTagSummaryPage{}, err
	}
	return DeviceTagSummaryPage{Tags: items, Page: Page{Limit: limit, Offset: offset, Total: total}}, tx.Commit(ctx)
}
