package store

import (
	"context"
	"errors"
	"time"
)

type HandoffJob struct {
	OperationID, CloudID, LeaseID, Phase string
	Generation                           int64
	RetryCount                           int
	LeaseUntil                           time.Time
}

// Database time governs leases, not worker clocks. SKIP LOCKED partitions work;
// expired workers may overlap remote delivery, so protocol IDs remain idempotent.
func (s *Store) ClaimHandoffJobs(ctx context.Context, limit int, lease time.Duration) ([]HandoffJob, error) {
	if limit < 1 || limit > 128 || lease < 30*time.Second || lease > 5*time.Minute {
		return nil, ErrConflict
	}
	rows, err := s.db.Query(ctx, `WITH candidates AS (
		SELECT j.operation_id FROM cloud_handoff_jobs j JOIN cloud_ownership_handoffs h ON h.id=j.operation_id
		WHERE h.phase NOT IN ('succeeded','canceled') AND j.available_at<=clock_timestamp()
		AND (j.lease_until IS NULL OR j.lease_until<=clock_timestamp())
		ORDER BY j.available_at,j.operation_id LIMIT $1 FOR UPDATE OF j SKIP LOCKED
	), claimed AS (
		UPDATE cloud_handoff_jobs j SET lease_id=gen_random_uuid(),lease_until=clock_timestamp()+$2*interval '1 millisecond',
		claim_count=claim_count+1,updated_at=clock_timestamp() FROM candidates c WHERE j.operation_id=c.operation_id RETURNING j.*
	) SELECT c.operation_id::text,h.brand_cloud_id::text,c.lease_id::text,h.phase,c.generation,c.retry_count,c.lease_until
	FROM claimed c JOIN cloud_ownership_handoffs h ON h.id=c.operation_id`, limit, lease.Milliseconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := []HandoffJob{}
	for rows.Next() {
		var job HandoffJob
		if err := rows.Scan(&job.OperationID, &job.CloudID, &job.LeaseID, &job.Phase, &job.Generation, &job.RetryCount, &job.LeaseUntil); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

var ErrHandoffLeaseLost = errors.New("handoff worker lease lost")

func (s *Store) checkHandoffLease(ctx context.Context, job HandoffJob) error {
	var valid bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM cloud_handoff_jobs j JOIN cloud_ownership_handoffs h ON h.id=j.operation_id
		WHERE j.operation_id::text=$1 AND h.brand_cloud_id::text=$2 AND j.lease_id::text=$3 AND j.lease_until>clock_timestamp())`, job.OperationID, job.CloudID, job.LeaseID).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return ErrHandoffLeaseLost
	}
	return nil
}

// No timeout/max-attempt transition changes protocol phase or clears a hold.
// A newer phase/receipt wakeup wins over this attempt's requested backoff.
func (s *Store) FinishHandoffJob(ctx context.Context, job HandoffJob, outcome string, retry bool, delay time.Duration) (bool, error) {
	if delay < 0 || delay > time.Hour {
		return false, ErrConflict
	}
	switch outcome {
	case "progress", "waiting_confirmation", "waiting_preparation", "waiting_release", "evidence_unavailable", "participant_unavailable", "balance_negative", "financial_blocked", "lifecycle_conflict", "quota_exceeded", "complete":
	default:
		return false, ErrConflict
	}
	result, err := s.db.Exec(ctx, `UPDATE cloud_handoff_jobs j SET available_at=CASE WHEN generation=$4 THEN clock_timestamp()+$5*interval '1 millisecond' ELSE clock_timestamp() END,
		lease_id=NULL,lease_until=NULL,retry_count=CASE WHEN $6 THEN LEAST(retry_count+1,1000000) ELSE 0 END,last_outcome=$7,updated_at=clock_timestamp()
		WHERE operation_id::text=$1 AND lease_id::text=$2 AND lease_until>clock_timestamp()
		AND EXISTS(SELECT 1 FROM cloud_ownership_handoffs WHERE id=j.operation_id AND brand_cloud_id::text=$3)`, job.OperationID, job.LeaseID, job.CloudID, job.Generation, delay.Milliseconds(), retry, outcome)
	if err != nil {
		return false, err
	}
	return result.RowsAffected() == 1, nil
}
