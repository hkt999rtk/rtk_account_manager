package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"rtk_account_manager/internal/channel"
	"rtk_account_manager/internal/model"
)

func TestIntegrationInternalDevicePresenceEvent(t *testing.T) {
	env := newIntegrationEnv(t)
	env.server.ConfigureInternalAuthToken("presence-internal-token")
	owner := legacyCustomerForTest(t, env, "presence-event-owner@example.com", "Presence Event Org")
	created := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices",
		devicePayload("presence-event-device", "PRESENCE-EVENT-001"), owner.Tokens.AccessToken)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	device := decodeBody[deviceBody](t, created).Device
	provision := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.ID+"/provision", map[string]any{
		"video_cloud_devid": "video-presence-event-1", "activity_id": "activity-presence-event-1",
		"clip_public_key": "clip-presence-event-1", "operation_id": "presence-event-provision-1",
	}, owner.Tokens.AccessToken)
	if provision.Code != http.StatusCreated {
		t.Fatalf("provision status=%d body=%s", provision.Code, provision.Body.String())
	}
	activatedAt := time.Now().UTC().Truncate(time.Second)
	result := performJSON(env.router, http.MethodPost, "/v1/internal/device-provisioning-results", map[string]any{
		"operation_id": "presence-event-provision-1", "org_id": owner.Organization.ID,
		"account_device_id": device.ID, "video_cloud_devid": "video-presence-event-1",
		"activity_id": "activity-presence-event-1", "activated_at": activatedAt,
	}, "presence-internal-token")
	if result.Code != http.StatusOK {
		t.Fatalf("activation result status=%d body=%s", result.Code, result.Body.String())
	}

	observedAt := activatedAt.Add(time.Second)
	post := func(messageID, videoID string, status channel.OnlineStatus, seenAt time.Time, token string) int {
		t.Helper()
		payload, err := json.Marshal(channel.DeviceOnlineChangedPayload{
			OrgID: owner.Organization.ID, AccountDeviceID: device.ID, VideoCloudDevid: videoID,
			Status: status, LastSeenAt: seenAt,
		})
		if err != nil {
			t.Fatal(err)
		}
		response := performJSON(env.router, http.MethodPost, "/v1/internal/device-presence-events", channel.Envelope{
			MessageID: messageID, CorrelationID: "presence-event-provision-1", OperationID: "presence-event-provision-1",
			SourceService: channel.ServiceRealtekVideoCloud, TargetService: channel.ServiceAccountManager,
			MessageType: channel.MessageTypeDeviceOnlineChanged, SchemaVersion: channel.SchemaVersionV1,
			PartitionKey: device.ID, OccurredAt: seenAt, Payload: payload,
		}, token)
		if response.Code == http.StatusOK {
			var receipt struct {
				Status    string `json:"status"`
				MessageID string `json:"message_id"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil || receipt.Status != "ok" || receipt.MessageID != messageID {
				t.Fatalf("unverified presence receipt: %s (%v)", response.Body.String(), err)
			}
		}
		return response.Code
	}
	if got := post("presence-event-online", "video-presence-event-1", channel.OnlineStatusOnline, observedAt, "wrong-token"); got != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d", got)
	}
	if got := post("presence-event-online", "video-presence-event-1", channel.OnlineStatusOnline, observedAt, "presence-internal-token"); got != http.StatusOK {
		t.Fatalf("online status=%d", got)
	}
	if got := post("presence-event-online", "video-presence-event-1", channel.OnlineStatusOnline, observedAt, "presence-internal-token"); got != http.StatusOK {
		t.Fatalf("idempotent replay status=%d", got)
	}
	if got := post("presence-event-old-offline", "video-presence-event-1", channel.OnlineStatusOffline, observedAt.Add(-time.Second), "presence-internal-token"); got != http.StatusOK {
		t.Fatalf("older offline status=%d", got)
	}
	stored, err := env.store.GetDevice(context.Background(), owner.Organization.ID, device.ID)
	if err != nil || stored.Status != model.DeviceStatusOnline {
		t.Fatalf("older observation regressed status: %+v err=%v", stored, err)
	}
	if got := post("presence-event-equal-offline", "video-presence-event-1", channel.OnlineStatusOffline, observedAt, "presence-internal-token"); got != http.StatusOK {
		t.Fatalf("equal-time offline status=%d", got)
	}
	if got := post("presence-event-wrong-video", "different-video-device", channel.OnlineStatusOnline, observedAt.Add(time.Second), "presence-internal-token"); got != http.StatusConflict {
		t.Fatalf("identity mismatch status=%d", got)
	}
	if got := post("presence-event-online", "video-presence-event-1", channel.OnlineStatusOffline, observedAt, "presence-internal-token"); got != http.StatusConflict {
		t.Fatalf("message-id payload conflict status=%d", got)
	}
	stored, err = env.store.GetDevice(context.Background(), owner.Organization.ID, device.ID)
	if err != nil || stored.Status != model.DeviceStatusOffline || stored.LastSeenAt == nil || !stored.LastSeenAt.Equal(observedAt) {
		t.Fatalf("final projected presence: %+v err=%v", stored, err)
	}
}
