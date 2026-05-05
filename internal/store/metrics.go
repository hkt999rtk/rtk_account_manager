package store

import (
	"context"
)

type EvaluationQuotaUsage struct {
	OrganizationID        string
	OrganizationName      string
	ActiveDevices         int
	EvaluationDeviceQuota int
}

func (s *Store) CountEvaluationSignupEvents(ctx context.Context) (evaluation int64, commercial int64, err error) {
	err = s.db.QueryRow(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE payload->>'organization_tier' = 'evaluation'),
			COUNT(*) FILTER (WHERE payload->>'organization_tier' = 'commercial')
		FROM audit_events
		WHERE event_type = 'signup_created'
	`).Scan(&evaluation, &commercial)
	return evaluation, commercial, err
}

func (s *Store) CountEmailVerificationEventsFromSignup(ctx context.Context) (int64, error) {
	var total int64
	err := s.db.QueryRow(ctx, `
		SELECT count(*)
		FROM audit_events
		WHERE event_type = 'email_verified'
		  AND payload->>'signup_pending_verification' = 'true'
	`).Scan(&total)
	return total, err
}

func (s *Store) CountQuotaRaiseRequestStatuses(ctx context.Context) (pending, approved, declined int64, err error) {
	err = s.db.QueryRow(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE status = 'pending'),
			COUNT(*) FILTER (WHERE status = 'approved'),
			COUNT(*) FILTER (WHERE status = 'declined')
		FROM quota_raise_requests
	`).Scan(&pending, &approved, &declined)
	return pending, approved, declined, err
}

func (s *Store) ListEvaluationQuotaUsage(ctx context.Context) ([]EvaluationQuotaUsage, error) {
	rows, err := s.db.Query(ctx, `
		SELECT
			o.id::text,
			o.name,
			COUNT(d.id)::int,
			o.evaluation_device_quota
		FROM organizations o
		LEFT JOIN devices d
			ON d.organization_id = o.id
		   AND d.disabled_at IS NULL
		WHERE o.tier = 'evaluation'
		GROUP BY o.id, o.name, o.evaluation_device_quota, o.created_at
		ORDER BY o.created_at ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	usages := []EvaluationQuotaUsage{}
	for rows.Next() {
		var usage EvaluationQuotaUsage
		if err := rows.Scan(&usage.OrganizationID, &usage.OrganizationName, &usage.ActiveDevices, &usage.EvaluationDeviceQuota); err != nil {
			return nil, err
		}
		usages = append(usages, usage)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return usages, nil
}

func (u EvaluationQuotaUsage) Utilization() float64 {
	if u.EvaluationDeviceQuota <= 0 {
		return 0
	}
	return float64(u.ActiveDevices) / float64(u.EvaluationDeviceQuota)
}
