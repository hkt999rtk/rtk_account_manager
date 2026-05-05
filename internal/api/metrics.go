package api

import (
	"math"
	"net/http"

	"github.com/gin-gonic/gin"
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
}

type evaluationQuotaUsageResponse struct {
	OrganizationID        string  `json:"organization_id"`
	OrganizationName      string  `json:"organization_name"`
	ActiveDevices         int     `json:"active_devices"`
	EvaluationDeviceQuota int     `json:"evaluation_device_quota"`
	Utilization           float64 `json:"utilization"`
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

	c.JSON(http.StatusOK, body)
}
