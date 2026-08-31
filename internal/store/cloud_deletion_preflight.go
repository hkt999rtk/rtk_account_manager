package store

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"rtk_account_manager/internal/billinghandoff"
	"rtk_account_manager/internal/model"
)

type CloudDeletionBilling interface {
	CloudDeletionPreflight(context.Context, billinghandoff.CloudDeletionScope) (billinghandoff.CloudDeletionPreflight, error)
}
type CloudDeletionResourceScope struct {
	billinghandoff.CloudDeletionScope
	AuthorizationVersion int64
}
type CloudDeletionResourceEvidence struct {
	Scope                     CloudDeletionResourceScope
	Complete                  bool
	ReceiptID, EvidenceSHA256 string
	Blockers                  []string
	ObservedAt, ExpiresAt     time.Time
}

// Each adapter must verify its service's resource/job inventory and checkpoint.
// A successful health request or empty page is never a completeness receipt.
type CloudDeletionResourceObserver interface {
	ObserveCloudDeletion(context.Context, CloudDeletionResourceScope) (CloudDeletionResourceEvidence, error)
}
type CloudDeletionPreflightOptions struct {
	Billing CloudDeletionBilling
	// Explicit reviewed inventory. Absent adapters block eligibility; registering
	// this map does not replace production completeness/coverage verification.
	Resources map[string]CloudDeletionResourceObserver
}

func (s *Store) ConfigureCloudDeletionPreflight(in CloudDeletionPreflightOptions) error {
	if s.deletionPreflight != nil || in.Billing == nil {
		return ErrConflict
	}
	copy := CloudDeletionPreflightOptions{Billing: in.Billing, Resources: map[string]CloudDeletionResourceObserver{}}
	for name, observer := range in.Resources {
		if name == "billing" || !handoffParticipantName.MatchString(name) || observer == nil {
			return ErrConflict
		}
		copy.Resources[name] = observer
	}
	s.deletionPreflight = &copy
	return nil
}

type CloudDeletionPreflight struct {
	Eligible bool                 `json:"eligible"`
	Blockers []model.CloudBlocker `json:"blockers"`
}
type deletionLocalSnapshot struct {
	scope                   CloudDeletionResourceScope
	products, devices, jobs int64
	lifecycleConflict       bool
}

func (s *Store) deletionLocalSnapshot(ctx context.Context, userID, cloudID string) (deletionLocalSnapshot, error) {
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return deletionLocalSnapshot{}, err
	}
	defer tx.Rollback(ctx)
	snap := deletionLocalSnapshot{scope: CloudDeletionResourceScope{CloudDeletionScope: billinghandoff.CloudDeletionScope{CloudID: cloudID, OwnerUserID: userID}}}
	err = tx.QueryRow(ctx, `SELECT o.ownership_version,o.authorization_version,
        EXISTS(SELECT 1 FROM cloud_ownership_handoffs h WHERE h.brand_cloud_id=o.id AND h.phase NOT IN ('canceled','succeeded'))
        FROM organizations o JOIN organization_members m ON m.organization_id=o.id AND m.role='owner'
        WHERE o.id::text=$1 AND m.user_id::text=$2 AND o.organization_kind='brand_cloud' AND o.deleted_at IS NULL
          AND user_can_access_brand_cloud_without_handoff($2,$1)`, cloudID, userID).Scan(&snap.scope.OwnershipVersion, &snap.scope.AuthorizationVersion, &snap.lifecycleConflict)
	if errors.Is(err, pgx.ErrNoRows) {
		return snap, ErrNotFound
	}
	if err != nil {
		return snap, err
	}
	// Disabled Product/device rows are not tombstones. Unprovisioned device
	// history resides in operations/audit, not in the live devices table.
	err = tx.QueryRow(ctx, `SELECT
        (SELECT count(*) FROM device_item_profiles WHERE brand_cloud_id::text=$1),
        (SELECT count(*) FROM devices WHERE organization_id::text=$1),
        (SELECT count(*) FROM device_operations WHERE organization_id::text=$1 AND (status NOT IN ('succeeded','failed') OR completed_at IS NULL))
        + (SELECT count(*) FROM factory_production_runs WHERE brand_cloud_id::text=$1 AND status='active' AND valid_until>now())`, cloudID).Scan(&snap.products, &snap.devices, &snap.jobs)
	if err != nil {
		return snap, err
	}
	return snap, tx.Commit(ctx)
}

func (s *Store) PreflightDeveloperBrandCloudDeletion(ctx context.Context, userID, cloudID string) (CloudDeletionPreflight, error) {
	initial, err := s.deletionLocalSnapshot(ctx, userID, cloudID)
	if err != nil {
		return CloudDeletionPreflight{}, err
	}
	// Do not hold an AM transaction across dependency I/O. Recheck authorization
	// and local resource state after observing the dependencies.
	dependencyCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	dependencyBlockers := map[string]model.CloudBlocker{}
	add := func(code string) {
		dependencyBlockers[code] = model.CloudBlocker{Code: code, Retryable: code == "evidence_unavailable" || code == "usage_unsettled" || code == "jobs_running"}
	}
	var financial *billinghandoff.CloudDeletionPreflight
	var evidenceExpiry time.Time
	if s.deletionPreflight == nil {
		add("evidence_unavailable")
	} else {
		result, err := s.deletionPreflight.Billing.CloudDeletionPreflight(dependencyCtx, initial.scope.CloudDeletionScope)
		if err != nil || billinghandoff.ValidateCloudDeletionPreflight(initial.scope.CloudDeletionScope, result, time.Now()) != nil {
			add("evidence_unavailable")
		} else {
			financial = &result
			for _, code := range result.Blockers {
				add(code)
			}
		}
		if len(s.deletionPreflight.Resources) == 0 {
			add("evidence_unavailable")
		}
		names := make([]string, 0, len(s.deletionPreflight.Resources))
		for name := range s.deletionPreflight.Resources {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if dependencyCtx.Err() != nil {
				add("evidence_unavailable")
				break
			}
			evidence, err := s.deletionPreflight.Resources[name].ObserveCloudDeletion(dependencyCtx, initial.scope)
			now := time.Now()
			if err != nil || evidence.Scope != initial.scope || !evidence.Complete || evidence.ReceiptID == "" || !handoffEvidenceDigest.MatchString(evidence.EvidenceSHA256) || evidence.Blockers == nil ||
				evidence.ObservedAt.After(now) || evidence.ObservedAt.Before(now.Add(-5*time.Minute)) || !evidence.ExpiresAt.After(now) || evidence.ExpiresAt.After(evidence.ObservedAt.Add(5*time.Minute)) {
				add("evidence_unavailable")
				continue
			}
			if evidenceExpiry.IsZero() || evidence.ExpiresAt.Before(evidenceExpiry) {
				evidenceExpiry = evidence.ExpiresAt
			}
			for _, code := range evidence.Blockers {
				switch code {
				case "products_present", "devices_present", "jobs_running", "lifecycle_conflict", "evidence_unavailable":
					add(code)
				default:
					add("evidence_unavailable")
				}
			}
		}
	}
	final, err := s.deletionLocalSnapshot(ctx, userID, cloudID)
	if err != nil {
		return CloudDeletionPreflight{}, err
	}
	if final.scope != initial.scope {
		dependencyBlockers = map[string]model.CloudBlocker{}
		add("evidence_unavailable")
		financial = nil
	}
	if financial != nil && !financial.ExpiresAt.After(time.Now()) {
		add("evidence_unavailable")
	}
	if !evidenceExpiry.IsZero() && !evidenceExpiry.After(time.Now()) {
		add("evidence_unavailable")
	}
	out := CloudDeletionPreflight{Blockers: []model.CloudBlocker{}}
	for _, entry := range []struct {
		code  string
		count int64
	}{{"products_present", final.products}, {"devices_present", final.devices}, {"jobs_running", final.jobs}} {
		if entry.count > 0 {
			if _, remote := dependencyBlockers[entry.code]; !remote {
				count := entry.count
				dependencyBlockers[entry.code] = model.CloudBlocker{Code: entry.code, Retryable: entry.code == "jobs_running", Count: &count}
			}
		}
	}
	if final.lifecycleConflict {
		add("lifecycle_conflict")
	}
	if financial != nil && financial.BalanceMinor != 0 {
		amount := financial.BalanceMinor
		blocker := dependencyBlockers["balance_nonzero"]
		blocker.BalanceMinor = &amount
		dependencyBlockers["balance_nonzero"] = blocker
	}
	for _, code := range []string{"products_present", "devices_present", "jobs_running", "balance_nonzero", "usage_unsettled", "debt_outstanding", "payment_pending", "refund_pending", "dispute_pending", "lifecycle_conflict", "evidence_unavailable"} {
		if blocker, exists := dependencyBlockers[code]; exists {
			out.Blockers = append(out.Blockers, blocker)
		}
	}
	out.Eligible = len(out.Blockers) == 0
	return out, nil
}
