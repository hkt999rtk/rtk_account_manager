package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"rtk_account_manager/internal/emaildelivery"
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
	EnqueueEmail   bool
}

type QuotaRaiseRequestListFilter struct {
	Status model.QuotaRaiseRequestStatus
	Limit  int
	Offset int
}

func (s *Store) IsPlatformAdmin(ctx context.Context, userID string) (bool, error) {
	var platformAdmin bool
	err := s.db.QueryRow(ctx, `
		SELECT COALESCE(platform_admin, false)
		FROM users
		WHERE id = $1 AND disabled_at IS NULL AND signup_pending_verification = false
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
	request, err := scanQuotaRaiseRequest(s.db.QueryRow(ctx, `
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
	`, requestID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.QuotaRaiseRequest{}, ErrNotFound
	}
	return request, err
}

func (s *Store) ListQuotaRaiseRequests(ctx context.Context, in QuotaRaiseRequestListFilter) (QuotaRaiseRequestPage, error) {
	var total int
	if err := s.db.QueryRow(ctx, `
		SELECT count(*)::int
		FROM quota_raise_requests
		WHERE ($1 = '' OR status = $1)
	`, in.Status).Scan(&total); err != nil {
		return QuotaRaiseRequestPage{}, err
	}
	rows, err := s.db.Query(ctx, `
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
		WHERE ($1 = '' OR status = $1)
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`, in.Status, in.Limit, in.Offset)
	if err != nil {
		return QuotaRaiseRequestPage{}, err
	}
	defer rows.Close()

	requests := []model.QuotaRaiseRequest{}
	for rows.Next() {
		request, err := scanQuotaRaiseRequest(rows)
		if err != nil {
			return QuotaRaiseRequestPage{}, err
		}
		requests = append(requests, request)
	}
	if err := rows.Err(); err != nil {
		return QuotaRaiseRequestPage{}, err
	}
	return QuotaRaiseRequestPage{Requests: requests, Page: Page{Limit: in.Limit, Offset: in.Offset, Total: total}}, nil
}

func scanQuotaRaiseRequest(row rowScanner) (model.QuotaRaiseRequest, error) {
	var request model.QuotaRaiseRequest
	var rawContactInfo []byte
	err := row.Scan(
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
	if err == nil {
		if err := json.Unmarshal(rawContactInfo, &request.ContactInfo); err != nil {
			return model.QuotaRaiseRequest{}, err
		}
	}
	return request, err
}

func (s *Store) DecideQuotaRaiseRequest(ctx context.Context, in QuotaRaiseDecisionInput) (model.QuotaRaiseRequest, model.Organization, model.User, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.QuotaRaiseRequest{}, model.Organization{}, model.User{}, err
	}
	defer tx.Rollback(ctx)

	var request model.QuotaRaiseRequest
	var org model.Organization
	var requester model.User
	var rawContactInfo []byte
	var rawOrgMetadata []byte
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
			o.organization_kind,
			o.status,
			o.tier,
			o.evaluation_device_quota,
			o.metadata,
			o.created_at,
			o.updated_at,
			u.id::text,
			u.email,
			u.display_name,
			u.email_verified,
			u.email_verified_at,
			u.signup_pending_verification,
			u.created_at,
			u.updated_at,
			u.disabled_at
		FROM quota_raise_requests q
		JOIN organizations o ON o.id = q.organization_id
		JOIN users u ON u.id = q.requested_by
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
		&org.OrganizationKind,
		&org.Status,
		&org.Tier,
		&org.EvaluationDeviceQuota,
		&rawOrgMetadata,
		&org.CreatedAt,
		&org.UpdatedAt,
		&requester.ID,
		&requester.Email,
		&requester.DisplayName,
		&requester.EmailVerified,
		&requester.EmailVerifiedAt,
		&requester.SignupPendingVerification,
		&requester.CreatedAt,
		&requester.UpdatedAt,
		&requester.DisabledAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.QuotaRaiseRequest{}, model.Organization{}, model.User{}, ErrNotFound
	}
	if err != nil {
		return model.QuotaRaiseRequest{}, model.Organization{}, model.User{}, err
	}
	if err := json.Unmarshal(rawContactInfo, &request.ContactInfo); err != nil {
		return model.QuotaRaiseRequest{}, model.Organization{}, model.User{}, err
	}
	if err := json.Unmarshal(rawOrgMetadata, &org.Metadata); err != nil {
		return model.QuotaRaiseRequest{}, model.Organization{}, model.User{}, err
	}
	if request.Status != model.QuotaRaiseRequestStatusPending {
		return model.QuotaRaiseRequest{}, model.Organization{}, model.User{}, ErrConflict
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
			return model.QuotaRaiseRequest{}, model.Organization{}, model.User{}, err
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
		return model.QuotaRaiseRequest{}, model.Organization{}, model.User{}, err
	}
	if tag.RowsAffected() == 0 {
		return model.QuotaRaiseRequest{}, model.Organization{}, model.User{}, ErrNotFound
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{
		EventType:      eventType,
		ActorUserID:    &in.DecidedBy,
		OrganizationID: &org.ID,
		SubjectType:    "quota_raise_request",
		SubjectID:      request.ID,
		Payload:        payload,
	}); err != nil {
		return model.QuotaRaiseRequest{}, model.Organization{}, model.User{}, err
	}
	if in.EnqueueEmail {
		messageType := "quota_declined"
		var approvedQuotaValue *int
		if in.Approved {
			messageType = "quota_approved"
			value := approvedQuota
			approvedQuotaValue = &value
		}
		if err := s.enqueueEmailTx(ctx, tx, EmailOutboxInput{
			IdempotencyKey:  "quota-decision:" + request.ID + ":" + string(decision),
			MessageType:     messageType,
			TemplateVersion: 1,
			Payload: emaildelivery.Payload{
				RecipientEmail:   requester.Email,
				RecipientName:    requester.DisplayName,
				OrganizationID:   org.ID,
				OrganizationName: org.Name,
				RequestedQuota:   request.RequestedQuota,
				ApprovedQuota:    approvedQuotaValue,
				DecisionReason:   in.DecisionReason,
			},
		}); err != nil {
			return model.QuotaRaiseRequest{}, model.Organization{}, model.User{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return model.QuotaRaiseRequest{}, model.Organization{}, model.User{}, err
	}
	request.Status = decision
	request.DecidedBy = &in.DecidedBy
	request.DecisionReason = in.DecisionReason
	request.DecidedAt = &now
	org.EvaluationDeviceQuota = approvedQuota
	return request, org, requester, nil
}
