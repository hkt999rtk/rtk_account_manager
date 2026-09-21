package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

// The Video Cloud integration test supplies its actual HTTP router and a
// separate PostgreSQL database for the remote transfer-fence lifecycle.
func TestIntegrationAgainstExternalVideoTransferFence(t *testing.T) {
	baseURL := os.Getenv("TRANSFER_FENCE_E2E_BASE_URL")
	if baseURL == "" {
		t.Skip("TRANSFER_FENCE_E2E_BASE_URL is not set")
	}
	token := os.Getenv("TRANSFER_FENCE_E2E_TOKEN")
	successVideoID := os.Getenv("TRANSFER_FENCE_E2E_SUCCESS_DEVICE_ID")
	lostVideoID := os.Getenv("TRANSFER_FENCE_E2E_LOST_DEVICE_ID")
	rollbackVideoID := os.Getenv("TRANSFER_FENCE_E2E_ROLLBACK_DEVICE_ID")
	if token == "" || successVideoID == "" || lostVideoID == "" || rollbackVideoID == "" ||
		successVideoID == lostVideoID || successVideoID == rollbackVideoID || lostVideoID == rollbackVideoID {
		t.Fatal("external Video Cloud transfer-fence inputs are incomplete")
	}
	env := newIntegrationEnv(t)
	ctx := context.Background()
	admin := legacyCustomerForTest(t, env, "cross-transfer-admin@example.com", "Cross Transfer Admin")
	source := legacyCustomerForTest(t, env, "cross-transfer-source@example.com", "Cross Transfer Source")
	target := legacyCustomerForTest(t, env, "cross-transfer-target@example.com", "Cross Transfer Target")
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin=true WHERE id=$1`, admin.User.ID); err != nil {
		t.Fatal(err)
	}
	if err := env.server.ConfigureVideoPresence(baseURL, token, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	createClaim := func(videoID string) (string, string) {
		t.Helper()
		now := time.Now().UTC()
		rawToken := "cross-transfer-" + videoID
		if _, err := env.store.CreateDeviceClaimToken(ctx, store.DeviceClaimTokenCreateInput{
			OrganizationID: &source.Organization.ID, TokenHash: auth.HashToken(rawToken),
			Category: model.DeviceCategoryIPCamera, VideoCloudDevid: videoID,
			ActivityID: "activity-" + videoID, ClipPublicKey: "clip-" + videoID,
			ExpiresAt: now.Add(time.Hour), Now: now,
		}); err != nil {
			t.Fatal(err)
		}
		resolved, err := env.store.ResolveDeviceClaimToken(ctx, store.DeviceClaimResolveInput{
			TokenHash: auth.HashToken(rawToken), OrganizationID: source.Organization.ID,
			RequestedBy: source.User.ID, DeviceName: "Cross Transfer Camera", Now: now.Add(time.Second),
		})
		if err != nil {
			t.Fatal(err)
		}
		return resolved.Claim.ID, resolved.Device.ID
	}
	transfer := func(claimID string) *httptest.ResponseRecorder {
		t.Helper()
		return performJSON(env.router, http.MethodPost, "/v1/admin/device-claims/"+claimID+"/transfer", map[string]any{
			"target_organization_id": target.Organization.ID,
			"reason":                 "cross-service transfer verification",
			"evidence":               map[string]any{"ticket": "CROSS-TRANSFER-E2E"},
		}, admin.Tokens.AccessToken)
	}
	remote := &httpVideoPresenceReader{baseURL: baseURL, token: token, client: &http.Client{Timeout: 3 * time.Second}}
	successClaimID, successAccountID := createClaim(successVideoID)
	if response := transfer(successClaimID); response.Code != http.StatusOK {
		t.Fatalf("cross-service transfer status=%d body=%s", response.Code, response.Body.String())
	}
	fence, err := remote.ReadTransferFence(ctx, successVideoID)
	if err != nil || len(fence.ReservationID) != 64 || fence.OrganizationID != target.Organization.ID || fence.AccountDeviceID != successAccountID {
		t.Fatalf("durable Video Cloud fence=%+v err=%v", fence, err)
	}
	deviceAfter, err := env.store.GetDevice(ctx, target.Organization.ID, successAccountID)
	if err != nil || deviceAfter.Metadata[model.DeviceMetadataVideoCloudTransferReservationID] != fence.ReservationID {
		t.Fatalf("account-side transfer binding=%+v err=%v", deviceAfter, err)
	}
	activate := func(reservationID, orgID string) int {
		t.Helper()
		body, err := json.Marshal(map[string]string{
			"devid": successVideoID, "clip_public_key": "clip-" + successVideoID,
			"activityid": "activity-" + successVideoID, "org_id": orgID,
			"account_device_id": successAccountID, "transfer_reservation_id": reservationID,
		})
		if err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost,
			baseURL+"/v1/internal/account-manager/devices/"+successVideoID+"/activate", strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusConflict && response.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(io.LimitReader(response.Body, 8192))
			t.Fatalf("unexpected activation status=%d body=%s", response.StatusCode, raw)
		}
		return response.StatusCode
	}
	if got := activate("", source.Organization.ID); got != http.StatusConflict {
		t.Fatalf("source activation bypassed transferred fence: %d", got)
	}
	if got := activate("wrong-generation", target.Organization.ID); got != http.StatusConflict {
		t.Fatalf("wrong-generation activation bypassed transferred fence: %d", got)
	}
	if got := activate(fence.ReservationID, target.Organization.ID); got != http.StatusOK {
		t.Fatalf("target activation with committed generation failed: %d", got)
	}
	if response := performJSON(env.router, http.MethodPost,
		"/v1/admin/device-claims/"+successClaimID+"/transfer-fence/cancel", map[string]any{
			"reservation_id": fence.ReservationID, "reason": "must reject committed transfer",
			"evidence": map[string]any{"ticket": "CROSS-TRANSFER-E2E"},
		}, admin.Tokens.AccessToken); response.Code != http.StatusConflict {
		t.Fatalf("committed fence cancellation status=%d body=%s", response.Code, response.Body.String())
	}

	// A proxy forwards the reservation to Video Cloud and then drops the HTTP
	// response, reproducing a committed remote write with an unknown local result.
	lostClaimID, lostAccountID := createClaim(lostVideoID)
	upstream, err := url.Parse(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	proxyHandler := httputil.NewSingleHostReverseProxy(upstream)
	var dropped atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost && strings.Contains(request.URL.Path, lostVideoID+"/transfer-fence") && !dropped.Swap(true) {
			forward, err := http.NewRequestWithContext(request.Context(), request.Method,
				baseURL+request.URL.RequestURI(), request.Body)
			if err == nil {
				forward.Header = request.Header.Clone()
				response, sendErr := http.DefaultClient.Do(forward)
				if sendErr == nil {
					_, _ = io.Copy(io.Discard, response.Body)
					_ = response.Body.Close()
				}
			}
			connection, _, hijackErr := w.(http.Hijacker).Hijack()
			if hijackErr == nil {
				_ = connection.Close()
			}
			return
		}
		proxyHandler.ServeHTTP(w, request)
	}))
	defer proxy.Close()
	if err := env.server.ConfigureVideoPresence(proxy.URL, token, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if response := transfer(lostClaimID); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("lost reservation response status=%d body=%s", response.Code, response.Body.String())
	}
	if !dropped.Load() {
		t.Fatal("reservation response was not dropped")
	}
	lostFence, err := remote.ReadTransferFence(ctx, lostVideoID)
	if err != nil || lostFence.OrganizationID != target.Organization.ID || lostFence.AccountDeviceID != lostAccountID {
		t.Fatalf("lost-response fence was not persisted: %+v err=%v", lostFence, err)
	}
	if _, err := env.store.GetDevice(ctx, source.Organization.ID, lostAccountID); err != nil {
		t.Fatalf("unknown remote outcome changed account owner: %v", err)
	}
	inspectPath := "/v1/admin/device-claims/" + lostClaimID + "/transfer-fence"
	if response := performJSON(env.router, http.MethodGet, inspectPath, nil, admin.Tokens.AccessToken); response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"status":"cancelable"`) {
		t.Fatalf("lost-response reconciliation status=%d body=%s", response.Code, response.Body.String())
	}
	if response := performJSON(env.router, http.MethodPost, inspectPath+"/cancel", map[string]any{
		"reservation_id": lostFence.ReservationID, "reason": "abandon unknown remote result",
		"evidence": map[string]any{"ticket": "CROSS-TRANSFER-E2E"},
	}, admin.Tokens.AccessToken); response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"status":"cancelled"`) {
		t.Fatalf("lost-response cancellation status=%d body=%s", response.Code, response.Body.String())
	}
	if _, err := remote.ReadTransferFence(ctx, lostVideoID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cancelled remote fence lookup = %v, want not found", err)
	}

	// A definite Account Manager transaction failure must release only the
	// reservation created by that attempt, leaving the source claim retryable.
	rollbackClaimID, rollbackAccountID := createClaim(rollbackVideoID)
	if _, err := env.store.TransferDeviceClaim(ctx, store.DeviceClaimTransferInput{
		ClaimID: rollbackClaimID, TargetOrganizationID: target.Organization.ID,
		ActorUserID: admin.User.ID, Reason: "force definite pre-commit failure",
		Evidence: map[string]any{"unserializable": make(chan int)},
		ReserveNoVideoCloudLifecycle: func(ctx context.Context, videoID, reservationID, orgID, accountID string) error {
			return env.server.reserveNoVideoCloudLifecycle(ctx, videoID, orgID, accountID, reservationID)
		},
		ReleaseNoVideoCloudLifecycle: env.server.releaseNoVideoCloudLifecycle,
	}); err == nil {
		t.Fatal("unserializable account audit unexpectedly committed")
	}
	if _, err := remote.ReadTransferFence(ctx, rollbackVideoID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("definite failure left remote fence: %v", err)
	}
	if _, err := env.store.GetDevice(ctx, source.Organization.ID, rollbackAccountID); err != nil {
		t.Fatalf("definite failure changed source owner: %v", err)
	}
	if response := transfer(rollbackClaimID); response.Code != http.StatusOK {
		t.Fatalf("transfer could not retry after definite failure status=%d body=%s", response.Code, response.Body.String())
	}
	if _, err := remote.ReadTransferFence(ctx, rollbackVideoID); err != nil {
		t.Fatalf("retry did not persist remote fence: %v", err)
	}
}
