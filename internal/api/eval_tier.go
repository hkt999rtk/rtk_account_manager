package api

import (
	"errors"
	"net/http"
	"net/mail"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

type signupRequest struct {
	Email string `json:"email"`
}

type signupResponse struct {
	User         model.User         `json:"user"`
	BrandCloud   model.Organization `json:"brand_cloud"`
	Organization model.Organization `json:"organization"`
}

type quotaRaiseRequestRequest struct {
	RequestedQuota int            `json:"requested_quota" binding:"required"`
	UseCase        string         `json:"use_case" binding:"required"`
	ContactInfo    map[string]any `json:"contact_info" binding:"required"`
}

type quotaRaiseRequestResponse struct {
	QuotaRaiseRequest model.QuotaRaiseRequest `json:"quota_raise_request"`
}

type quotaRaiseRequestsResponse struct {
	QuotaRaiseRequests []model.QuotaRaiseRequest `json:"quota_raise_requests"`
	Pagination         store.Page                `json:"pagination"`
}

type auditEventsResponse struct {
	AuditEvents []model.AuditEvent `json:"audit_events"`
	Pagination  store.Page         `json:"pagination"`
}

type quotaRaiseDecisionRequest struct {
	ApprovedQuota  *int    `json:"approved_quota"`
	DecisionReason *string `json:"decision_reason"`
}

type quotaRaiseDecisionResponse struct {
	QuotaRaiseRequest model.QuotaRaiseRequest `json:"quota_raise_request"`
	Organization      model.Organization      `json:"organization"`
}

type signupPolicy struct {
	disposableDomains map[string]struct{}
}

func loadSignupPolicy() signupPolicy {
	policy := signupPolicy{
		disposableDomains: map[string]struct{}{
			"mailinator.com":    {},
			"10minutemail.com":  {},
			"guerrillamail.com": {},
			"tempmail.com":      {},
			"yopmail.com":       {},
		},
	}
	if env := strings.TrimSpace(os.Getenv("SIGNUP_DISPOSABLE_DOMAINS")); env != "" {
		policy.disposableDomains = map[string]struct{}{}
		for _, domain := range strings.Split(env, ",") {
			domain = strings.ToLower(strings.TrimSpace(domain))
			if domain != "" {
				policy.disposableDomains[domain] = struct{}{}
			}
		}
	}
	return policy
}

type signupLimiter struct {
	mu       sync.Mutex
	limit    int
	window   time.Duration
	counters map[string]signupLimitState
}

type signupLimitState struct {
	windowStart time.Time
	count       int
}

func newSignupLimiter(limit int, window time.Duration) *signupLimiter {
	return &signupLimiter{
		limit:    limit,
		window:   window,
		counters: map[string]signupLimitState{},
	}
}

func (l *signupLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Lazy eviction of stale windows so the map cannot grow unboundedly when
	// callers vary the key (e.g. spoofed X-Forwarded-For). The sweep is
	// amortized: only run when the map is non-trivially sized.
	if len(l.counters) > 256 {
		for k, state := range l.counters {
			if state.windowStart.IsZero() || now.Sub(state.windowStart) >= l.window {
				delete(l.counters, k)
			}
		}
	}

	state := l.counters[key]
	if state.windowStart.IsZero() || now.Sub(state.windowStart) >= l.window {
		state.windowStart = now
		state.count = 0
	}
	state.count++
	l.counters[key] = state
	return state.count <= l.limit
}

func (s *Server) signup(c *gin.Context) {
	var req signupRequest
	if !bindStrict(c, &req) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	parsed, err := mail.ParseAddress(email)
	if err != nil || parsed.Address != email {
		writeError(c, http.StatusBadRequest, "invalid_request", "email must be a valid email address")
		return
	}
	if !s.allowSignup(c, email) {
		return
	}
	pendingSecret, err := auth.RandomToken()
	if err != nil {
		writeError(c, http.StatusInternalServerError, "password_hash_failed", "Could not initialize pending account")
		return
	}
	hash, err := auth.HashPassword(pendingSecret)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "password_hash_failed", "Could not initialize pending account")
		return
	}
	result, err := s.store.SignupDeveloper(c.Request.Context(), store.DeveloperSignupInput{
		Email:                     email,
		PasswordHash:              hash,
		SignupPendingVerification: true,
	})
	if errors.Is(err, store.ErrConflict) {
		result, err = s.store.ResumeExpiredDeveloperSignup(c.Request.Context(), email)
	}
	if err != nil {
		writeStoreError(c, err)
		return
	}
	if _, _, err := s.issueAuthToken(c, result.User.ID, result.User.Email, "email_verification"); err != nil {
		writeError(c, http.StatusInternalServerError, "token_delivery_failed", "Could not deliver verification token")
		return
	}
	c.JSON(http.StatusAccepted, signupResponse{User: result.User, BrandCloud: result.BrandCloud, Organization: result.BrandCloud})
}

func (s *Server) createQuotaRaiseRequest(c *gin.Context) {
	var req quotaRaiseRequestRequest
	if !bind(c, &req) {
		return
	}
	if req.RequestedQuota <= 0 {
		writeError(c, http.StatusBadRequest, "invalid_request", "requested_quota must be positive")
		return
	}
	if req.RequestedQuota > 200 {
		writeError(c, http.StatusBadRequest, "invalid_request", "requested_quota must not exceed 200")
		return
	}
	if !requireNonBlank(c, "use_case", req.UseCase) {
		return
	}
	if len(req.ContactInfo) == 0 {
		writeError(c, http.StatusBadRequest, "invalid_request", "contact_info must not be empty")
		return
	}
	request, err := s.store.CreateQuotaRaiseRequest(c.Request.Context(), store.QuotaRaiseRequestInput{
		OrganizationID: c.Param("orgId"),
		RequestedBy:    currentUserID(c),
		RequestedQuota: req.RequestedQuota,
		UseCase:        strings.TrimSpace(req.UseCase),
		ContactInfo:    req.ContactInfo,
	})
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusCreated, quotaRaiseRequestResponse{QuotaRaiseRequest: request})
}

func (s *Server) approveQuotaRaiseRequest(c *gin.Context) {
	s.decideQuotaRaiseRequest(c, true)
}

func (s *Server) declineQuotaRaiseRequest(c *gin.Context) {
	s.decideQuotaRaiseRequest(c, false)
}

func (s *Server) listAdminQuotaRaiseRequests(c *gin.Context) {
	limit, offset := pagination(c)
	status, ok := parseQuotaRaiseStatusFilter(c)
	if !ok {
		return
	}
	page, err := s.store.ListQuotaRaiseRequests(c.Request.Context(), store.QuotaRaiseRequestListFilter{
		Status: status,
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, quotaRaiseRequestsResponse{QuotaRaiseRequests: page.Requests, Pagination: page.Page})
}

func (s *Server) getAdminQuotaRaiseRequest(c *gin.Context) {
	request, err := s.store.GetQuotaRaiseRequest(c.Request.Context(), c.Param("requestId"))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, quotaRaiseRequestResponse{QuotaRaiseRequest: request})
}

func (s *Server) listAdminAuditEvents(c *gin.Context) {
	limit, offset := pagination(c)
	page, err := s.store.ListAuditEvents(c.Request.Context(), store.AuditEventListFilter{
		EventType:   strings.TrimSpace(c.Query("event_type")),
		SubjectType: strings.TrimSpace(c.Query("subject_type")),
		Limit:       limit,
		Offset:      offset,
	})
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, auditEventsResponse{AuditEvents: page.Events, Pagination: page.Page})
}

func parseQuotaRaiseStatusFilter(c *gin.Context) (model.QuotaRaiseRequestStatus, bool) {
	status := model.QuotaRaiseRequestStatus(strings.TrimSpace(c.Query("status")))
	switch status {
	case "":
		return "", true
	case model.QuotaRaiseRequestStatusPending, model.QuotaRaiseRequestStatusApproved, model.QuotaRaiseRequestStatusDeclined:
		return status, true
	default:
		writeError(c, http.StatusBadRequest, "invalid_request", "status must be pending, approved, or declined")
		return "", false
	}
}

func (s *Server) decideQuotaRaiseRequest(c *gin.Context, approved bool) {
	var req quotaRaiseDecisionRequest
	if !bindOptional(c, &req) {
		return
	}
	requestID := c.Param("requestId")
	decision := store.QuotaRaiseDecisionInput{
		RequestID:      requestID,
		DecidedBy:      currentUserID(c),
		DecisionReason: req.DecisionReason,
		Approved:       approved,
		EnqueueEmail:   s.emailOutboxStore != nil,
	}
	if approved {
		if req.ApprovedQuota != nil {
			decision.ApprovedQuota = *req.ApprovedQuota
		}
	}
	request, org, requester, err := s.store.DecideQuotaRaiseRequest(c.Request.Context(), decision)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	if s.emailOutboxStore == nil && s.quotaRaiseNotificationSink != nil {
		if err := s.quotaRaiseNotificationSink.DeliverQuotaRaiseNotification(c.Request.Context(), QuotaRaiseNotificationDelivery{
			RecipientEmail:   requester.Email,
			RecipientName:    requester.DisplayName,
			OrganizationID:   org.ID,
			OrganizationName: org.Name,
			RequestedQuota:   request.RequestedQuota,
			ApprovedQuota:    approvedQuotaForNotification(org, approved),
			DecisionReason:   request.DecisionReason,
			Decision:         string(request.Status),
		}); err != nil {
			writeError(c, http.StatusInternalServerError, "notification_delivery_failed", "Could not deliver quota-raise decision notification")
			return
		}
	}
	c.JSON(http.StatusOK, quotaRaiseDecisionResponse{QuotaRaiseRequest: request, Organization: org})
}

func approvedQuotaForNotification(org model.Organization, approved bool) *int {
	if !approved {
		return nil
	}
	quota := org.EvaluationDeviceQuota
	return &quota
}

func (s *Server) requirePlatformAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		isAdmin, err := s.store.IsPlatformAdmin(c.Request.Context(), currentUserID(c))
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeError(c, http.StatusForbidden, "forbidden", "Insufficient permissions")
			} else {
				writeError(c, http.StatusInternalServerError, "internal_error", "Internal server error")
			}
			c.Abort()
			return
		}
		if !isAdmin {
			writeError(c, http.StatusForbidden, "forbidden", "Insufficient permissions")
			c.Abort()
			return
		}
		c.Next()
	}
}

func (s *Server) allowSignup(c *gin.Context, email string) bool {
	if s.signupLimiter != nil {
		ip := c.ClientIP()
		if ip == "" {
			ip = c.Request.RemoteAddr
		}
		if !s.signupLimiter.allow(ip, time.Now().UTC()) {
			writeError(c, http.StatusTooManyRequests, "rate_limited", "Too many signup attempts")
			return false
		}
	}
	if isDisposableSignupEmail(email, s.signupPolicy.disposableDomains) {
		writeError(c, http.StatusBadRequest, "disposable_email", "Disposable email addresses are not allowed")
		return false
	}
	return true
}

func isDisposableSignupEmail(email string, disposableDomains map[string]struct{}) bool {
	parsed, err := mail.ParseAddress(email)
	if err == nil {
		email = parsed.Address
	}
	parts := strings.Split(strings.ToLower(strings.TrimSpace(email)), "@")
	if len(parts) != 2 {
		return false
	}
	_, ok := disposableDomains[parts[1]]
	return ok
}
