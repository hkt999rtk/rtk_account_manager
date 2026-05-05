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

func (s *Store) ListAuditEvents(ctx context.Context, limit, offset int) ([]model.AuditEvent, error) {
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
		ORDER BY created_at ASC
		LIMIT $1 OFFSET $2
	`, limit, offset)
	if err != nil {
		return nil, err
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
			return nil, err
		}
		if err := json.Unmarshal(rawPayload, &event.Payload); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return events, nil
}
