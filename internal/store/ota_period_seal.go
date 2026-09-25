package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrOTAPeriodSealInvalid = errors.New("invalid OTA period seal scope")

// OTAPeriodGrantSeal is the Platform half of Billing's OTA source-completeness
// checkpoint. It records grant history only; OTA usage facts belong to the OTA
// producer's independent seal.
type OTAPeriodGrantSeal struct {
	OrganizationID  string           `json:"organization_id"`
	PeriodStart     time.Time        `json:"period_start"`
	PeriodEnd       time.Time        `json:"period_end"`
	IssuerKind      string           `json:"issuer_kind"`
	SealID          string           `json:"seal_id"`
	SourceHighWater json.RawMessage  `json:"source_high_water"`
	ProductIDs      []string         `json:"product_ids"`
	MetricCounts    map[string]int64 `json:"metric_counts"`
	SourceSHA256    string           `json:"source_sha256"`
	SealedAt        time.Time        `json:"sealed_at"`
}

type otaGrantHistoryRow struct {
	ProductID      string
	Revision       int64
	CreatedAt      time.Time
	Options        []string
	Bindings       []PlatformServiceCatalogOption
	SnapshotSHA256 string
}

// BuildOTAPeriodGrantSeal drains in-flight grant inserts before reading the
// completed UTC month. New inserts use database insertion time, so a writer
// that resumes after the fence cannot backdate the sealed Product set.
func (s *Store) BuildOTAPeriodGrantSeal(ctx context.Context, organizationID string, start, end time.Time) (OTAPeriodGrantSeal, error) {
	if !billingCreationUUID(organizationID) || !validOTAMonth(start, end) {
		return OTAPeriodGrantSeal{}, ErrOTAPeriodSealInvalid
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return OTAPeriodGrantSeal{}, err
	}
	defer tx.Rollback(ctx)
	var requiredMigrationsReady bool
	if err := tx.QueryRow(ctx, `SELECT count(*)=2 FROM schema_migrations WHERE version IN (
		'088_product_service_grants_immutable.sql', '089_ota_grant_insert_time.sql'
	)`).Scan(&requiredMigrationsReady); err != nil {
		return OTAPeriodGrantSeal{}, err
	}
	if !requiredMigrationsReady {
		return OTAPeriodGrantSeal{}, ErrOTAPeriodSealInvalid
	}
	var databaseNow time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&databaseNow); err != nil {
		return OTAPeriodGrantSeal{}, err
	}
	if databaseNow.Before(end) {
		return OTAPeriodGrantSeal{}, ErrOTAPeriodSealInvalid
	}
	var organizationExists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organizations WHERE id=$1)`,
		organizationID).Scan(&organizationExists); err != nil {
		return OTAPeriodGrantSeal{}, err
	}
	if !organizationExists {
		return OTAPeriodGrantSeal{}, ErrNotFound
	}
	// SHARE conflicts with each INSERT's ROW EXCLUSIVE lock. After it has
	// drained current writers, the next READ COMMITTED statement sees their
	// committed rows; later inserts wait until this read is complete.
	if _, err := tx.Exec(ctx, `LOCK TABLE product_service_grants IN SHARE MODE`); err != nil {
		return OTAPeriodGrantSeal{}, err
	}
	rows, err := tx.Query(ctx, `
		SELECT product_id::text, revision, created_at, options, bindings, snapshot_sha256
		FROM product_service_grants
		WHERE brand_cloud_id=$1 AND created_at < $2
		ORDER BY product_id, revision
	`, organizationID, end)
	if err != nil {
		return OTAPeriodGrantSeal{}, err
	}
	history := make([]otaGrantHistoryRow, 0)
	for rows.Next() {
		var row otaGrantHistoryRow
		var rawOptions, rawBindings []byte
		if err := rows.Scan(&row.ProductID, &row.Revision, &row.CreatedAt, &rawOptions, &rawBindings, &row.SnapshotSHA256); err != nil {
			rows.Close()
			return OTAPeriodGrantSeal{}, err
		}
		if err := json.Unmarshal(rawOptions, &row.Options); err != nil {
			rows.Close()
			return OTAPeriodGrantSeal{}, ErrOTAPeriodSealInvalid
		}
		if err := json.Unmarshal(rawBindings, &row.Bindings); err != nil {
			rows.Close()
			return OTAPeriodGrantSeal{}, ErrOTAPeriodSealInvalid
		}
		_, _, digest, err := encodeProductServiceGrant(row.Options, row.Bindings)
		if err != nil || digest != row.SnapshotSHA256 {
			rows.Close()
			return OTAPeriodGrantSeal{}, ErrOTAPeriodSealInvalid
		}
		history = append(history, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return OTAPeriodGrantSeal{}, err
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return OTAPeriodGrantSeal{}, err
	}
	return newOTAPeriodGrantSeal(organizationID, start, end, history)
}

func validOTAMonth(start, end time.Time) bool {
	start = start.UTC()
	end = end.UTC()
	return start.Day() == 1 && start.Hour() == 0 && start.Minute() == 0 &&
		start.Second() == 0 && start.Nanosecond() == 0 && end.Equal(start.AddDate(0, 1, 0))
}

func newOTAPeriodGrantSeal(organizationID string, start, end time.Time, history []otaGrantHistoryRow) (OTAPeriodGrantSeal, error) {
	start, end = start.UTC(), end.UTC()
	if !billingCreationUUID(organizationID) || !validOTAMonth(start, end) {
		return OTAPeriodGrantSeal{}, ErrOTAPeriodSealInvalid
	}
	slices.SortFunc(history, func(a, b otaGrantHistoryRow) int {
		if a.ProductID < b.ProductID {
			return -1
		}
		if a.ProductID > b.ProductID {
			return 1
		}
		if a.Revision < b.Revision {
			return -1
		}
		if a.Revision > b.Revision {
			return 1
		}
		return 0
	})
	canonical := make([][]any, 0, len(history))
	products := make([]string, 0)
	for i, row := range history {
		if !billingCreationUUID(row.ProductID) || row.Revision <= 0 ||
			row.CreatedAt.IsZero() || !row.CreatedAt.Before(end) ||
			len(row.SnapshotSHA256) != 64 {
			return OTAPeriodGrantSeal{}, ErrOTAPeriodSealInvalid
		}
		options := slices.Clone(row.Options)
		slices.Sort(options)
		canonical = append(canonical, []any{row.ProductID, row.Revision,
			row.CreatedAt.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano),
			options, row.SnapshotSHA256})
		// Preserve the full revision chain even after OTA is removed. Stored
		// artifacts and already-issued CDN grants can generate later usage.
		if i+1 < len(history) && history[i+1].ProductID == row.ProductID {
			if history[i+1].Revision <= row.Revision || history[i+1].CreatedAt.Before(row.CreatedAt) {
				return OTAPeriodGrantSeal{}, ErrOTAPeriodSealInvalid
			}
		}
		if !slices.Contains(options, "ota") {
			continue
		}
		if len(products) == 0 || products[len(products)-1] != row.ProductID {
			products = append(products, row.ProductID)
		}
	}
	raw, err := json.Marshal(canonical)
	if err != nil {
		return OTAPeriodGrantSeal{}, err
	}
	sum := sha256.Sum256(raw)
	sourceDigest := hex.EncodeToString(sum[:])
	highWater, err := json.Marshal(struct {
		GrantRowCount      int    `json:"grant_row_count"`
		GrantHistorySHA256 string `json:"grant_history_sha256"`
	}{len(history), sourceDigest})
	if err != nil {
		return OTAPeriodGrantSeal{}, err
	}
	return OTAPeriodGrantSeal{
		OrganizationID: organizationID, PeriodStart: start, PeriodEnd: end,
		IssuerKind: "platform_grants", SealID: otaPeriodSealID(organizationID, start, end),
		SourceHighWater: highWater, ProductIDs: products, MetricCounts: map[string]int64{},
		SourceSHA256: sourceDigest, SealedAt: end,
	}, nil
}

func otaPeriodSealID(organizationID string, start, end time.Time) string {
	name := fmt.Sprintf("rtk-ota-platform-period-seal-v1\n%s\n%s\n%s",
		organizationID, start.UTC().Format(time.RFC3339Nano), end.UTC().Format(time.RFC3339Nano))
	sum := sha256.Sum256([]byte(name))
	id := sum[:16]
	id[6] = (id[6] & 0x0f) | 0x80 // RFC 9562 UUIDv8 custom SHA-256 identity.
	id[8] = (id[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:16])
}
