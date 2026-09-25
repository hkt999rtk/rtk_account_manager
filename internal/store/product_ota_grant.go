package store

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

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

func (s *Store) GetProductOTAGrant(ctx context.Context, brandCloudID, productID string) (ProductOTAGrant, error) {
	var status string
	var profileOptionsJSON, grantOptionsJSON, grantBindingsJSON []byte
	var out ProductOTAGrant
	err := s.db.QueryRow(ctx, `
		SELECT p.brand_cloud_id::text, p.id::text, p.status, p.service_options,
		       COALESCE(g.revision, 0), COALESCE(g.options, '[]'::jsonb),
		       COALESCE(g.bindings, 'null'::jsonb),
		       COALESCE(g.snapshot_sha256, '')
		FROM device_item_profiles p
		LEFT JOIN LATERAL (
			SELECT revision, options, bindings, snapshot_sha256
			FROM product_service_grants
			WHERE product_id = p.id AND brand_cloud_id = p.brand_cloud_id
			ORDER BY revision DESC LIMIT 1
		) g ON true
		WHERE p.brand_cloud_id = $1 AND p.id = $2
	`, brandCloudID, productID).Scan(
		&out.BrandCloudID, &out.ProductID, &status, &profileOptionsJSON,
		&out.ProductServiceRevision, &grantOptionsJSON, &grantBindingsJSON,
		&out.ServiceGrantSHA256,
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
	_, _, expectedDigest, err := encodeProductServiceGrant(grantOptions, bindings)
	if err != nil {
		return ProductOTAGrant{}, err
	}
	out.Active = status == string(model.DeviceItemProfileStatusActive)
	out.Enabled = out.Active && out.ProductServiceRevision > 0 &&
		strings.EqualFold(strings.TrimSpace(out.ServiceGrantSHA256), expectedDigest) &&
		slices.Equal(profileOptions, grantOptions) && slices.Contains(grantOptions, "ota")
	return out, nil
}
