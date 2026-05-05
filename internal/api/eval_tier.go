package api

import (
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
	Email            string  `json:"email" binding:"required,email"`
	Password         string  `json:"password" binding:"required,min=8"`
	DisplayName      *string `json:"display_name"`
	OrganizationName string  `json:"organization_name" binding:"required"`
	CaptchaToken     *string `json:"captcha_token"`
}

type signupResponse struct {
	User         model.User         `json:"user"`
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

type quotaRaiseDecisionRequest struct {
	ApprovedQuota  *int    `json:"approved_quota"`
	DecisionReason *string `json:"decision_reason"`
}

type quotaRaiseDecisionResponse struct {
	QuotaRaiseRequest model.QuotaRaiseRequest `json:"quota_raise_request"`
	Organization      model.Organization      `json:"organization"`
}

type signupPolicy struct {
	captchaRequired   bool
	disposableDomains map[string]struct{}
}

func loadSignupPolicy() signupPolicy {
	policy := signupPolicy{
		captchaRequired: parseBoolEnv("SIGNUP_CAPTCHA_REQUIRED"),
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

func parseBoolEnv(key string) bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	return value == "1" || value == "true" || value == "yes" || value == "on"
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
	if !bind(c, &req) {
		return
	}
	if !requireNonBlank(c, "organization_name", req.OrganizationName) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if !s.allowSignup(c, email, req.CaptchaToken) {
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "password_hash_failed", "Could not hash password")
		return
	}
	result, err := s.store.Register(c.Request.Context(), store.RegisterInput{
		Email:                     email,
		PasswordHash:              hash,
		DisplayName:               req.DisplayName,
		OrganizationName:          strings.TrimSpace(req.OrganizationName),
		OrganizationTier:          model.OrganizationTierEvaluation,
		EvaluationDeviceQuota:     5,
		SignupPendingVerification: true,
	})
	if err != nil {
		writeStoreError(c, err)
		return
	}
	token, expiresAt, err := s.createAuthToken(c, result.User.ID, "email_verification")
	if err != nil {
		writeAuthTokenStoreError(c, err, "Could not issue verification token")
		return
	}
	if err := s.deliverAuthToken(c, result.User.Email, "email_verification", token, expiresAt); err != nil {
		writeError(c, http.StatusInternalServerError, "token_delivery_failed", "Could not deliver verification token")
		return
	}
	c.JSON(http.StatusAccepted, signupResponse{User: result.User, Organization: result.Organization})
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
	if s.quotaRaiseNotificationSink != nil {
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
			writeError(c, http.StatusForbidden, "forbidden", "Insufficient permissions")
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

func (s *Server) allowSignup(c *gin.Context, email string, captchaToken *string) bool {
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
	if s.signupPolicy.captchaRequired && (captchaToken == nil || strings.TrimSpace(*captchaToken) == "") {
		writeError(c, http.StatusBadRequest, "captcha_required", "captcha_token must be provided")
		return false
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
