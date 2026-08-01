package lifecyclehttp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"

	"rtk_account_manager/internal/broker"
	"rtk_account_manager/internal/channel"
	"rtk_account_manager/internal/store"
	"rtk_account_manager/internal/worker/inbox"
)

const AdapterDirectHTTP = "direct_http"

type Options struct {
	BaseURL     string
	Token       string
	Timeout     time.Duration
	HTTPClient  *http.Client
	Store       *store.Store
	MaxAttempts int
	Now         func() time.Time
	Logger      *zap.Logger
	Project     func(context.Context, broker.Message) error
}

type Publisher struct {
	baseURL     string
	token       string
	client      *http.Client
	store       *store.Store
	maxAttempts int
	now         func() time.Time
	logger      *zap.Logger
	project     func(context.Context, broker.Message) error
}

func NewPublisher(opts Options) (*Publisher, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("video cloud lifecycle base URL must be absolute")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, fmt.Errorf("video cloud lifecycle base URL must be a credential-free origin")
	}
	if strings.TrimSpace(opts.Token) == "" {
		return nil, fmt.Errorf("video cloud lifecycle token is required")
	}
	if opts.Store == nil && opts.Project == nil {
		return nil, fmt.Errorf("account manager store is required")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: opts.Timeout}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = zap.NewNop()
	}
	publisher := &Publisher{baseURL: baseURL, token: opts.Token, client: opts.HTTPClient, store: opts.Store, maxAttempts: opts.MaxAttempts, now: opts.Now, logger: opts.Logger, project: opts.Project}
	if publisher.project == nil {
		publisher.project = publisher.projectWithInbox
	}
	return publisher, nil
}

func (p *Publisher) Publish(ctx context.Context, stream string, envelope channel.Envelope) error {
	if stream != channel.StreamAccountVideoCommands {
		return fmt.Errorf("direct lifecycle publisher does not support stream %q", stream)
	}
	payload, err := envelope.ValidateAndDecode(stream)
	if err != nil {
		return err
	}

	action, deviceID, body, err := directRequest(payload)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/internal/account-manager/devices/"+url.PathEscape(deviceID)+"/"+action, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+p.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return broker.Transient(fmt.Errorf("call video lifecycle API: %w", err))
	}
	defer response.Body.Close()

	now := p.now().UTC()
	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= http.StatusInternalServerError {
		io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return broker.Transient(fmt.Errorf("video lifecycle API returned %s", response.Status))
	}

	result, err := resultEnvelope(envelope, payload, now, response)
	if err != nil {
		return err
	}
	if err := p.project(ctx, broker.Message{Stream: channel.StreamVideoAccountEvents, Envelope: result}); err != nil {
		return broker.Transient(fmt.Errorf("project video lifecycle result: %w", err))
	}
	return nil
}

func (p *Publisher) projectWithInbox(ctx context.Context, message broker.Message) error {
	consumer := &singleMessageConsumer{message: message}
	stats, err := inbox.NewService(p.store, consumer, inbox.Options{
		Stream: channel.StreamVideoAccountEvents, MaxAttempts: p.maxAttempts, Now: p.now, Logger: p.logger,
	}).RunOnce(ctx)
	if err != nil {
		return err
	}
	if stats.Processed != 1 && stats.Skipped != 1 {
		return fmt.Errorf("video lifecycle result was not processed: %+v", stats)
	}
	return nil
}

func (p *Publisher) Close(context.Context) error { return nil }

func directRequest(payload channel.Payload) (action, deviceID string, body []byte, err error) {
	switch typed := payload.(type) {
	case *channel.DeviceProvisionRequestedPayload:
		body, err = json.Marshal(map[string]any{
			"devid": typed.VideoCloudDevid, "clip_public_key": typed.ClipPublicKey, "activityid": typed.ActivityID,
			"org_id": typed.OrgID, "account_device_id": typed.AccountDeviceID,
		})
		return "activate", typed.VideoCloudDevid, body, err
	case *channel.DeviceDeactivateRequestedPayload:
		body, err = json.Marshal(map[string]string{"devid": typed.VideoCloudDevid})
		return "deactivate", typed.VideoCloudDevid, body, err
	case *channel.DeviceUnprovisionRequestedPayload:
		body, err = json.Marshal(map[string]string{"devid": typed.VideoCloudDevid})
		return "unprovision", typed.VideoCloudDevid, body, err
	default:
		return "", "", nil, fmt.Errorf("unsupported direct lifecycle payload %T", payload)
	}
}

func resultEnvelope(request channel.Envelope, payload channel.Payload, now time.Time, response *http.Response) (channel.Envelope, error) {
	succeeded := response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices
	var result channel.Payload
	var messageType channel.MessageType
	if succeeded {
		switch typed := payload.(type) {
		case *channel.DeviceProvisionRequestedPayload:
			messageType = channel.MessageTypeDeviceProvisionSucceeded
			result = &channel.DeviceProvisionSucceededPayload{OrgID: typed.OrgID, AccountDeviceID: typed.AccountDeviceID, VideoCloudDevid: typed.VideoCloudDevid, ActivityID: typed.ActivityID, ActivatedAt: now}
		case *channel.DeviceDeactivateRequestedPayload:
			messageType = channel.MessageTypeDeviceDeactivateSucceeded
			result = &channel.DeviceDeactivateSucceededPayload{OrgID: typed.OrgID, AccountDeviceID: typed.AccountDeviceID, VideoCloudDevid: typed.VideoCloudDevid, DeactivatedAt: now}
		case *channel.DeviceUnprovisionRequestedPayload:
			messageType = channel.MessageTypeDeviceUnprovisionSucceeded
			result = &channel.DeviceUnprovisionSucceededPayload{OrgID: typed.OrgID, AccountDeviceID: typed.AccountDeviceID, VideoCloudDevid: typed.VideoCloudDevid, UnprovisionedAt: now}
		}
	} else {
		reason := failureReason(response)
		switch typed := payload.(type) {
		case *channel.DeviceProvisionRequestedPayload:
			messageType = channel.MessageTypeDeviceProvisionFailed
			result = &channel.DeviceProvisionFailedPayload{OrgID: typed.OrgID, AccountDeviceID: typed.AccountDeviceID, VideoCloudDevid: typed.VideoCloudDevid, ActivityID: typed.ActivityID, ErrorCode: "activation_failed", ErrorMessage: reason, FailedAt: now}
		case *channel.DeviceDeactivateRequestedPayload:
			messageType = channel.MessageTypeDeviceDeactivateFailed
			result = &channel.DeviceDeactivateFailedPayload{OrgID: typed.OrgID, AccountDeviceID: typed.AccountDeviceID, VideoCloudDevid: typed.VideoCloudDevid, ErrorCode: "deactivation_failed", ErrorMessage: reason, FailedAt: now}
		case *channel.DeviceUnprovisionRequestedPayload:
			messageType = channel.MessageTypeDeviceUnprovisionFailed
			result = &channel.DeviceUnprovisionFailedPayload{OrgID: typed.OrgID, AccountDeviceID: typed.AccountDeviceID, VideoCloudDevid: typed.VideoCloudDevid, ErrorCode: "unprovision_failed", ErrorMessage: reason, FailedAt: now}
		}
	}
	payloadJSON, err := json.Marshal(result)
	if err != nil {
		return channel.Envelope{}, err
	}
	envelope := channel.Envelope{
		MessageID: request.MessageID + "-result", CorrelationID: request.CorrelationID, CausationID: request.MessageID,
		OperationID: request.OperationID, SourceService: channel.ServiceRealtekVideoCloud, TargetService: channel.ServiceAccountManager,
		MessageType: messageType, SchemaVersion: channel.SchemaVersionV1, PartitionKey: request.PartitionKey, OccurredAt: now, Payload: payloadJSON,
	}
	if _, err := envelope.ValidateAndDecode(channel.StreamVideoAccountEvents); err != nil {
		return channel.Envelope{}, err
	}
	return envelope, nil
}

func failureReason(response *http.Response) string {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	var decoded struct {
		Reason string `json:"reason"`
	}
	if json.Unmarshal(body, &decoded) == nil && strings.TrimSpace(decoded.Reason) != "" {
		return truncate(strings.TrimSpace(decoded.Reason), 512)
	}
	return "video lifecycle API returned " + response.Status
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

type singleMessageConsumer struct {
	message broker.Message
	done    bool
}

func (c *singleMessageConsumer) Receive(context.Context, int) ([]broker.Message, error) {
	if c.done {
		return nil, nil
	}
	c.done = true
	return []broker.Message{c.message}, nil
}
func (*singleMessageConsumer) Close(context.Context) error { return nil }
