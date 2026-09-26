package store

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"rtk_account_manager/internal/model"
)

// ProductOTAGrant is a current Product decision, resolved from the Product row
// and its latest immutable service grant in one database snapshot.
type ProductOTAGrant struct {
	BrandCloudID           string
	ProductID              string
	Active                 bool
	Enabled                bool
	ProductServiceRevision int64
	ServiceGrantSHA256     string
}

// HistoricalProductOTAGrant describes one immutable Product service revision.
// The interval is bounded by the next revision, so callers can verify the
// grant that existed when a billable OTA action was authorized. Product
// lifecycle and device authorization remain separate checks at the source.
type HistoricalProductOTAGrant struct {
	BrandCloudID           string     `json:"brand_cloud_id"`
	ProductID              string     `json:"product_id"`
	ServiceCode            string     `json:"service_code"`
	Enabled                bool       `json:"enabled"`
	ProductServiceRevision int64      `json:"product_service_revision"`
	ServiceGrantSHA256     string     `json:"service_grant_sha256"`
	ValidFrom              time.Time  `json:"valid_from"`
	ValidUntil             *time.Time `json:"valid_until,omitempty"`
}

func (s *Store) GetHistoricalProductOTAGrant(ctx context.Context, brandCloudID, productID string, revision int64) (HistoricalProductOTAGrant, error) {
	if revision < 1 {
		return HistoricalProductOTAGrant{}, ErrNotFound
	}
	var out HistoricalProductOTAGrant
	var rawOptions, rawBindings []byte
	var logRetentionDays *int
	err := s.db.QueryRow(ctx, `
		SELECT g.brand_cloud_id::text, g.product_id::text, g.revision,
		       g.options, g.bindings, g.log_retention_days, g.snapshot_sha256, g.created_at,
		       (SELECT next.created_at FROM product_service_grants next
		        WHERE next.brand_cloud_id=g.brand_cloud_id AND next.product_id=g.product_id
		          AND next.revision>g.revision ORDER BY next.revision LIMIT 1)
		FROM product_service_grants g
		WHERE g.brand_cloud_id=$1 AND g.product_id=$2 AND g.revision=$3
	`, brandCloudID, productID, revision).Scan(&out.BrandCloudID, &out.ProductID,
		&out.ProductServiceRevision, &rawOptions, &rawBindings, &logRetentionDays,
		&out.ServiceGrantSHA256, &out.ValidFrom, &out.ValidUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return HistoricalProductOTAGrant{}, ErrNotFound
	}
	if err != nil {
		return HistoricalProductOTAGrant{}, err
	}
	var options []string
	var bindings []PlatformServiceCatalogOption
	if err := json.Unmarshal(rawOptions, &options); err != nil {
		return HistoricalProductOTAGrant{}, ErrConflict
	}
	if err := json.Unmarshal(rawBindings, &bindings); err != nil {
		return HistoricalProductOTAGrant{}, ErrConflict
	}
	_, _, digest, err := encodeProductServiceGrantWithRetention(options, bindings, logRetentionDays)
	if err != nil || out.ServiceGrantSHA256 != digest ||
		out.ValidUntil != nil && out.ValidUntil.Before(out.ValidFrom) {
		return HistoricalProductOTAGrant{}, ErrConflict
	}
	out.ServiceCode = "ota"
	out.Enabled = slices.Contains(options, "ota")
	return out, nil
}

func (s *Store) GetProductOTAGrant(ctx context.Context, brandCloudID, productID string) (ProductOTAGrant, error) {
	var status string
	var profileOptionsJSON, grantOptionsJSON, grantBindingsJSON []byte
	var out ProductOTAGrant
	var logRetentionDays *int
	err := s.db.QueryRow(ctx, `
		SELECT p.brand_cloud_id::text, p.id::text, p.status, p.service_options,
		       COALESCE(g.revision, 0), COALESCE(g.options, '[]'::jsonb),
		       COALESCE(g.bindings, 'null'::jsonb),
		       COALESCE(g.snapshot_sha256, ''), g.log_retention_days
		FROM device_item_profiles p
		LEFT JOIN LATERAL (
			SELECT revision, options, bindings, snapshot_sha256, log_retention_days
			FROM product_service_grants
			WHERE product_id = p.id AND brand_cloud_id = p.brand_cloud_id
			ORDER BY revision DESC LIMIT 1
		) g ON true
		WHERE p.brand_cloud_id = $1 AND p.id = $2
	`, brandCloudID, productID).Scan(
		&out.BrandCloudID, &out.ProductID, &status, &profileOptionsJSON,
		&out.ProductServiceRevision, &grantOptionsJSON, &grantBindingsJSON,
		&out.ServiceGrantSHA256, &logRetentionDays,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProductOTAGrant{}, ErrNotFound
	}
	if err != nil {
		return ProductOTAGrant{}, err
	}
	var profileOptions, grantOptions []string
	if err := json.Unmarshal(profileOptionsJSON, &profileOptions); err != nil {
		return ProductOTAGrant{}, err
	}
	if err := json.Unmarshal(grantOptionsJSON, &grantOptions); err != nil {
		return ProductOTAGrant{}, err
	}
	var bindings []PlatformServiceCatalogOption
	if err := json.Unmarshal(grantBindingsJSON, &bindings); err != nil {
		return ProductOTAGrant{}, err
	}
	slices.Sort(profileOptions)
	slices.Sort(grantOptions)
	_, _, expectedDigest, err := encodeProductServiceGrantWithRetention(grantOptions, bindings, logRetentionDays)
	if err != nil {
		return ProductOTAGrant{}, err
	}
	out.Active = status == string(model.DeviceItemProfileStatusActive)
	out.Enabled = out.Active && out.ProductServiceRevision > 0 &&
		strings.EqualFold(strings.TrimSpace(out.ServiceGrantSHA256), expectedDigest) &&
		slices.Equal(profileOptions, grantOptions) && slices.Contains(grantOptions, "ota")
	return out, nil
}
