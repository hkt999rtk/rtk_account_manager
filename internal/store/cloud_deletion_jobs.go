package store

import (
	"context"
	"time"
)

type CloudDeletionJob struct{ OperationID, CloudID, LeaseID string }

// Scheduling never supplies closure evidence. Expired leases may overlap remote
// delivery, which must reuse the persisted request and close-command IDs.
func (s *Store) ClaimCloudDeletionJobs(ctx context.Context, limit int, lease time.Duration) ([]CloudDeletionJob, error) {
	if limit < 1 || limit > 128 || lease < 30*time.Second || lease > 5*time.Minute {
		return nil, ErrConflict
	}
	rows, err := s.db.Query(ctx, `WITH candidates AS (
        SELECT j.operation_id FROM cloud_deletion_jobs j JOIN cloud_deletion_operations d ON d.id=j.operation_id
        WHERE d.phase NOT IN ('succeeded','canceled') AND j.available_at<=clock_timestamp() AND (j.lease_until IS NULL OR j.lease_until<=clock_timestamp())
        ORDER BY j.available_at,j.operation_id LIMIT $1 FOR UPDATE OF j SKIP LOCKED
    ), claimed AS (
        UPDATE cloud_deletion_jobs j SET lease_id=gen_random_uuid(),lease_until=clock_timestamp()+$2*interval '1 millisecond',attempts=attempts+1
        FROM candidates c WHERE j.operation_id=c.operation_id RETURNING j.operation_id,j.lease_id
    ) SELECT c.operation_id::text,d.brand_cloud_id::text,c.lease_id::text FROM claimed c JOIN cloud_deletion_operations d ON d.id=c.operation_id`, limit, lease.Milliseconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := []CloudDeletionJob{}
	for rows.Next() {
		var job CloudDeletionJob
		if err = rows.Scan(&job.OperationID, &job.CloudID, &job.LeaseID); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}
func (s *Store) ProcessCloudDeletionJob(ctx context.Context, job CloudDeletionJob) (CloudDeletionOperation, error) {
	var valid bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM cloud_deletion_jobs j JOIN cloud_deletion_operations d ON d.id=j.operation_id WHERE j.operation_id::text=$1 AND d.brand_cloud_id::text=$2 AND j.lease_id::text=$3 AND j.lease_until>clock_timestamp())`, job.OperationID, job.CloudID, job.LeaseID).Scan(&valid)
	if err != nil {
		return CloudDeletionOperation{}, err
	}
	if !valid {
		return CloudDeletionOperation{}, ErrHandoffLeaseLost
	}
	return s.AdvanceCloudDeletion(ctx, job.CloudID, job.OperationID)
}
func (s *Store) FinishCloudDeletionJob(ctx context.Context, job CloudDeletionJob, delay time.Duration) (bool, error) {
	if delay < 0 || delay > time.Hour {
		return false, ErrConflict
	}
	tag, err := s.db.Exec(ctx, `UPDATE cloud_deletion_jobs j SET lease_id=NULL,lease_until=NULL,available_at=clock_timestamp()+$4*interval '1 millisecond'
        WHERE operation_id::text=$1 AND lease_id::text=$3 AND lease_until>clock_timestamp() AND EXISTS(SELECT 1 FROM cloud_deletion_operations d WHERE d.id=j.operation_id AND d.brand_cloud_id::text=$2)`, job.OperationID, job.CloudID, job.LeaseID, delay.Milliseconds())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
