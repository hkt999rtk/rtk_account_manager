package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"rtk_account_manager/internal/model"
)

type QuotaRaiseRequestInput struct {
	OrganizationID string
	RequestedBy    string
	RequestedQuota int
	UseCase        string
	ContactInfo    map[string]any
}

type QuotaRaiseDecisionInput struct {
	RequestID      string
	DecidedBy      string
	ApprovedQuota  int
	DecisionReason *string
	Approved       bool
}

func (s *Store) IsPlatformAdmin(ctx context.Context, userID string) (bool, error) {
	var platformAdmin bool
	err := s.db.QueryRow(ctx, `
		SELECT COALESCE(platform_admin, false)
		FROM users
		WHERE id = $1 AND disabled_at IS NULL
	`, userID).Scan(&platformAdmin)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	return platformAdmin, err
}

func (s *Store) CreateQuotaRaiseRequest(ctx context.Context, in QuotaRaiseRequestInput) (model.QuotaRaiseRequest, error) {
	contactInfo, err := json.Marshal(in.ContactInfo)
	if err != nil {
		return model.QuotaRaiseRequest{}, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.QuotaRaiseRequest{}, err
	}
	defer tx.Rollback(ctx)

	var request model.QuotaRaiseRequest
	var rawContactInfo []byte
	err = tx.QueryRow(ctx, `
		INSERT INTO quota_raise_requests (
			organization_id,
			requested_by,
			requested_quota,
			use_case,
			contact_info
		)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING
			id::text,
			organization_id::text,
			requested_by::text,
			requested_quota,
			use_case,
			contact_info,
			status,
			decided_by::text,
			decision_reason,
			created_at,
			updated_at,
			decided_at
	`, in.OrganizationID, in.RequestedBy, in.RequestedQuota, in.UseCase, contactInfo).Scan(
		&request.ID,
		&request.OrganizationID,
		&request.RequestedBy,
		&request.RequestedQuota,
		&request.UseCase,
		&rawContactInfo,
		&request.Status,
		&request.DecidedBy,
		&request.DecisionReason,
		&request.CreatedAt,
		&request.UpdatedAt,
		&request.DecidedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.QuotaRaiseRequest{}, ErrNotFound
	}
	if err == nil {
		if err := json.Unmarshal(rawContactInfo, &request.ContactInfo); err != nil {
			return model.QuotaRaiseRequest{}, err
		}
	}
	if err != nil {
		return model.QuotaRaiseRequest{}, err
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{
		EventType:      "quota_raise_requested",
		ActorUserID:    &in.RequestedBy,
		OrganizationID: &in.OrganizationID,
		SubjectType:    "quota_raise_request",
		SubjectID:      request.ID,
		Payload: map[string]any{
			"requested_quota": request.RequestedQuota,
			"use_case":        request.UseCase,
		},
	}); err != nil {
		return model.QuotaRaiseRequest{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.QuotaRaiseRequest{}, err
	}
	return request, err
}

func (s *Store) GetQuotaRaiseRequest(ctx context.Context, requestID string) (model.QuotaRaiseRequest, error) {
	var request model.QuotaRaiseRequest
	var rawContactInfo []byte
	err := s.db.QueryRow(ctx, `
		SELECT
			id::text,
			organization_id::text,
			requested_by::text,
			requested_quota,
			use_case,
			contact_info,
			status,
			decided_by::text,
			decision_reason,
			created_at,
			updated_at,
			decided_at
		FROM quota_raise_requests
		WHERE id = $1
	`, requestID).Scan(
		&request.ID,
		&request.OrganizationID,
		&request.RequestedBy,
		&request.RequestedQuota,
		&request.UseCase,
		&rawContactInfo,
		&request.Status,
		&request.DecidedBy,
		&request.DecisionReason,
		&request.CreatedAt,
		&request.UpdatedAt,
		&request.DecidedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.QuotaRaiseRequest{}, ErrNotFound
	}
	if err == nil {
		if err := json.Unmarshal(rawContactInfo, &request.ContactInfo); err != nil {
			return model.QuotaRaiseRequest{}, err
		}
	}
	return request, err
}

func (s *Store) DecideQuotaRaiseRequest(ctx context.Context, in QuotaRaiseDecisionInput) (model.QuotaRaiseRequest, model.Organization, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.QuotaRaiseRequest{}, model.Organization{}, err
	}
	defer tx.Rollback(ctx)

	var request model.QuotaRaiseRequest
	var org model.Organization
	var rawContactInfo []byte
	err = tx.QueryRow(ctx, `
		SELECT
			q.id::text,
			q.organization_id::text,
			q.requested_by::text,
			q.requested_quota,
			q.use_case,
			q.contact_info,
			q.status,
			q.decided_by::text,
			q.decision_reason,
			q.created_at,
			q.updated_at,
			q.decided_at,
			o.id::text,
			o.name,
			o.tier,
			o.evaluation_device_quota,
			o.created_at,
			o.updated_at
		FROM quota_raise_requests q
		JOIN organizations o ON o.id = q.organization_id
		WHERE q.id = $1
		FOR UPDATE OF q, o
	`, in.RequestID).Scan(
		&request.ID,
		&request.OrganizationID,
		&request.RequestedBy,
		&request.RequestedQuota,
		&request.UseCase,
		&rawContactInfo,
		&request.Status,
		&request.DecidedBy,
		&request.DecisionReason,
		&request.CreatedAt,
		&request.UpdatedAt,
		&request.DecidedAt,
		&org.ID,
		&org.Name,
		&org.Tier,
		&org.EvaluationDeviceQuota,
		&org.CreatedAt,
		&org.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.QuotaRaiseRequest{}, model.Organization{}, ErrNotFound
	}
	if err != nil {
		return model.QuotaRaiseRequest{}, model.Organization{}, err
	}
	if err := json.Unmarshal(rawContactInfo, &request.ContactInfo); err != nil {
		return model.QuotaRaiseRequest{}, model.Organization{}, err
	}
	if request.Status != model.QuotaRaiseRequestStatusPending {
		return model.QuotaRaiseRequest{}, model.Organization{}, ErrConflict
	}

	decision := model.QuotaRaiseRequestStatusDeclined
	approvedQuota := org.EvaluationDeviceQuota
	if in.Approved {
		decision = model.QuotaRaiseRequestStatusApproved
		limit := in.ApprovedQuota
		if limit <= 0 {
			limit = request.RequestedQuota
		}
		if limit > 200 {
			limit = 200
		}
		if limit > org.EvaluationDeviceQuota {
			approvedQuota = limit
		}
		_, err = tx.Exec(ctx, `
			UPDATE organizations
			SET evaluation_device_quota = $2, updated_at = now()
			WHERE id = $1
		`, org.ID, approvedQuota)
		if err != nil {
			return model.QuotaRaiseRequest{}, model.Organization{}, err
		}
	}

	eventType := "quota_raise_declined"
	payload := map[string]any{
		"request_id":      request.ID,
		"requested_quota": request.RequestedQuota,
	}
	if in.Approved {
		eventType = "quota_raise_approved"
		payload["approved_quota"] = approvedQuota
	} else if in.DecisionReason != nil {
		payload["decision_reason"] = *in.DecisionReason
	}

	now := time.Now().UTC()
	tag, err := tx.Exec(ctx, `
		UPDATE quota_raise_requests
		SET status = $2,
		    decided_by = $3,
		    decision_reason = $4,
		    decided_at = $5,
		    updated_at = now()
		WHERE id = $1
	`, request.ID, decision, in.DecidedBy, in.DecisionReason, now)
	if err != nil {
		return model.QuotaRaiseRequest{}, model.Organization{}, err
	}
	if tag.RowsAffected() == 0 {
		return model.QuotaRaiseRequest{}, model.Organization{}, ErrNotFound
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{
		EventType:      eventType,
		ActorUserID:    &in.DecidedBy,
		OrganizationID: &org.ID,
		SubjectType:    "quota_raise_request",
		SubjectID:      request.ID,
		Payload:        payload,
	}); err != nil {
		return model.QuotaRaiseRequest{}, model.Organization{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.QuotaRaiseRequest{}, model.Organization{}, err
	}
	request.Status = decision
	request.DecidedBy = &in.DecidedBy
	request.DecisionReason = in.DecisionReason
	request.DecidedAt = &now
	org.EvaluationDeviceQuota = approvedQuota
	return request, org, nil
}
