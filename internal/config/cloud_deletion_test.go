package config

import (
	"strings"
	"testing"
	"time"
)

func TestCloudDeletionWorkerConfiguration(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("DATABASE_URL", "postgres://isolated")
	t.Setenv("BILLING_HANDOFF_BASE_URL", "")
	t.Setenv("BILLING_HANDOFF_TOKEN", "")
	if _, err := LoadCloudDeletionWorker(); err == nil {
		t.Fatal("missing transport accepted")
	}
	t.Setenv("BILLING_HANDOFF_BASE_URL", "https://billing.example")
	t.Setenv("BILLING_HANDOFF_TOKEN", strings.Repeat("h", 32))
	if _, err := LoadCloudDeletionWorker(); err == nil {
		t.Fatal("missing resource producer accepted")
	}
	t.Setenv("VIDEO_CONTROL_PLANE_HANDOFF_BASE_URL", "https://video.example")
	t.Setenv("VIDEO_CONTROL_PLANE_HANDOFF_TOKEN", strings.Repeat("v", 32))
	if _, err := LoadCloudDeletionWorker(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ key, value string }{
		{"CLOUD_DELETION_WORKER_POLL_INTERVAL", "invalid"}, {"CLOUD_DELETION_WORKER_BATCH_SIZE", "NaN"},
		{"CLOUD_DELETION_WORKER_POLL_INTERVAL", "0s"}, {"CLOUD_DELETION_WORKER_LEASE_DURATION", "30s"},
		{"CLOUD_DELETION_WORKER_STEP_TIMEOUT", "2m"}, {"CLOUD_DELETION_WORKER_BATCH_SIZE", "129"},
		{"CLOUD_DELETION_WORKER_BATCH_SIZE", "0"}, {"CLOUD_DELETION_WORKER_BATCH_SIZE", "-1"},
		{"DATABASE_URL", ""}, {"CLOUD_DELETION_WORKER_POLL_INTERVAL", " 0s "},
		{"CLOUD_DELETION_WORKER_STEP_TIMEOUT", "2562047h47m16.854775807s"},
		{"BILLING_HANDOFF_BASE_URL", "http://billing.example"}, {"ACCOUNT_MANAGER_INTERNAL_AUTH_TOKEN", strings.Repeat("h", 32)},
	} {
		t.Run(tc.key+tc.value, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			if _, err := LoadCloudDeletionWorker(); err == nil {
				t.Fatal("unsafe configuration")
			}
		})
	}
	t.Setenv("CLOUD_DELETION_WORKER_POLL_INTERVAL", " 1s ")
	t.Setenv("CLOUD_DELETION_WORKER_BATCH_SIZE", " 2 ")
	cfg, err := LoadCloudDeletionWorker()
	if err != nil || cfg.CloudDeletionPollInterval != time.Second || cfg.CloudDeletionBatchSize != 2 {
		t.Fatalf("explicit settings replaced by defaults: %+v %v", cfg.CloudDeletionPollInterval, err)
	}
}
