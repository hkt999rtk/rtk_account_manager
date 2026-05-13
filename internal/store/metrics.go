package store

import (
	"context"
	"errors"
	"time"
)

type EvaluationQuotaUsage struct {
	OrganizationID        string
	OrganizationName      string
	ActiveDevices         int
	EvaluationDeviceQuota int
}

type LifecycleMetrics struct {
	Outbox     LifecycleMessageMetrics
	Inbox      LifecycleMessageMetrics
	Operations LifecycleOperationMetrics
}

type LifecycleMessageMetrics struct {
	ByStatus            map[string]int64
	DeadLetteredByError []LifecycleMessageErrorCount
	LastCompletedAt     *time.Time
}

type LifecycleMessageErrorCount struct {
	MessageType string
	ErrorCode   string
	Count       int64
}

type LifecycleOperationMetrics struct {
	ByStatus                map[string]int64
	ByTypeAndStatus         []LifecycleOperationStatusCount
	OldestActiveAgeSeconds  int64
	LastTerminalCompletedAt *time.Time
}

type LifecycleOperationStatusCount struct {
	OperationType string
	Status        string
	Count         int64
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

func (s *Store) GetLifecycleMetrics(ctx context.Context) (LifecycleMetrics, error) {
	outboxByStatus, err := s.countByStatus(ctx, "device_message_outbox")
	if err != nil {
		return LifecycleMetrics{}, err
	}
	inboxByStatus, err := s.countByStatus(ctx, "device_message_inbox")
	if err != nil {
		return LifecycleMetrics{}, err
	}
	operationByStatus, err := s.countByStatus(ctx, "device_operations")
	if err != nil {
		return LifecycleMetrics{}, err
	}
	outboxDeadLetters, err := s.countOutboxDeadLettersByError(ctx)
	if err != nil {
		return LifecycleMetrics{}, err
	}
	inboxDeadLetters, err := s.countInboxDeadLettersByError(ctx)
	if err != nil {
		return LifecycleMetrics{}, err
	}
	operationTypeStatuses, err := s.countOperationsByTypeAndStatus(ctx)
	if err != nil {
		return LifecycleMetrics{}, err
	}

	var outboxLastPublishedAt *time.Time
	if err := s.db.QueryRow(ctx, `SELECT MAX(published_at) FROM device_message_outbox`).Scan(&outboxLastPublishedAt); err != nil {
		return LifecycleMetrics{}, err
	}
	var inboxLastProcessedAt *time.Time
	if err := s.db.QueryRow(ctx, `SELECT MAX(processed_at) FROM device_message_inbox`).Scan(&inboxLastProcessedAt); err != nil {
		return LifecycleMetrics{}, err
	}
	var oldestActiveAgeSeconds int64
	var lastTerminalCompletedAt *time.Time
	if err := s.db.QueryRow(ctx, `
		SELECT
			COALESCE(EXTRACT(EPOCH FROM now() - MIN(created_at) FILTER (WHERE status IN ('pending', 'published', 'retrying'))), 0)::bigint,
			MAX(completed_at)
		FROM device_operations
	`).Scan(&oldestActiveAgeSeconds, &lastTerminalCompletedAt); err != nil {
		return LifecycleMetrics{}, err
	}

	return LifecycleMetrics{
		Outbox: LifecycleMessageMetrics{
			ByStatus:            outboxByStatus,
			DeadLetteredByError: outboxDeadLetters,
			LastCompletedAt:     outboxLastPublishedAt,
		},
		Inbox: LifecycleMessageMetrics{
			ByStatus:            inboxByStatus,
			DeadLetteredByError: inboxDeadLetters,
			LastCompletedAt:     inboxLastProcessedAt,
		},
		Operations: LifecycleOperationMetrics{
			ByStatus:                operationByStatus,
			ByTypeAndStatus:         operationTypeStatuses,
			OldestActiveAgeSeconds:  oldestActiveAgeSeconds,
			LastTerminalCompletedAt: lastTerminalCompletedAt,
		},
	}, nil
}

func (s *Store) countByStatus(ctx context.Context, table string) (map[string]int64, error) {
	switch table {
	case "device_message_outbox", "device_message_inbox", "device_operations":
	default:
		return nil, errors.New("invalid lifecycle metrics table")
	}

	rows, err := s.db.Query(ctx, `SELECT status, count(*) FROM `+table+` GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := map[string]int64{}
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		counts[status] = count
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return counts, nil
}

func (s *Store) countOutboxDeadLettersByError(ctx context.Context) ([]LifecycleMessageErrorCount, error) {
	return s.countMessageDeadLettersByError(ctx, "device_message_outbox")
}

func (s *Store) countInboxDeadLettersByError(ctx context.Context) ([]LifecycleMessageErrorCount, error) {
	return s.countMessageDeadLettersByError(ctx, "device_message_inbox")
}

func (s *Store) countMessageDeadLettersByError(ctx context.Context, table string) ([]LifecycleMessageErrorCount, error) {
	switch table {
	case "device_message_outbox", "device_message_inbox":
	default:
		return nil, errors.New("invalid lifecycle metrics table")
	}

	rows, err := s.db.Query(ctx, `
		SELECT message_type, COALESCE(last_error, ''), count(*)
		FROM `+table+`
		WHERE status = 'dead_lettered'
		GROUP BY message_type, COALESCE(last_error, '')
		ORDER BY count(*) DESC, message_type ASC, COALESCE(last_error, '') ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := []LifecycleMessageErrorCount{}
	for rows.Next() {
		var count LifecycleMessageErrorCount
		if err := rows.Scan(&count.MessageType, &count.ErrorCode, &count.Count); err != nil {
			return nil, err
		}
		counts = append(counts, count)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return counts, nil
}

func (s *Store) countOperationsByTypeAndStatus(ctx context.Context) ([]LifecycleOperationStatusCount, error) {
	rows, err := s.db.Query(ctx, `
		SELECT operation_type, status, count(*)
		FROM device_operations
		GROUP BY operation_type, status
		ORDER BY operation_type ASC, status ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := []LifecycleOperationStatusCount{}
	for rows.Next() {
		var count LifecycleOperationStatusCount
		if err := rows.Scan(&count.OperationType, &count.Status, &count.Count); err != nil {
			return nil, err
		}
		counts = append(counts, count)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return counts, nil
}

func (u EvaluationQuotaUsage) Utilization() float64 {
	if u.EvaluationDeviceQuota <= 0 {
		return 0
	}
	return float64(u.ActiveDevices) / float64(u.EvaluationDeviceQuota)
}
