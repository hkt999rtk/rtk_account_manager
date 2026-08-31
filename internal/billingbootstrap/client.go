package billingbootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"rtk_account_manager/internal/store"
)

var ErrInvalid = errors.New("invalid Billing cloud creation configuration or event")
var ErrUnavailable = errors.New("Billing cloud creation evidence unavailable")

type Config struct {
	BaseURL, Token string
	Transport      http.RoundTripper
}
type Client struct {
	baseURL, token string
	http           *http.Client
}

func New(in Config) (*Client, error) {
	u, err := url.Parse(in.BaseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" || len(in.Token) < 32 || strings.TrimSpace(in.Token) != in.Token || strings.ContainsAny(in.Token, " \t\r\n") {
		return nil, ErrInvalid
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && ip != nil && ip.IsLoopback()) {
		return nil, ErrInvalid
	}
	u.Path = ""
	return &Client{u.String(), in.Token, &http.Client{Transport: in.Transport, Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (c *Client) Bootstrap(ctx context.Context, e store.BillingCloudCreation) (store.BillingCloudCreationReceipt, error) {
	if !e.Valid() {
		return store.BillingCloudCreationReceipt{}, ErrInvalid
	}
	if c == nil || c.http == nil {
		return store.BillingCloudCreationReceipt{}, ErrUnavailable
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return store.BillingCloudCreationReceipt{}, ErrInvalid
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/internal/billing/cloud-creations", bytes.NewReader(raw))
	if err != nil {
		return store.BillingCloudCreationReceipt{}, ErrInvalid
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return store.BillingCloudCreationReceipt{}, ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return store.BillingCloudCreationReceipt{}, ErrUnavailable
	}
	raw, err = io.ReadAll(io.LimitReader(resp.Body, (16<<10)+1))
	if err != nil || len(raw) > 16<<10 {
		return store.BillingCloudCreationReceipt{}, ErrUnavailable
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var out store.BillingCloudCreationReceipt
	var account pgtype.UUID
	if decoder.Decode(&out) != nil || decoder.Decode(new(any)) != io.EOF || !out.Valid() || out.EventID != e.EventID || out.CloudID != e.CloudID || out.OwnerUserID != e.OwnerUserID || out.OwnershipVersion != e.OwnershipVersion || !out.OccurredAt.Equal(e.OccurredAt) || out.EvidenceSHA256 != e.EvidenceSHA256 || account.Scan(out.AccountID) != nil || !account.Valid || account.Bytes == [16]byte{} || account.String() != out.AccountID {
		return store.BillingCloudCreationReceipt{}, ErrUnavailable
	}
	return out, nil
}
