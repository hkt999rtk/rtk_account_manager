package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// LegacyProductServiceGrantReport describes an all-Product, read-only preflight
// or an explicitly applied backfill. Issues block the entire write transaction.
type LegacyProductServiceGrantReport struct {
	Products         int            `json:"products"`
	AlreadyVersioned int            `json:"already_versioned"`
	NeedsBackfill    int            `json:"needs_backfill"`
	WithoutMQTT      int            `json:"without_mqtt"`
	Applied          int            `json:"applied"`
	OptionCounts     map[string]int `json:"option_counts"`
	IssueCount       int            `json:"issue_count"`
	Issues           []string       `json:"issues,omitempty"`
	SnapshotSHA256   string         `json:"snapshot_sha256"`
	Ready            bool           `json:"ready"`
}

type legacyProductServiceGrant struct {
	productID, brandCloudID string
	options                 []string
}

// BackfillLegacyProductServiceGrants never infers MQTT or consults the current
// catalog. Existing runs keep their null legacy revision; only future runs use
// the backfilled immutable grant. Apply locks both tables and commits all-or-none.
func (s *Store) BackfillLegacyProductServiceGrants(ctx context.Context, apply bool, expectedSnapshotSHA256 string) (LegacyProductServiceGrantReport, error) {
	report := LegacyProductServiceGrantReport{OptionCounts: map[string]int{}}
	txOptions := pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
	if apply {
		txOptions = pgx.TxOptions{}
	}
	tx, err := s.db.BeginTx(ctx, txOptions)
	if err != nil {
		return report, err
	}
	defer tx.Rollback(ctx)
	var schemaReady bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_schema=current_schema() AND table_name='product_service_grants' AND column_name='legacy'
	)`).Scan(&schemaReady); err != nil {
		return report, err
	}
	if !schemaReady {
		return report, fmt.Errorf("legacy Product service grant schema migration 080 is required")
	}
	if apply {
		// Block concurrent Product edits and grant writes until the snapshot is
		// checked and committed. Report-only mode does not block writers.
		if _, err := tx.Exec(ctx, `LOCK TABLE device_item_profiles, product_service_grants IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return report, err
		}
	}
	rows, err := tx.Query(ctx, `SELECT p.id::text,p.brand_cloud_id::text,p.service_options,g.revision,g.options,
		g.brand_cloud_id::text,g.bindings,g.snapshot_sha256,to_jsonb(g)
		FROM device_item_profiles p LEFT JOIN LATERAL (
			SELECT * FROM product_service_grants
			WHERE product_id=p.id ORDER BY revision DESC LIMIT 1
		) g ON true ORDER BY p.id`)
	if err != nil {
		return report, err
	}
	var missing []legacyProductServiceGrant
	hash := sha256.New()
	for rows.Next() {
		var productID, brandCloudID string
		var profileJSON, grantJSON, bindingsJSON, grantRecordJSON []byte
		var revision sql.NullInt64
		var grantCloud, storedDigest sql.NullString
		if err := rows.Scan(&productID, &brandCloudID, &profileJSON, &revision, &grantJSON, &grantCloud, &bindingsJSON, &storedDigest, &grantRecordJSON); err != nil {
			rows.Close()
			return report, err
		}
		// Hash raw JSONB and current grant presence before normalizing options.
		// This binds apply to the exact report snapshot, including malformed rows.
		fingerprintRow, err := json.Marshal(struct {
			ProductID      string          `json:"product_id"`
			BrandCloudID   string          `json:"brand_cloud_id"`
			ServiceOptions json.RawMessage `json:"service_options"`
			GrantRecord    json.RawMessage `json:"grant_record"`
		}{productID, brandCloudID, profileJSON, grantRecordJSON})
		if err != nil {
			rows.Close()
			return report, err
		}
		_, _ = hash.Write(fingerprintRow)
		_, _ = hash.Write([]byte("\n"))
		report.Products++
		if revision.Valid {
			report.AlreadyVersioned++
		} else {
			report.NeedsBackfill++
		}
		var options []string
		if err := json.Unmarshal(profileJSON, &options); err != nil || len(options) == 0 || validateProductServiceOptions(options) != nil {
			report.addIssue(productID, "invalid Product service_options")
			continue
		}
		for _, code := range options {
			report.OptionCounts[code]++
		}
		if !slices.Contains(options, "mqtt") {
			report.WithoutMQTT++
		}
		if revision.Valid {
			var grantOptions []string
			if err := json.Unmarshal(grantJSON, &grantOptions); err != nil || validateProductServiceOptions(grantOptions) != nil || !serviceOptionSetsEqual(options, grantOptions) {
				report.addIssue(productID, "latest grant differs from Product service_options")
				continue
			}
			if !grantCloud.Valid || grantCloud.String != brandCloudID {
				report.addIssue(productID, "latest grant belongs to another Brand Cloud")
			}
			var bindings []PlatformServiceCatalogOption
			if err := json.Unmarshal(bindingsJSON, &bindings); err != nil {
				report.addIssue(productID, "latest grant bindings are invalid")
				continue
			}
			_, _, wantDigest, err := encodeProductServiceGrant(grantOptions, bindings)
			if err != nil || !storedDigest.Valid || storedDigest.String != wantDigest {
				report.addIssue(productID, "latest grant digest is invalid")
			}
			continue
		}
		unknown := false
		for _, code := range options {
			if !knownLegacyProductOption(code) {
				report.addIssue(productID, "unrecognized legacy service option "+code)
				unknown = true
			}
		}
		if unknown {
			continue
		}
		missing = append(missing, legacyProductServiceGrant{productID: productID, brandCloudID: brandCloudID, options: options})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return report, err
	}
	rows.Close()
	report.SnapshotSHA256 = hex.EncodeToString(hash.Sum(nil))
	report.Ready = report.IssueCount == 0
	if apply && !strings.EqualFold(strings.TrimSpace(expectedSnapshotSHA256), report.SnapshotSHA256) {
		report.addIssue("snapshot", "reviewed report SHA-256 is missing or stale")
		report.Ready = false
	}
	if !apply || !report.Ready {
		return report, nil
	}
	for _, product := range missing {
		options := slices.Clone(product.options)
		optionsJSON, bindingsJSON, digest, err := encodeProductServiceGrant(options, []PlatformServiceCatalogOption{})
		if err != nil {
			return report, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO product_service_grants
			(product_id,revision,brand_cloud_id,catalog_revision,options,bindings,snapshot_sha256,legacy)
			VALUES($1,1,$2,0,$3,$4,$5,true)`, product.productID, product.brandCloudID, optionsJSON, bindingsJSON, digest); err != nil {
			return report, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return report, err
	}
	report.Applied = len(missing)
	return report, nil
}

func knownLegacyProductOption(code string) bool {
	switch code {
	case "mqtt", "iot_shadow", "video_streaming", "video_storage", "device_logging":
		return true
	default:
		return false
	}
}

func (r *LegacyProductServiceGrantReport) addIssue(productID, reason string) {
	r.IssueCount++
	if len(r.Issues) < 100 {
		r.Issues = append(r.Issues, fmt.Sprintf("%s: %s", productID, reason))
	}
}
