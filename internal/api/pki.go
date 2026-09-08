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

	"github.com/gin-gonic/gin"
	"rtk_account_manager/internal/auth"
)

type pkiProxy struct {
	requireUserMFA bool
	base           *url.URL
	client         *http.Client
	environment    string
}
type pkiRoleStore interface {
	PKIRoles(context.Context, string) ([]string, error)
}

func pkiStepUpLocation(location string) (string, error) {
	acr := os.Getenv("PKI_OIDC_MFA_ACR")
	if acr == "" {
		return "", fmt.Errorf("MFA assurance class required")
	}
	u, err := url.Parse(location)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("acr_values", acr)
	q.Set("max_age", "0")
	q.Set("prompt", "login")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (s *Server) ConfigurePKIFromEnv() error {
	requireUserMFA, err := pkiUserMFASetting(os.Getenv("PKI_REQUIRE_USER_MFA"))
	if err != nil {
		return err
	}
	raw := os.Getenv("PKI_CONTROLLER_URL")
	if raw == "" {
		if os.Getenv("PKI_CONTROLLER_SOCKET") != "" {
			return fmt.Errorf("PKI socket requires controller HTTPS origin")
		}
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return fmt.Errorf("invalid PKI controller URL")
	}
	environment := os.Getenv("PKI_ENVIRONMENT")
	if environment == "" || (requireUserMFA && os.Getenv("PKI_OIDC_MFA_ACR") == "") {
		return fmt.Errorf("PKI environment required; optional user MFA requires an OIDC assurance class")
	}
	var client *http.Client
	if socket := os.Getenv("PKI_CONTROLLER_SOCKET"); socket != "" {
		if os.Getenv("PKI_CONTROLLER_CLIENT_CERT") != "" || os.Getenv("PKI_CONTROLLER_CLIENT_KEY") != "" || os.Getenv("PKI_CONTROLLER_CA") != "" {
			return fmt.Errorf("PKI socket and static controller credentials are mutually exclusive")
		}
		client, err = newPKISocketClient(u, socket)
		if err != nil {
			return err
		}
	} else {
		identity, err := tls.LoadX509KeyPair(os.Getenv("PKI_CONTROLLER_CLIENT_CERT"), os.Getenv("PKI_CONTROLLER_CLIENT_KEY"))
		if err != nil {
			return fmt.Errorf("load PKI controller client identity: %w", err)
		}
		ca, err := os.ReadFile(os.Getenv("PKI_CONTROLLER_CA"))
		if err != nil {
			return err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(ca) {
			return fmt.Errorf("invalid PKI controller trust bundle")
		}
		client = &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{identity}, RootCAs: pool}}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	s.pkiClient = &pkiProxy{requireUserMFA: requireUserMFA, base: u, environment: environment, client: client}
	return nil
}

func pkiUserMFASetting(value string) (bool, error) {
	if value == "" || value == "false" {
		return false, nil
	}
	if value != "true" {
		return false, fmt.Errorf("PKI_REQUIRE_USER_MFA must be true or false")
	}
	return true, nil
}

func (p *pkiProxy) acceptsPKISession(claims *auth.Claims, now time.Time) bool {
	if claims == nil || claims.SubjectType != auth.SubjectTypeUser || claims.UserID == "" {
		return false
	}
	return !p.requireUserMFA || (claims.MFA && claims.AuthenticationTime >= now.Add(-5*time.Minute).Unix() && claims.AuthenticationTime <= now.Unix()+30)
}

func (s *Server) applyOIDCStepUp(userID string, identity auth.OIDCIdentity, tokens tokenResponse) (tokenResponse, error) {
	t, ok := auth.OIDCStepUp(identity.Claims, os.Getenv("PKI_OIDC_MFA_ACR"), s.now())
	if !ok {
		return tokens, nil
	}
	access, expires, err := s.auth.IssueAssuredAccessToken(userID, t)
	if err != nil {
		return tokenResponse{}, err
	}
	tokens.AccessToken = access
	tokens.AccessTokenExpiresAt = expires
	return tokens, nil
}

func (s *Server) proxyPKI(c *gin.Context) {
	if s.pkiClient == nil {
		writeError(c, 503, "pki_unavailable", "PKI controller is not configured")
		return
	}
	claims, err := s.auth.ParseAccessToken(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
	now := s.now()
	if err != nil || !s.pkiClient.acceptsPKISession(claims, now) {
		if s.pkiClient.requireUserMFA {
			writeError(c, 403, "pki_step_up_required", "Recent OIDC MFA authentication is required")
		} else {
			writeError(c, 403, "pki_authentication_required", "An authenticated human user session is required")
		}
		return
	}
	roleStore, ok := s.store.(pkiRoleStore)
	if !ok {
		writeError(c, 503, "pki_unavailable", "PKI role storage is unavailable")
		return
	}
	roles, err := roleStore.PKIRoles(c.Request.Context(), claims.UserID)
	if err != nil || len(roles) == 0 {
		writeError(c, 403, "pki_denied", "PKI role required")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 256<<10))
	if err != nil {
		writeError(c, 400, "invalid_request", "Invalid PKI request")
		return
	}
	path := "/v1/pki" + c.Param("path")
	parts := strings.Split(strings.Trim(c.Param("path"), "/"), "/")
	if len(parts) == 0 || (parts[0] != "operations" && parts[0] != "issuers") || strings.Contains(path, "..") {
		writeError(c, 404, "not_found", "Unknown PKI route")
		return
	}
	input := auth.PKIAssertionInput{UserID: claims.UserID, Roles: roles, AuthenticationTime: claims.AuthenticationTime, Environment: s.pkiClient.environment, Method: c.Request.Method, Path: path, Body: body, IdempotencyKey: c.GetHeader("Idempotency-Key")}
	input.MFA = claims.MFA && claims.AuthenticationTime >= now.Add(-5*time.Minute).Unix() && claims.AuthenticationTime <= now.Unix()+30
	if c.Request.Method == "POST" && (len(parts) == 1 || (len(parts) == 3 && (parts[2] == "provision" || parts[2] == "activate"))) {
		var scope struct {
			CloudID     string `json:"brand_cloud_id"`
			ProductID   string `json:"device_item_profile_id"`
			Environment string `json:"environment"`
		}
		if len(parts) == 1 {
			if json.Unmarshal(body, &scope) != nil {
				writeError(c, 400, "invalid_request", "Invalid PKI scope")
				return
			}
		} else {
			var op struct {
				IssuerID string `json:"issuer_id"`
			}
			if s.pkiRead(c, input, "/v1/pki/operations/"+parts[1], &op) != nil || s.pkiRead(c, input, "/v1/pki/issuers/"+op.IssuerID, &scope) != nil {
				writeError(c, 503, "pki_unavailable", "Could not resolve PKI operation")
				return
			}
		}
		if scope.Environment != s.pkiClient.environment {
			writeError(c, 403, "pki_denied", "Wrong PKI environment")
			return
		}
		if scope.CloudID != "" {
			cloud, e := s.store.GetBrandCloud(c.Request.Context(), scope.CloudID)
			if e != nil || string(cloud.Status) != "active" {
				writeError(c, 403, "pki_denied", "Active Brand Cloud required")
				return
			}
		}
		if scope.ProductID != "" {
			product, e := s.store.GetDeviceItemProfile(c.Request.Context(), scope.CloudID, scope.ProductID)
			if e != nil || string(product.Status) != "active" || product.BrandCloudID != scope.CloudID {
				writeError(c, 403, "pki_denied", "Active Product in the Brand Cloud required")
				return
			}
		}
		input.ActiveContext = true
	}
	status, response, err := s.pkiCall(c.Request.Context(), input, now)
	if err != nil {
		writeError(c, 503, "pki_unavailable", "PKI controller request failed")
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Data(status, "application/json", response)
}

func (s *Server) pkiRead(c *gin.Context, input auth.PKIAssertionInput, path string, out any) error {
	input.Method = "GET"
	input.Path = path
	input.Body = nil
	input.IdempotencyKey = ""
	input.ActiveContext = false
	status, body, err := s.pkiCall(c.Request.Context(), input, s.now())
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("PKI lookup rejected")
	}
	return json.Unmarshal(body, out)
}

func (s *Server) pkiCall(ctx context.Context, input auth.PKIAssertionInput, now time.Time) (int, []byte, error) {
	assertion, err := s.auth.SignPKIAssertion(input, now)
	if err != nil {
		return 0, nil, err
	}
	u := *s.pkiClient.base
	u.Path = input.Path
	req, err := http.NewRequestWithContext(ctx, input.Method, u.String(), bytes.NewReader(input.Body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+assertion)
	req.Header.Set("Content-Type", "application/json")
	if input.IdempotencyKey != "" {
		req.Header.Set("Idempotency-Key", input.IdempotencyKey)
	}
	resp, err := s.pkiClient.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body, err
}
