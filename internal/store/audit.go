package store

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"rtk_account_manager/internal/model"
)

type AuditEventInput struct {
	EventType      string
	ActorUserID    *string
	OrganizationID *string
	SubjectType    string
	SubjectID      string
	Payload        map[string]any
}

type AuditEventListFilter struct {
	EventType   string
	SubjectType string
	Limit       int
	Offset      int
}

func createAuditEventTx(ctx context.Context, tx pgx.Tx, in AuditEventInput) error {
	payload := []byte(`{}`)
	if len(in.Payload) > 0 {
		raw, err := json.Marshal(in.Payload)
		if err != nil {
			return err
		}
		payload = raw
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO audit_events (
			event_type,
			actor_user_id,
			organization_id,
			subject_type,
			subject_id,
			payload
		)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, in.EventType, in.ActorUserID, in.OrganizationID, in.SubjectType, in.SubjectID, payload)
	return err
}

func (s *Store) CreateAuditEvent(ctx context.Context, in AuditEventInput) error {
	payload := []byte(`{}`)
	if len(in.Payload) > 0 {
		raw, err := json.Marshal(in.Payload)
		if err != nil {
			return err
		}
		payload = raw
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO audit_events (
			event_type,
			actor_user_id,
			organization_id,
			subject_type,
			subject_id,
			payload
		)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, in.EventType, in.ActorUserID, in.OrganizationID, in.SubjectType, in.SubjectID, payload)
	return err
}

func (s *Store) ListAuditEvents(ctx context.Context, in AuditEventListFilter) (AuditEventPage, error) {
	var total int
	if err := s.db.QueryRow(ctx, `
		SELECT count(*)::int
		FROM audit_events
		WHERE ($1 = '' OR event_type = $1)
			AND ($2 = '' OR subject_type = $2)
	`, in.EventType, in.SubjectType).Scan(&total); err != nil {
		return AuditEventPage{}, err
	}
	rows, err := s.db.Query(ctx, `
		SELECT
			id::text,
			event_type,
			actor_user_id::text,
			organization_id::text,
			subject_type,
			subject_id,
			payload,
			created_at,
			updated_at
		FROM audit_events
		WHERE ($1 = '' OR event_type = $1)
			AND ($2 = '' OR subject_type = $2)
		ORDER BY created_at ASC
		LIMIT $3 OFFSET $4
	`, in.EventType, in.SubjectType, in.Limit, in.Offset)
	if err != nil {
		return AuditEventPage{}, err
	}
	defer rows.Close()

	events := []model.AuditEvent{}
	for rows.Next() {
		var event model.AuditEvent
		var rawPayload []byte
		if err := rows.Scan(
			&event.ID,
			&event.EventType,
			&event.ActorUserID,
			&event.OrganizationID,
			&event.SubjectType,
			&event.SubjectID,
			&rawPayload,
			&event.CreatedAt,
			&event.UpdatedAt,
		); err != nil {
			return AuditEventPage{}, err
		}
		if err := json.Unmarshal(rawPayload, &event.Payload); err != nil {
			return AuditEventPage{}, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return AuditEventPage{}, err
	}
	return AuditEventPage{Events: events, Page: Page{Limit: in.Limit, Offset: in.Offset, Total: total}}, nil
}
