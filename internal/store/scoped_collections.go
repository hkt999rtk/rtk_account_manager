package store

import (
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5"
	"rtk_account_manager/internal/model"
	"strings"
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

func (s *Store) ListDeviceGroupAggregatesForUser(ctx context.Context, org, user string, limit, offset int) (DeviceGroupAggregatePage, error) {
	groups, err := s.ListDeviceGroupsForUser(ctx, org, user, "", limit, offset)
	if err != nil {
		return DeviceGroupAggregatePage{}, err
	}
	result := DeviceGroupAggregatePage{Aggregates: make([]DeviceGroupAggregate, 0, len(groups.Groups)), Page: groups.Page}
	for _, group := range groups.Groups {
		row := s.db.QueryRow(ctx, `
			SELECT count(d.id)::int,
				count(d.id) FILTER (WHERE d.status='online')::int,
				count(d.id) FILTER (WHERE d.status='offline')::int,
				COALESCE((SELECT jsonb_object_agg(COALESCE(d2.metadata->>'health','unknown'), d2.n)
					FROM (SELECT d.metadata, count(*)::int AS n FROM device_group_members gm JOIN devices d ON d.id=gm.device_id
						WHERE gm.organization_id=$1 AND gm.group_id=$2 GROUP BY d.metadata->>'health') d2), '{}'::jsonb),
				COALESCE((SELECT jsonb_object_agg(COALESCE(d2.metadata->>'firmware','unknown'), d2.n)
					FROM (SELECT d.metadata, count(*)::int AS n FROM device_group_members gm JOIN devices d ON d.id=gm.device_id
						WHERE gm.organization_id=$1 AND gm.group_id=$2 GROUP BY d.metadata->>'firmware') d2), '{}'::jsonb)
			FROM device_group_members gm JOIN devices d ON d.id=gm.device_id
			WHERE gm.organization_id=$1 AND gm.group_id=$2`, org, group.ID)
		var item DeviceGroupAggregate
		var health, firmware []byte
		if err := row.Scan(&item.MemberCount, &item.OnlineCount, &item.OfflineCount, &health, &firmware); err != nil {
			return DeviceGroupAggregatePage{}, err
		}
		if err := json.Unmarshal(health, &item.HealthDistribution); err != nil {
			return DeviceGroupAggregatePage{}, err
		}
		if err := json.Unmarshal(firmware, &item.FirmwareDistribution); err != nil {
			return DeviceGroupAggregatePage{}, err
		}
		item.GroupID = group.ID
		result.Aggregates = append(result.Aggregates, item)
	}
	return result, nil
}

func (s *Store) ListOrganizationTagsForUser(ctx context.Context, org, user string, limit, offset int) (DeviceTagSummaryPage, error) {
	cte, actorArgs := scopedDevicesCTE(user, "device_tag.read")
	args := append([]any{org}, actorArgs...)
	visible := ` FROM device_tag_catalog catalog LEFT JOIN device_tags tags ON tags.organization_id=catalog.organization_id AND tags.tag=catalog.tag LEFT JOIN visible_devices vd ON vd.id=tags.device_id WHERE catalog.organization_id::text=$1`
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return DeviceTagSummaryPage{}, err
	}
	defer tx.Rollback(ctx)
	var total int
	if err := tx.QueryRow(ctx, cte+`SELECT count(DISTINCT catalog.tag)`+visible, args...).Scan(&total); err != nil {
		return DeviceTagSummaryPage{}, err
	}
	args = append(args, limit, offset)
	rows, err := tx.Query(ctx, cte+`SELECT catalog.tag,count(DISTINCT CASE WHEN vd.id IS NOT NULL THEN tags.device_id END)::int`+visible+` GROUP BY catalog.tag ORDER BY catalog.tag LIMIT $5 OFFSET $6`, args...)
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

func (s *Store) CreateOrganizationTag(ctx context.Context, orgID, tag string) error {
	_, err := s.db.Exec(ctx, `INSERT INTO device_tag_catalog (organization_id, tag) VALUES ($1,$2)`, orgID, strings.TrimSpace(tag))
	return err
}

func (s *Store) RenameOrganizationTag(ctx context.Context, orgID, oldTag, newTag string) error {
	oldTag, newTag = strings.TrimSpace(oldTag), strings.TrimSpace(newTag)
	if oldTag == "" || newTag == "" {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `DELETE FROM device_tags old WHERE old.organization_id=$1 AND old.tag=$2 AND EXISTS (SELECT 1 FROM device_tags existing WHERE existing.organization_id=old.organization_id AND existing.device_id=old.device_id AND existing.tag=$3)`, orgID, oldTag, newTag); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE device_tags SET tag=$3, updated_at=now() WHERE organization_id=$1 AND tag=$2`, orgID, oldTag, newTag); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) DeleteOrganizationTag(ctx context.Context, orgID, tag string) error {
	result, err := s.db.Exec(ctx, `DELETE FROM device_tags WHERE organization_id=$1 AND tag=$2`, orgID, strings.TrimSpace(tag))
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
