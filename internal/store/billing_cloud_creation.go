package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

type BillingCloudCreation struct {
	EventID          string    `json:"event_id"`
	CloudID          string    `json:"cloud_id"`
	OwnerUserID      string    `json:"owner_user_id"`
	OwnershipVersion int64     `json:"ownership_version"`
	OccurredAt       time.Time `json:"occurred_at"`
	EvidenceSHA256   string    `json:"evidence_sha256"`
}

func (e BillingCloudCreation) Digest() string {
	raw := strings.Join([]string{"brand-cloud-created-v1", e.EventID, e.CloudID, e.OwnerUserID, "1", e.OccurredAt.UTC().Format("2006-01-02T15:04:05.000000Z")}, "\n")
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

func billingCreationUUID(value string) bool {
	var id pgtype.UUID
	return id.Scan(value) == nil && id.Valid && id.Bytes != [16]byte{} && id.String() == value
}
func (e BillingCloudCreation) Valid() bool {
	return billingCreationUUID(e.EventID) && billingCreationUUID(e.CloudID) && billingCreationUUID(e.OwnerUserID) && e.OwnershipVersion == 1 && !e.OccurredAt.IsZero() && e.OccurredAt.Equal(e.OccurredAt.Truncate(time.Microsecond)) && e.EvidenceSHA256 == e.Digest()
}

type BillingCloudCreationJob struct {
	BillingCloudCreation
	LeaseID string
}
type BillingCloudCreationReceipt struct {
	BillingCloudCreation
	AccountID string `json:"account_id"`
}

func (s *Store) ClaimBillingCloudCreations(ctx context.Context) ([]BillingCloudCreationJob, error) {
	rows, err := s.db.Query(ctx, `WITH pending AS (
        SELECT event_id FROM brand_cloud_billing_creation_outbox WHERE delivered_at IS NULL AND available_at<=clock_timestamp()
        AND (lease_until IS NULL OR lease_until<=clock_timestamp()) ORDER BY available_at,event_id LIMIT 4 FOR UPDATE SKIP LOCKED
    ) UPDATE brand_cloud_billing_creation_outbox j SET lease_id=gen_random_uuid(),lease_until=clock_timestamp()+interval '2 minutes',attempts=LEAST(attempts+1,1000000)
      FROM pending p WHERE j.event_id=p.event_id RETURNING j.event_id::text,j.cloud_id::text,j.owner_user_id::text,j.ownership_version,j.occurred_at,j.lease_id::text`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []BillingCloudCreationJob
	for rows.Next() {
		var j BillingCloudCreationJob
		if err := rows.Scan(&j.EventID, &j.CloudID, &j.OwnerUserID, &j.OwnershipVersion, &j.OccurredAt, &j.LeaseID); err != nil {
			return nil, err
		}
		j.EvidenceSHA256 = j.Digest()
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// Only a fully authenticated exact Billing receipt can mark delivery. Lost
// replies and leases retry the immutable event; no source-owner lookup is used.
func (s *Store) FinishBillingCloudCreation(ctx context.Context, j BillingCloudCreationJob, r *BillingCloudCreationReceipt) (bool, error) {
	if r != nil && (!r.Valid() || r.EventID != j.EventID || r.CloudID != j.CloudID || r.OwnerUserID != j.OwnerUserID || r.OwnershipVersion != j.OwnershipVersion || !r.OccurredAt.Equal(j.OccurredAt) || r.EvidenceSHA256 != j.Digest() || !billingCreationUUID(r.AccountID)) {
		return false, ErrConflict
	}
	if r == nil {
		result, err := s.db.Exec(ctx, `UPDATE brand_cloud_billing_creation_outbox SET available_at=clock_timestamp()+interval '30 seconds',lease_id=NULL,lease_until=NULL WHERE event_id::text=$1 AND lease_id::text=$2 AND lease_until>clock_timestamp() AND delivered_at IS NULL`, j.EventID, j.LeaseID)
		return result.RowsAffected() == 1, err
	}
	result, err := s.db.Exec(ctx, `UPDATE brand_cloud_billing_creation_outbox SET delivered_at=clock_timestamp(),billing_account_id=$3,receipt_sha256=$4,lease_id=NULL,lease_until=NULL
        WHERE event_id::text=$1 AND lease_id::text=$2 AND lease_until>clock_timestamp() AND delivered_at IS NULL AND cloud_id::text=$5 AND owner_user_id::text=$6 AND ownership_version=$7 AND occurred_at=$8`, j.EventID, j.LeaseID, r.AccountID, r.EvidenceSHA256, j.CloudID, j.OwnerUserID, j.OwnershipVersion, j.OccurredAt)
	return result.RowsAffected() == 1, err
}
