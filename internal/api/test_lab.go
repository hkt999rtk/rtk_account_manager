package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"rtk_account_manager/internal/store"
)

type testLabRuntime struct {
	store         *store.Store
	origin, token string
	client        *http.Client
}

func testLabEnabled() bool {
	env := strings.ToLower(strings.TrimSpace(os.Getenv("ACCOUNT_MANAGER_ENV")))
	return strings.EqualFold(os.Getenv("TEST_LAB_ENABLED"), "true") && (env == "dev" || env == "development" || env == "local" || env == "staging")
}

func (s *Server) ConfigureTestLab(repository *store.Store, origin, token string) {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || token == "" {
		return
	}
	s.testLab = &testLabRuntime{repository, strings.TrimRight(origin, "/"), token, &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (s *Server) requireTestLab(c *gin.Context) bool {
	c.Header("Cache-Control", "no-store")
	if !testLabEnabled() {
		writeError(c, 404, "test_lab_disabled", "Test Lab is disabled")
		return false
	}
	if s.testLab == nil {
		writeError(c, 503, "test_lab_unavailable", "Test Lab runtime is unavailable")
		return false
	}
	return requireManagedCloudSession(c)
}

func (s *Server) createTestLabSession(c *gin.Context) {
	if !s.requireTestLab(c) {
		return
	}
	var in struct {
		ProductID string `json:"product_id"`
		DeviceID  string `json:"device_id"`
		AccountID string `json:"account_id"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&in) != nil || !labUUID.MatchString(in.ProductID) || !labUUID.MatchString(in.DeviceID) || !labUUID.MatchString(in.AccountID) {
		writeError(c, 400, "invalid_scope", "Product, device and authorized test account are required")
		return
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		writeError(c, 400, "invalid_request", "One JSON object required")
		return
	}
	result, err := s.testLab.store.CreateTestLabSession(c.Request.Context(), currentUserID(c), c.Param("brandCloudId"), in.ProductID, in.DeviceID, in.AccountID)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusCreated, result)
}

func (s *Server) closeTestLabSession(c *gin.Context) {
	if !s.requireTestLab(c) {
		return
	}
	err := s.testLab.store.CloseTestLabSession(c.Request.Context(), c.Param("sessionId"), currentUserID(c), c.Param("brandCloudId"))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (s *Server) testLabCredentials(c *gin.Context) {
	if !s.requireTestLab(c) {
		return
	}
	session, err := s.testLab.store.GetTestLabSession(c.Request.Context(), c.Param("sessionId"))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	if session.UserID != currentUserID(c) || session.CloudID != c.Param("brandCloudId") {
		writeError(c, 404, "not_found", "Session not found")
		return
	}
	profile, err := s.testLab.store.GetDeviceItemProfile(c.Request.Context(), session.CloudID, session.ProductID)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	services := []string{}
	for _, v := range profile.ServiceOptions {
		if v == "mqtt" || v == "iot_shadow" || v == "video_streaming" {
			services = append(services, v)
		}
	}
	if len(services) == 0 {
		writeError(c, 403, "services_unavailable", "No test services enabled")
		return
	}
	raw, _ := json.Marshal(map[string]any{"session_id": session.ID, "devid": session.Devid, "brand_cloud_id": session.CloudID, "services": services})
	req, err := http.NewRequestWithContext(c.Request.Context(), "POST", s.testLab.origin+"/v1/internal/account-manager/test-lab/token", bytes.NewReader(raw))
	if err != nil {
		writeError(c, 503, "runtime_unavailable", "Runtime unavailable")
		return
	}
	req.Header.Set("Authorization", "Bearer "+s.testLab.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.testLab.client.Do(req)
	if err != nil {
		writeError(c, 503, "runtime_unavailable", "Runtime unavailable")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		writeError(c, 503, "runtime_unavailable", "Runtime authorization unavailable")
		return
	}
	var out map[string]json.RawMessage
	if json.NewDecoder(io.LimitReader(resp.Body, 32768)).Decode(&out) != nil {
		writeError(c, 502, "invalid_runtime_response", "Invalid runtime response")
		return
	}
	c.JSON(200, out)
}
