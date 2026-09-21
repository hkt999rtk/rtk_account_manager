package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"rtk_account_manager/internal/lifecyclehttp"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/worker/outbox"
)

// The sibling Video Cloud test supplies its real HTTP router and a separate
// PostgreSQL database. The proxy fails before delivery, sends a mismatched
// HTTP 200 receipt, then loses a response after the remote activation committed.
func TestIntegrationAgainstExternalVideoActivationReplay(t *testing.T) {
	baseURL := os.Getenv("ACTIVATION_REPLAY_E2E_BASE_URL")
	if baseURL == "" {
		t.Skip("ACTIVATION_REPLAY_E2E_BASE_URL is not set")
	}
	token := os.Getenv("ACTIVATION_REPLAY_E2E_TOKEN")
	videoID := os.Getenv("ACTIVATION_REPLAY_E2E_DEVICE_ID")
	if token == "" || videoID == "" {
		t.Fatal("external Video Cloud activation inputs are incomplete")
	}
	env := newIntegrationEnv(t)
	ctx := context.Background()
	owner := legacyCustomerForTest(t, env, "cross-activation@example.com", "Cross Activation")
	created := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices",
		devicePayload("cross-activation", "CROSS-ACTIVATION-001"), owner.Tokens.AccessToken)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	device := decodeBody[deviceBody](t, created).Device
	const operationID = "cross-repo-activation-operation-1"
	const activityID = "activity-cross-repo-activation-1"
	provision := performJSON(env.router, http.MethodPost,
		"/v1/orgs/"+owner.Organization.ID+"/devices/"+device.ID+"/provision", map[string]any{
			"video_cloud_devid": videoID, "activity_id": activityID,
			"clip_public_key": "clip-cross-repo-activation-1", "operation_id": operationID,
		}, owner.Tokens.AccessToken)
	if provision.Code != http.StatusCreated {
		t.Fatalf("provision status=%d body=%s", provision.Code, provision.Body.String())
	}
	clock := time.Now().UTC().Add(time.Minute)
	now := func() time.Time { return clock }
	publisher, err := lifecyclehttp.NewPublisher(lifecyclehttp.Options{
		BaseURL: baseURL, Token: token, Store: env.store, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	worker := outbox.NewService(env.store, publisher, outbox.Options{
		Now: now, RetryDelay: time.Second, LeaseDuration: 10 * time.Second,
		MaxAttempts: 5, BatchSize: 1,
	})
	for attempt := 1; attempt <= 4; attempt++ {
		stats, err := worker.RunOnce(ctx)
		if err != nil {
			t.Fatalf("outbox attempt %d: %v", attempt, err)
		}
		var status string
		var count int
		if err := env.db.QueryRow(ctx, `SELECT status, attempt_count FROM device_message_outbox WHERE operation_id=$1`, operationID).Scan(&status, &count); err != nil {
			t.Fatal(err)
		}
		if attempt < 4 {
			if stats.Retrying != 1 || stats.Published != 0 || status != "retrying" || count != attempt {
				t.Fatalf("attempt %d did not remain retryable: stats=%+v status=%s count=%d", attempt, stats, status, count)
			}
			var inboxCount int
			if err := env.db.QueryRow(ctx, `SELECT count(*) FROM device_message_inbox WHERE operation_id=$1`, operationID).Scan(&inboxCount); err != nil || inboxCount != 0 {
				t.Fatalf("attempt %d projected before acknowledgement: inbox=%d err=%v", attempt, inboxCount, err)
			}
		} else if stats.Published != 1 || status != "published" || count != 4 {
			t.Fatalf("replayed activation was not published: stats=%+v status=%s count=%d", stats, status, count)
		}
		clock = clock.Add(2 * time.Second)
	}
	stored, err := env.store.GetDevice(ctx, owner.Organization.ID, device.ID)
	if err != nil || stored.Metadata[model.DeviceMetadataVideoCloudActivationStatus] != string(model.VideoCloudActivationStatusActivated) ||
		stored.Status == model.DeviceStatusOnline {
		t.Fatalf("account activation projection=%+v err=%v", stored, err)
	}
	var operationStatus string
	var inboxCount int
	if err := env.db.QueryRow(ctx, `SELECT status FROM device_operations WHERE operation_id=$1`, operationID).Scan(&operationStatus); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM device_message_inbox WHERE operation_id=$1`, operationID).Scan(&inboxCount); err != nil {
		t.Fatal(err)
	}
	if operationStatus != "succeeded" || inboxCount != 1 {
		t.Fatalf("activation result status=%s inbox=%d, want succeeded/1", operationStatus, inboxCount)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		baseURL+"/v1/internal/account-manager/devices/"+videoID+"/presence", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var presence struct {
		DeviceID        string `json:"devid"`
		OrgID           string `json:"org_id"`
		AccountDeviceID string `json:"account_device_id"`
		Activated       bool   `json:"activated"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&presence) != nil ||
		presence.DeviceID != videoID || presence.OrgID != owner.Organization.ID ||
		presence.AccountDeviceID != device.ID || !presence.Activated {
		t.Fatalf("remote activation presence status=%d value=%+v", response.StatusCode, presence)
	}
}
