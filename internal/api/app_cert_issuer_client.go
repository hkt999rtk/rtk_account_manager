package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type HTTPAppCertificateIssuerConfig struct {
	BaseURL    string
	ClientCert string
	ClientKey  string
	CAFile     string
	Timeout    time.Duration
}

type HTTPAppCertificateIssuer struct {
	baseURL *url.URL
	client  *http.Client
}

func NewHTTPAppCertificateIssuer(cfg HTTPAppCertificateIssuerConfig) (*HTTPAppCertificateIssuer, error) {
	baseURL, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil || baseURL == nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return nil, fmt.Errorf("app certissuer base url is invalid")
	}
	cert, err := tls.LoadX509KeyPair(cfg.ClientCert, cfg.ClientKey)
	if err != nil {
		return nil, fmt.Errorf("load app certissuer client certificate: %w", err)
	}
	caPEM, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read app certissuer ca bundle: %w", err)
	}
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("app certissuer ca bundle contains no certificates")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &HTTPAppCertificateIssuer{
		baseURL: baseURL,
		client: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{
				MinVersion:   tls.VersionTLS12,
				Certificates: []tls.Certificate{cert},
				RootCAs:      roots,
			}},
		},
	}, nil
}

func (c *HTTPAppCertificateIssuer) IssueAppCertificate(ctx context.Context, reqBody AppCertificateIssueRequest) (AppCertificateIssueResponse, error) {
	if c == nil || c.client == nil || c.baseURL == nil {
		return AppCertificateIssueResponse{}, fmt.Errorf("app certissuer client is not configured")
	}
	raw, err := json.Marshal(map[string]any{
		"request_id": reqBody.RequestID,
		"user_id":    reqBody.UserID,
		"csr_pem":    reqBody.CSRPem,
		"ttl_days":   reqBody.TTLDays,
	})
	if err != nil {
		return AppCertificateIssueResponse{}, fmt.Errorf("marshal app certissuer request: %w", err)
	}
	endpoint := c.baseURL.ResolveReference(&url.URL{Path: strings.TrimRight(c.baseURL.Path, "/") + "/v1/certificates/app/issue"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(raw))
	if err != nil {
		return AppCertificateIssueResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return AppCertificateIssueResponse{}, fmt.Errorf("call app certissuer: %w", err)
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, 1<<20)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var out struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.NewDecoder(limited).Decode(&out)
		return AppCertificateIssueResponse{}, fmt.Errorf("app certissuer status %d: %s", resp.StatusCode, out.Error.Code)
	}
	var out AppCertificateIssueResponse
	if err := json.NewDecoder(limited).Decode(&out); err != nil {
		return AppCertificateIssueResponse{}, fmt.Errorf("decode app certissuer response: %w", err)
	}
	return out, nil
}
