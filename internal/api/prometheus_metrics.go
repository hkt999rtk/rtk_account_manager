package api

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

func (s *Server) prometheusMetrics(c *gin.Context) {
	c.Header("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	var b strings.Builder
	writeMetricHelp(&b, "rtk_account_manager_up", "Whether the Account Manager app is serving metrics.")
	writeMetricType(&b, "rtk_account_manager_up", "gauge")
	writeMetric(&b, "rtk_account_manager_up", nil, 1)

	if s.store == nil {
		c.String(http.StatusOK, b.String())
		return
	}

	ctx := c.Request.Context()
	evalCreated, commercialCreated, err := s.store.CountEvaluationSignupEvents(ctx)
	if err != nil {
		c.String(http.StatusInternalServerError, b.String())
		return
	}
	verificationCompleted, err := s.store.CountEmailVerificationEventsFromSignup(ctx)
	if err != nil {
		c.String(http.StatusInternalServerError, b.String())
		return
	}
	pending, approved, declined, err := s.store.CountQuotaRaiseRequestStatuses(ctx)
	if err != nil {
		c.String(http.StatusInternalServerError, b.String())
		return
	}
	lifecycleMetrics, err := s.store.GetLifecycleMetrics(ctx)
	if err != nil {
		c.String(http.StatusInternalServerError, b.String())
		return
	}

	writeMetricHelp(&b, "rtk_account_manager_eval_signups_total", "Account Manager signup events by organization tier.")
	writeMetricType(&b, "rtk_account_manager_eval_signups_total", "counter")
	writeMetric(&b, "rtk_account_manager_eval_signups_total", map[string]string{"tier": "evaluation"}, evalCreated)
	writeMetric(&b, "rtk_account_manager_eval_signups_total", map[string]string{"tier": "commercial"}, commercialCreated)

	writeMetricHelp(&b, "rtk_account_manager_email_verification_completed_total", "Email verification completions for signup flows.")
	writeMetricType(&b, "rtk_account_manager_email_verification_completed_total", "counter")
	writeMetric(&b, "rtk_account_manager_email_verification_completed_total", nil, verificationCompleted)

	writeMetricHelp(&b, "rtk_account_manager_quota_raise_requests", "Quota raise requests by status.")
	writeMetricType(&b, "rtk_account_manager_quota_raise_requests", "gauge")
	writeMetric(&b, "rtk_account_manager_quota_raise_requests", map[string]string{"status": "pending"}, pending)
	writeMetric(&b, "rtk_account_manager_quota_raise_requests", map[string]string{"status": "approved"}, approved)
	writeMetric(&b, "rtk_account_manager_quota_raise_requests", map[string]string{"status": "declined"}, declined)

	if s.emailOutboxStore != nil {
		emailCounts, err := s.emailOutboxStore.GetEmailOutboxCounts(ctx, time.Now().UTC())
		if err != nil {
			c.String(http.StatusInternalServerError, b.String())
			return
		}
		writeMetricHelp(&b, "rtk_account_manager_email_outbox", "Email outbox messages by status.")
		writeMetricType(&b, "rtk_account_manager_email_outbox", "gauge")
		writeMetric(&b, "rtk_account_manager_email_outbox", map[string]string{"status": "pending"}, emailCounts.Pending)
		writeMetric(&b, "rtk_account_manager_email_outbox", map[string]string{"status": "retrying"}, emailCounts.Retrying)
		writeMetric(&b, "rtk_account_manager_email_outbox", map[string]string{"status": "sent"}, emailCounts.Sent)
		writeMetric(&b, "rtk_account_manager_email_outbox", map[string]string{"status": "dead_lettered"}, emailCounts.DeadLettered)
		writeMetric(&b, "rtk_account_manager_email_outbox", map[string]string{"status": "expired"}, emailCounts.Expired)
		writeMetricHelp(&b, "rtk_account_manager_email_outbox_oldest_pending_seconds", "Age of the oldest deliverable email.")
		writeMetricType(&b, "rtk_account_manager_email_outbox_oldest_pending_seconds", "gauge")
		writeMetric(&b, "rtk_account_manager_email_outbox_oldest_pending_seconds", nil, int64(emailCounts.OldestPendingAge.Seconds()))
		writeMetricHelp(&b, "rtk_account_manager_email_delivery_total", "Terminal email delivery outcomes.")
		writeMetricType(&b, "rtk_account_manager_email_delivery_total", "counter")
		writeMetric(&b, "rtk_account_manager_email_delivery_total", map[string]string{"outcome": "sent"}, emailCounts.Sent)
		writeMetric(&b, "rtk_account_manager_email_delivery_total", map[string]string{"outcome": "dead_lettered"}, emailCounts.DeadLettered)
		writeMetric(&b, "rtk_account_manager_email_delivery_total", map[string]string{"outcome": "expired"}, emailCounts.Expired)
		writeMetricHelp(&b, "rtk_account_manager_email_delivery_latency_seconds", "Average end-to-end latency for sent email.")
		writeMetricType(&b, "rtk_account_manager_email_delivery_latency_seconds", "gauge")
		writeMetric(&b, "rtk_account_manager_email_delivery_latency_seconds", nil, int64(emailCounts.DeliveryLatency.Seconds()))
	}

	writeMetricHelp(&b, "rtk_account_manager_lifecycle_messages", "Lifecycle message counts by queue and status.")
	writeMetricType(&b, "rtk_account_manager_lifecycle_messages", "gauge")
	writeStatusMetrics(&b, "rtk_account_manager_lifecycle_messages", "outbox", lifecycleMetrics.Outbox.ByStatus)
	writeStatusMetrics(&b, "rtk_account_manager_lifecycle_messages", "inbox", lifecycleMetrics.Inbox.ByStatus)

	writeMetricHelp(&b, "rtk_account_manager_lifecycle_operations", "Lifecycle operation counts by type and status.")
	writeMetricType(&b, "rtk_account_manager_lifecycle_operations", "gauge")
	for _, count := range lifecycleMetrics.Operations.ByTypeAndStatus {
		writeMetric(&b, "rtk_account_manager_lifecycle_operations", map[string]string{
			"type":   count.OperationType,
			"status": count.Status,
		}, count.Count)
	}

	c.String(http.StatusOK, b.String())
}

func writeStatusMetrics(b *strings.Builder, name, queue string, counts map[string]int64) {
	statuses := make([]string, 0, len(counts))
	for status := range counts {
		statuses = append(statuses, status)
	}
	sort.Strings(statuses)
	for _, status := range statuses {
		writeMetric(b, name, map[string]string{"queue": queue, "status": status}, counts[status])
	}
}

func writeMetricHelp(b *strings.Builder, name, help string) {
	_, _ = fmt.Fprintf(b, "# HELP %s %s\n", name, help)
}

func writeMetricType(b *strings.Builder, name, typ string) {
	_, _ = fmt.Fprintf(b, "# TYPE %s %s\n", name, typ)
}

func writeMetric(b *strings.Builder, name string, labels map[string]string, value int64) {
	_, _ = fmt.Fprint(b, name)
	if len(labels) > 0 {
		keys := make([]string, 0, len(labels))
		for key := range labels {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		_, _ = fmt.Fprint(b, "{")
		for i, key := range keys {
			if i > 0 {
				_, _ = fmt.Fprint(b, ",")
			}
			_, _ = fmt.Fprintf(b, `%s="%s"`, key, prometheusLabelValue(labels[key]))
		}
		_, _ = fmt.Fprint(b, "}")
	}
	_, _ = fmt.Fprintf(b, " %d\n", value)
}

func prometheusLabelValue(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, "\n", `\n`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return value
}
