package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// BrandCloudBillingTierPeriod proves a tier only for periods covered by the
// append-only history. The migration baseline never certifies earlier months.
type BrandCloudBillingTierPeriod struct {
	BrandCloudID            string     `json:"brand_cloud_id"`
	Tier                    string     `json:"tier,omitempty"`
	CoveredFrom             *time.Time `json:"covered_from,omitempty"`
	HistoryCoversStart      bool       `json:"history_covers_start"`
	ChangedWithinPeriod     bool       `json:"changed_within_period"`
	CommercialForFullPeriod bool       `json:"commercial_for_full_period"`
}

func (s *Store) GetBrandCloudBillingTierPeriod(ctx context.Context, brandCloudID string, start, end time.Time) (BrandCloudBillingTierPeriod, error) {
	if brandCloudID == "" || !end.After(start) {
		return BrandCloudBillingTierPeriod{}, ErrConflict
	}
	start, end = start.UTC(), end.UTC()
	out := BrandCloudBillingTierPeriod{BrandCloudID: brandCloudID}
	var kind string
	var tier *string
	err := s.db.QueryRow(ctx, `
		SELECT o.organization_kind, e.tier, e.observed_at,
		       EXISTS (SELECT 1 FROM brand_cloud_tier_events changed
		               WHERE changed.brand_cloud_id=o.id
		                 AND changed.observed_at>$2 AND changed.observed_at<$3)
		FROM organizations o
		LEFT JOIN LATERAL (
		    SELECT tier, observed_at FROM brand_cloud_tier_events
		    WHERE brand_cloud_id=o.id AND observed_at<=$2
		    ORDER BY observed_at DESC, id DESC LIMIT 1
		) e ON true
		WHERE o.id=$1
	`, brandCloudID, start, end).Scan(&kind, &tier, &out.CoveredFrom, &out.ChangedWithinPeriod)
	if errors.Is(err, pgx.ErrNoRows) {
		return BrandCloudBillingTierPeriod{}, ErrNotFound
	}
	if err != nil {
		return BrandCloudBillingTierPeriod{}, err
	}
	if kind != "brand_cloud" {
		return BrandCloudBillingTierPeriod{}, ErrNotFound
	}
	if tier != nil {
		out.Tier = *tier
		out.HistoryCoversStart = true
		out.CommercialForFullPeriod = *tier == "commercial" && !out.ChangedWithinPeriod
	}
	return out, nil
}
