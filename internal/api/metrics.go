package api

import (
	"math"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/store"
)

type evalTierMetricsResponse struct {
	Signups struct {
		EvaluationCreated          int64   `json:"evaluation_created"`
		CommercialCreated          int64   `json:"commercial_created"`
		VerificationCompleted      int64   `json:"verification_completed"`
		VerificationCompletionRate float64 `json:"verification_completion_rate"`
	} `json:"signups"`
	QuotaRaiseRequests struct {
		Pending  int64 `json:"pending"`
		Approved int64 `json:"approved"`
		Declined int64 `json:"declined"`
	} `json:"quota_raise_requests"`
	EvaluationQuotaUsage []evaluationQuotaUsageResponse `json:"evaluation_quota_usage"`
	Lifecycle            lifecycleMetricsResponse       `json:"lifecycle"`
}

type evaluationQuotaUsageResponse struct {
	OrganizationID        string  `json:"organization_id"`
	OrganizationName      string  `json:"organization_name"`
	ActiveDevices         int     `json:"active_devices"`
	EvaluationDeviceQuota int     `json:"evaluation_device_quota"`
	Utilization           float64 `json:"utilization"`
}

type lifecycleMetricsResponse struct {
	Outbox     lifecycleMessageMetricsResponse   `json:"outbox"`
	Inbox      lifecycleMessageMetricsResponse   `json:"inbox"`
	Operations lifecycleOperationMetricsResponse `json:"operations"`
}

type lifecycleMessageMetricsResponse struct {
	ByStatus            map[string]int64                     `json:"by_status"`
	DeadLetteredByError []lifecycleMessageErrorCountResponse `json:"dead_lettered_by_error"`
	LastCompletedAt     *time.Time                           `json:"last_completed_at,omitempty"`
}

type lifecycleMessageErrorCountResponse struct {
	MessageType string `json:"message_type"`
	ErrorCode   string `json:"error_code"`
	Count       int64  `json:"count"`
}

type lifecycleOperationMetricsResponse struct {
	ByStatus                map[string]int64                        `json:"by_status"`
	ByTypeAndStatus         []lifecycleOperationStatusCountResponse `json:"by_type_and_status"`
	OldestActiveAgeSeconds  int64                                   `json:"oldest_active_age_seconds"`
	LastTerminalCompletedAt *time.Time                              `json:"last_terminal_completed_at,omitempty"`
}

type lifecycleOperationStatusCountResponse struct {
	OperationType string `json:"operation_type"`
	Status        string `json:"status"`
	Count         int64  `json:"count"`
}

func (s *Server) adminMetrics(c *gin.Context) {
	evalCreated, commercialCreated, err := s.store.CountEvaluationSignupEvents(c.Request.Context())
	if err != nil {
		writeStoreError(c, err)
		return
	}
	verificationCompleted, err := s.store.CountEmailVerificationEventsFromSignup(c.Request.Context())
	if err != nil {
		writeStoreError(c, err)
		return
	}
	pending, approved, declined, err := s.store.CountQuotaRaiseRequestStatuses(c.Request.Context())
	if err != nil {
		writeStoreError(c, err)
		return
	}
	usages, err := s.store.ListEvaluationQuotaUsage(c.Request.Context())
	if err != nil {
		writeStoreError(c, err)
		return
	}
	lifecycleMetrics, err := s.store.GetLifecycleMetrics(c.Request.Context())
	if err != nil {
		writeStoreError(c, err)
		return
	}

	body := evalTierMetricsResponse{}
	body.Signups.EvaluationCreated = evalCreated
	body.Signups.CommercialCreated = commercialCreated
	body.Signups.VerificationCompleted = verificationCompleted
	if evalCreated > 0 {
		body.Signups.VerificationCompletionRate = math.Round((float64(verificationCompleted)/float64(evalCreated))*10000) / 10000
	}
	body.QuotaRaiseRequests.Pending = pending
	body.QuotaRaiseRequests.Approved = approved
	body.QuotaRaiseRequests.Declined = declined
	body.EvaluationQuotaUsage = make([]evaluationQuotaUsageResponse, 0, len(usages))
	for _, usage := range usages {
		body.EvaluationQuotaUsage = append(body.EvaluationQuotaUsage, evaluationQuotaUsageResponse{
			OrganizationID:        usage.OrganizationID,
			OrganizationName:      usage.OrganizationName,
			ActiveDevices:         usage.ActiveDevices,
			EvaluationDeviceQuota: usage.EvaluationDeviceQuota,
			Utilization:           usage.Utilization(),
		})
	}
	body.Lifecycle = lifecycleMetricsToResponse(lifecycleMetrics)

	c.JSON(http.StatusOK, body)
}

func lifecycleMetricsToResponse(metrics store.LifecycleMetrics) lifecycleMetricsResponse {
	return lifecycleMetricsResponse{
		Outbox: lifecycleMessageMetricsToResponse(metrics.Outbox),
		Inbox:  lifecycleMessageMetricsToResponse(metrics.Inbox),
		Operations: lifecycleOperationMetricsResponse{
			ByStatus:                metrics.Operations.ByStatus,
			ByTypeAndStatus:         lifecycleOperationStatusCountsToResponse(metrics.Operations.ByTypeAndStatus),
			OldestActiveAgeSeconds:  metrics.Operations.OldestActiveAgeSeconds,
			LastTerminalCompletedAt: metrics.Operations.LastTerminalCompletedAt,
		},
	}
}

func lifecycleMessageMetricsToResponse(metrics store.LifecycleMessageMetrics) lifecycleMessageMetricsResponse {
	return lifecycleMessageMetricsResponse{
		ByStatus:            metrics.ByStatus,
		DeadLetteredByError: lifecycleMessageErrorCountsToResponse(metrics.DeadLetteredByError),
		LastCompletedAt:     metrics.LastCompletedAt,
	}
}

func lifecycleMessageErrorCountsToResponse(counts []store.LifecycleMessageErrorCount) []lifecycleMessageErrorCountResponse {
	out := make([]lifecycleMessageErrorCountResponse, 0, len(counts))
	for _, count := range counts {
		out = append(out, lifecycleMessageErrorCountResponse{
			MessageType: count.MessageType,
			ErrorCode:   count.ErrorCode,
			Count:       count.Count,
		})
	}
	return out
}

func lifecycleOperationStatusCountsToResponse(counts []store.LifecycleOperationStatusCount) []lifecycleOperationStatusCountResponse {
	out := make([]lifecycleOperationStatusCountResponse, 0, len(counts))
	for _, count := range counts {
		out = append(out, lifecycleOperationStatusCountResponse{
			OperationType: count.OperationType,
			Status:        count.Status,
			Count:         count.Count,
		})
	}
	return out
}
