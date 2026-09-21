package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"rtk_account_manager/internal/model"
)

// This is a local cross-repository HTTP test: Video Cloud's PostgreSQL outbox
// and worker send to the Account Manager router and separate PostgreSQL inbox.
func TestIntegrationCrossRepositoryDevicePresenceDelivery(t *testing.T) {
	videoDSN := os.Getenv("VIDEO_CLOUD_TEST_DSN")
	if videoDSN == "" {
		t.Skip("VIDEO_CLOUD_TEST_DSN for an isolated second database is not set")
	}
	videoRoot, err := filepath.Abs(filepath.Join("..", "..", "..", "rtk_video_cloud"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(videoRoot, "go.mod")); os.IsNotExist(err) {
		t.Skip("sibling Video Cloud checkout is not present")
	} else if err != nil {
		t.Fatal(err)
	}
	env := newIntegrationEnv(t)
	const internalToken = "presence-cross-repo-test-token"
	env.server.ConfigureInternalAuthToken(internalToken)
	owner := legacyCustomerForTest(t, env, "cross-repo-presence@example.com", "Cross Repo Presence")
	created := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices",
		devicePayload("cross-repo-presence", "CROSS-REPO-PRESENCE-001"), owner.Tokens.AccessToken)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	device := decodeBody[deviceBody](t, created).Device
	const videoID = "video-cross-repo-presence-1"
	provision := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.ID+"/provision", map[string]any{
		"video_cloud_devid": videoID, "activity_id": "activity-cross-repo-presence-1",
		"clip_public_key": "clip-cross-repo-presence-1", "operation_id": "cross-repo-provision-1",
	}, owner.Tokens.AccessToken)
	if provision.Code != http.StatusCreated {
		t.Fatalf("provision status=%d body=%s", provision.Code, provision.Body.String())
	}
	activated := performJSON(env.router, http.MethodPost, "/v1/internal/device-provisioning-results", map[string]any{
		"operation_id": "cross-repo-provision-1", "org_id": owner.Organization.ID,
		"account_device_id": device.ID, "video_cloud_devid": videoID,
		"activity_id": "activity-cross-repo-presence-1", "activated_at": time.Now().UTC().Truncate(time.Second),
	}, internalToken)
	if activated.Code != http.StatusOK {
		t.Fatalf("activation result status=%d body=%s", activated.Code, activated.Body.String())
	}
	receiver := httptest.NewServer(env.router)
	defer receiver.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "-race", "-run", "^TestWorkerAgainstExternalAccountManagerReceiver$", "-count=1", "./internal/presencebridge")
	cmd.Dir = videoRoot
	cmd.Env = append(os.Environ(),
		"GOWORK=off",
		"PRESENCE_BRIDGE_E2E_BASE_URL="+receiver.URL,
		"PRESENCE_BRIDGE_E2E_ORG_ID="+owner.Organization.ID,
		"PRESENCE_BRIDGE_E2E_ACCOUNT_DEVICE_ID="+device.ID,
		"PRESENCE_BRIDGE_E2E_VIDEO_DEVICE_ID="+videoID,
		"PRESENCE_BRIDGE_E2E_TOKEN="+internalToken,
		"PRESENCE_BRIDGE_E2E_VIDEO_DSN="+videoDSN,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Video Cloud worker cross-repository test: %v\n%s", err, output)
	}
	stored, err := env.store.GetDevice(context.Background(), owner.Organization.ID, device.ID)
	if err != nil || stored.Status != model.DeviceStatusOffline || stored.LastSeenAt == nil {
		t.Fatalf("cross-service final projection: %+v err=%v", stored, err)
	}
	var inboxCount int
	if err := env.db.QueryRow(context.Background(), `SELECT count(*) FROM device_message_inbox WHERE message_type='DeviceOnlineChanged'`).Scan(&inboxCount); err != nil || inboxCount != 2 {
		t.Fatalf("cross-service inbox count=%d err=%v", inboxCount, err)
	}
}
