package emaildelivery

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
)

type SendMailHTTPConfig struct {
	BaseURL     string
	BearerToken string
	Timeout     time.Duration
	HTTPClient  *http.Client
}

type SendMailHTTPClient struct {
	endpoint    string
	bearerToken string
	client      *http.Client
}

type sendMailHTTPRequest struct {
	To      []string `json:"to"`
	CC      []string `json:"cc,omitempty"`
	BCC     []string `json:"bcc,omitempty"`
	ReplyTo string   `json:"reply_to,omitempty"`
	Subject string   `json:"subject"`
	Text    string   `json:"text,omitempty"`
	HTML    string   `json:"html,omitempty"`
}

type sendMailHTTPAccepted struct {
	Status string `json:"status"`
}

func NewSendMailHTTPClient(config SendMailHTTPConfig) (*SendMailHTTPClient, error) {
	baseURL, err := url.Parse(strings.TrimSpace(config.BaseURL))
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return nil, errors.New("SENDMAIL_HTTP_BASE_URL must be an absolute URL")
	}
	if baseURL.User != nil || baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return nil, errors.New("SENDMAIL_HTTP_BASE_URL must be a credential-free origin")
	}
	if baseURL.Path != "" && baseURL.Path != "/" {
		return nil, errors.New("SENDMAIL_HTTP_BASE_URL must not contain a path")
	}
	if strings.TrimSpace(config.BearerToken) == "" {
		return nil, errors.New("SENDMAIL_HTTP_BEARER_TOKEN is required")
	}
	if config.Timeout <= 0 {
		config.Timeout = 15 * time.Second
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: config.Timeout}
	}
	baseURL.Path = "/send"
	return &SendMailHTTPClient{
		endpoint:    baseURL.String(),
		bearerToken: strings.TrimSpace(config.BearerToken),
		client:      client,
	}, nil
}

func (s *SendMailHTTPClient) Send(ctx context.Context, message Message) error {
	if strings.TrimSpace(message.Recipient) == "" {
		return &DeliveryError{Err: errors.New("sendmail_http recipient is required"), Transient: false}
	}
	if strings.TrimSpace(message.Subject) == "" {
		return &DeliveryError{Err: errors.New("sendmail_http subject is required"), Transient: false}
	}
	if message.Text == "" && message.HTML == "" {
		return &DeliveryError{Err: errors.New("sendmail_http text or html body is required"), Transient: false}
	}
	body, err := json.Marshal(sendMailHTTPRequest{
		To: []string{message.Recipient}, ReplyTo: message.ReplyTo,
		Subject: message.Subject, Text: message.Text, HTML: message.HTML,
	})
	if err != nil {
		return &DeliveryError{Err: fmt.Errorf("encode sendmail_http request: %w", err), Transient: false}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return &DeliveryError{Err: fmt.Errorf("create sendmail_http request: %w", err), Transient: false}
	}
	request.Header.Set("Authorization", "Bearer "+s.bearerToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		return &DeliveryError{Err: fmt.Errorf("sendmail_http request failed: %w", err), Transient: true}
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil {
		return &DeliveryError{Err: fmt.Errorf("read sendmail_http response: %w", err), Transient: true}
	}
	if len(responseBody) > 4096 {
		return &DeliveryError{Err: errors.New("sendmail_http response exceeded 4096 bytes"), Transient: false}
	}
	if response.StatusCode != http.StatusAccepted {
		transient := response.StatusCode == http.StatusRequestTimeout ||
			response.StatusCode == http.StatusTooManyRequests ||
			response.StatusCode >= http.StatusInternalServerError
		return &DeliveryError{
			Err:       fmt.Errorf("sendmail_http returned HTTP %d", response.StatusCode),
			Transient: transient,
		}
	}
	var accepted sendMailHTTPAccepted
	if err := json.Unmarshal(responseBody, &accepted); err != nil || accepted.Status != "sent" {
		return &DeliveryError{Err: errors.New("sendmail_http returned an invalid accepted response"), Transient: false}
	}
	return nil
}
