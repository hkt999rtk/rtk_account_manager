package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

type ProductGrantSummary struct {
	Revision         int64  `json:"revision"`
	Digest           string `json:"digest"`
	LogRetentionDays *int   `json:"log_retention_days,omitempty"`
}

func (s *Store) GetCurrentProductGrantSummary(ctx context.Context, cloudID, productID string) (ProductGrantSummary, error) {
	var summary ProductGrantSummary
	err := s.db.QueryRow(ctx, `SELECT revision,snapshot_sha256,log_retention_days FROM product_service_grants
		WHERE brand_cloud_id=$1 AND product_id=$2 ORDER BY revision DESC LIMIT 1`, cloudID, productID).
		Scan(&summary.Revision, &summary.Digest, &summary.LogRetentionDays)
	if errors.Is(err, pgx.ErrNoRows) {
		return summary, nil
	}
	return summary, err
}
