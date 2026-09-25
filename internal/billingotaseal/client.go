package billingotaseal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	"rtk_account_manager/internal/store"
)

var (
	ErrInvalid     = errors.New("invalid OTA Platform period seal configuration")
	ErrUnavailable = errors.New("Billing OTA period seal unavailable")
	ErrConflict    = errors.New("Billing OTA period seal conflict")
)

type Config struct {
	BaseURL   string
	Token     string
	Transport http.RoundTripper
}

type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

func New(config Config) (*Client, error) {
	u, err := url.Parse(config.BaseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" ||
		u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") ||
		u.RawPath != "" || len(config.Token) < 32 ||
		strings.TrimSpace(config.Token) != config.Token ||
		strings.ContainsAny(config.Token, " \t\r\n") {
		return nil, ErrInvalid
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && ip != nil && ip.IsLoopback()) {
		return nil, ErrInvalid
	}
	u.Path = ""
	return &Client{
		baseURL: u.String(), token: config.Token,
		http: &http.Client{
			Transport: config.Transport,
			Timeout:   12 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// Submit accepts only an exact canonical echo. A transport ambiguity leaves
// the deterministic seal unchanged so the caller can safely retry.
func (c *Client) Submit(ctx context.Context, seal store.OTAPeriodGrantSeal) (bool, error) {
	if c == nil || c.http == nil || seal.IssuerKind != "platform_grants" ||
		seal.SealID == "" || seal.SourceSHA256 == "" {
		return false, ErrInvalid
	}
	body, err := json.Marshal(seal)
	if err != nil {
		return false, ErrInvalid
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/internal/billing/ota-period-seals", bytes.NewReader(body))
	if err != nil {
		return false, ErrInvalid
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return false, ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		return false, ErrConflict
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return false, ErrUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (32<<10)+1))
	if err != nil || len(raw) > 32<<10 {
		return false, ErrUnavailable
	}
	var result struct {
		Seal      json.RawMessage `json:"ota_period_seal"`
		Duplicate bool            `json:"duplicate"`
	}
	if json.Unmarshal(raw, &result) != nil ||
		result.Duplicate != (resp.StatusCode == http.StatusOK) ||
		!sameJSON(body, result.Seal) {
		return false, ErrUnavailable
	}
	return result.Duplicate, nil
}

func sameJSON(a, b []byte) bool {
	var left, right any
	decode := func(raw []byte, target *any) bool {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		return decoder.Decode(target) == nil && decoder.Decode(new(any)) == io.EOF
	}
	return decode(a, &left) && decode(b, &right) && reflect.DeepEqual(left, right)
}
