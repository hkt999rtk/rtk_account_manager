package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"rtk_account_manager/internal/model"
)

func (s *Store) AuthorizeUserForVideoDevice(ctx context.Context, userID, videoCloudDevid string) error {
	var ok bool
	err := s.db.QueryRow(ctx, `
		SELECT true
		FROM devices d
		JOIN organization_members m ON m.organization_id = d.organization_id
		WHERE m.user_id = $1
		  AND d.disabled_at IS NULL
		  AND d.metadata ->> $2 = $3
		LIMIT 1
	`, userID, model.DeviceMetadataVideoCloudDevid, videoCloudDevid).Scan(&ok)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	return nil
}
