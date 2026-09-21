package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"rtk_account_manager/internal/store"
)

var errVideoPresenceNotFound = errors.New("video cloud device not found")

type videoPresenceSnapshot struct {
	DeviceID        string     `json:"devid"`
	OrgID           string     `json:"org_id"`
	AccountDeviceID string     `json:"account_device_id"`
	Activated       bool       `json:"activated"`
	Online          bool       `json:"online"`
	OwnerTransport  string     `json:"owner_transport"`
	LastSeenAt      *time.Time `json:"last_seen_at"`
	ObservedAt      time.Time  `json:"observed_at"`
}

type videoPresenceReader interface {
	ReadPresence(context.Context, string) (videoPresenceSnapshot, error)
}

type videoTransferFencer interface {
	ReadTransferFence(context.Context, string) (store.DeviceTransferFence, error)
	ReserveTransferFence(context.Context, string, string, string, string) error
	ReleaseTransferFence(context.Context, string, string) error
}

type httpVideoPresenceReader struct {
	baseURL string
	token   string
	client  *http.Client
}

func (s *Server) ConfigureVideoPresence(baseURL, token string, timeout time.Duration) error {
	baseURL = strings.TrimSpace(baseURL)
	token = strings.TrimSpace(token)
	if baseURL == "" && token == "" {
		return nil
	}
	endpoint, err := url.Parse(baseURL)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" ||
		endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" ||
		(endpoint.Path != "" && endpoint.Path != "/") || token == "" {
		return fmt.Errorf("Video Cloud presence requires a credential-free HTTP origin and internal token")
	}
	if timeout <= 0 {
		return fmt.Errorf("Video Cloud presence timeout must be positive")
	}
	s.videoPresence = &httpVideoPresenceReader{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		client: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	return nil
}

func (r *httpVideoPresenceReader) ReadPresence(ctx context.Context, deviceID string) (videoPresenceSnapshot, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		r.baseURL+"/v1/internal/account-manager/devices/"+url.PathEscape(deviceID)+"/presence", nil)
	if err != nil {
		return videoPresenceSnapshot{}, err
	}
	request.Header.Set("Authorization", "Bearer "+r.token)
	response, err := r.client.Do(request)
	if err != nil {
		return videoPresenceSnapshot{}, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		body, err := io.ReadAll(io.LimitReader(response.Body, 8193))
		if err != nil || len(body) > 8192 {
			return videoPresenceSnapshot{}, fmt.Errorf("Video Cloud presence returned unverified 404")
		}
		var failure struct {
			Status string `json:"status"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(body, &failure); err == nil &&
			failure.Status == "fail" && failure.Reason == "device not found" {
			return videoPresenceSnapshot{}, errVideoPresenceNotFound
		}
	}
	if response.StatusCode != http.StatusOK {
		return videoPresenceSnapshot{}, fmt.Errorf("Video Cloud presence returned %s", response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 8193))
	if err != nil {
		return videoPresenceSnapshot{}, fmt.Errorf("read Video Cloud presence: %w", err)
	}
	if len(body) > 8192 {
		return videoPresenceSnapshot{}, fmt.Errorf("Video Cloud presence exceeds response limit")
	}
	var snapshot videoPresenceSnapshot
	if err := json.Unmarshal(body, &snapshot); err != nil {
		return videoPresenceSnapshot{}, fmt.Errorf("decode Video Cloud presence: %w", err)
	}
	return snapshot, nil
}

// A durable Video Cloud reservation closes the gap between a remote 404 read
// and the Account Manager transaction commit. An existing row, including an
// inactive one, or a competing reservation rejects account-only transfer.
func (s *Server) reserveNoVideoCloudLifecycle(ctx context.Context, deviceID, targetOrgID, accountDeviceID, reservationID string) error {
	fencer, ok := s.videoPresence.(videoTransferFencer)
	if !ok || strings.TrimSpace(deviceID) == "" {
		return store.ErrClaimLifecycleCheckUnavailable
	}
	err := fencer.ReserveTransferFence(ctx, deviceID, reservationID, targetOrgID, accountDeviceID)
	if err != nil {
		if errors.Is(err, store.ErrClaimLifecycleBound) {
			return err
		}
		return fmt.Errorf("%w: %v", store.ErrClaimLifecycleCheckUnavailable, err)
	}
	return nil
}

func (s *Server) releaseNoVideoCloudLifecycle(ctx context.Context, deviceID, reservationID string) error {
	fencer, ok := s.videoPresence.(videoTransferFencer)
	if !ok || strings.TrimSpace(deviceID) == "" {
		return store.ErrClaimLifecycleCheckUnavailable
	}
	if err := fencer.ReleaseTransferFence(ctx, deviceID, reservationID); err != nil {
		return fmt.Errorf("release Video Cloud transfer fence: %w", err)
	}
	return nil
}

func (r *httpVideoPresenceReader) ReserveTransferFence(ctx context.Context, deviceID, reservationID, targetOrgID, accountDeviceID string) error {
	body, err := json.Marshal(map[string]string{
		"reservation_id": reservationID, "org_id": targetOrgID, "account_device_id": accountDeviceID,
	})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		r.baseURL+"/v1/internal/account-manager/devices/"+url.PathEscape(deviceID)+"/transfer-fence", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+r.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := r.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 8193))
	if err != nil || len(raw) > 8192 {
		return fmt.Errorf("Video Cloud transfer fence response unreadable")
	}
	if response.StatusCode == http.StatusConflict {
		var failure struct {
			Status string `json:"status"`
			Reason string `json:"reason"`
		}
		if json.Unmarshal(raw, &failure) == nil && failure.Status == "fail" &&
			(failure.Reason == "cloud_lifecycle_bound" || failure.Reason == "transfer_fence_conflict") {
			return store.ErrClaimLifecycleBound
		}
	}
	if response.StatusCode != http.StatusCreated {
		return fmt.Errorf("Video Cloud transfer fence returned %s", response.Status)
	}
	var receipt struct {
		Status        string `json:"status"`
		ReservationID string `json:"reservation_id"`
	}
	if json.Unmarshal(raw, &receipt) != nil || receipt.Status != "ok" || receipt.ReservationID != reservationID {
		return fmt.Errorf("Video Cloud transfer fence returned invalid receipt")
	}
	return nil
}

func (r *httpVideoPresenceReader) ReleaseTransferFence(ctx context.Context, deviceID, reservationID string) error {
	body, err := json.Marshal(map[string]string{"reservation_id": reservationID})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		r.baseURL+"/v1/internal/account-manager/devices/"+url.PathEscape(deviceID)+"/transfer-fence", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+r.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := r.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("Video Cloud transfer fence cancellation returned %s", response.Status)
	}
	return nil
}

func (r *httpVideoPresenceReader) ReadTransferFence(ctx context.Context, deviceID string) (store.DeviceTransferFence, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		r.baseURL+"/v1/internal/account-manager/devices/"+url.PathEscape(deviceID)+"/transfer-fence", nil)
	if err != nil {
		return store.DeviceTransferFence{}, err
	}
	request.Header.Set("Authorization", "Bearer "+r.token)
	response, err := r.client.Do(request)
	if err != nil {
		return store.DeviceTransferFence{}, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 8193))
	if err != nil || len(raw) > 8192 {
		return store.DeviceTransferFence{}, fmt.Errorf("Video Cloud transfer fence lookup unreadable")
	}
	if response.StatusCode == http.StatusNotFound {
		var failure struct {
			Status string `json:"status"`
			Reason string `json:"reason"`
		}
		if json.Unmarshal(raw, &failure) == nil && failure.Status == "fail" && failure.Reason == "transfer_fence_not_found" {
			return store.DeviceTransferFence{}, store.ErrNotFound
		}
	}
	if response.StatusCode != http.StatusOK {
		return store.DeviceTransferFence{}, fmt.Errorf("Video Cloud transfer fence lookup returned %s", response.Status)
	}
	var fence store.DeviceTransferFence
	if json.Unmarshal(raw, &fence) != nil || fence.DeviceID != deviceID ||
		strings.TrimSpace(fence.ReservationID) == "" || strings.TrimSpace(fence.OrganizationID) == "" ||
		strings.TrimSpace(fence.AccountDeviceID) == "" {
		return store.DeviceTransferFence{}, fmt.Errorf("Video Cloud transfer fence lookup returned invalid binding")
	}
	return fence, nil
}
