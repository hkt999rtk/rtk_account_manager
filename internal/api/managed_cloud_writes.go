package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/store"
)

func requireManagedCloudSession(c *gin.Context) bool {
	if currentSubjectType(c) != auth.SubjectTypeUser || currentUserID(c) == "" {
		writeError(c, http.StatusForbidden, "global_account_required", "Cloud management requires a global human account session")
		return false
	}
	return true
}

func bindManagedCloudWrite(c *gin.Context) (store.ManagedCloudWrite, bool) {
	var in store.ManagedCloudWrite
	invalid := func() (store.ManagedCloudWrite, bool) {
		writeError(c, http.StatusBadRequest, "invalid_request", "A JSON object with unique name/description string fields and Idempotency-Key (1-200 printable characters) is required")
		return in, false
	}
	if len(c.Request.Header.Values("Idempotency-Key")) != 1 || !store.ValidManagedCloudKey(c.GetHeader("Idempotency-Key")) {
		return invalid()
	}
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 16*1024))
	if err != nil || !utf8.Valid(raw) {
		return invalid()
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	start, err := d.Token()
	if err != nil || start != json.Delim('{') {
		return invalid()
	}
	seen := map[string]bool{}
	for d.More() {
		token, err := d.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[name] || (name != "name" && name != "description") {
			return invalid()
		}
		seen[name] = true
		var value *string
		if err := d.Decode(&value); err != nil || value == nil {
			return invalid()
		}
		if name == "name" {
			in.Name = value
		} else {
			in.Description = value
		}
	}
	end, err := d.Token()
	if err != nil || end != json.Delim('}') {
		return invalid()
	}
	if _, err := d.Token(); err != io.EOF {
		return invalid()
	}
	return in, true
}

func (s *Server) updateDeveloperBrandCloud(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if !requireManagedCloudSession(c) {
		return
	}
	req, ok := bindManagedCloudWrite(c)
	if !ok {
		return
	}
	cloud, err := s.store.UpdateManagedBrandCloud(c.Request.Context(), currentUserID(c), c.Param("brandCloudId"), c.GetHeader("Idempotency-Key"), req)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	s.writeManagedCloudResponse(c, http.StatusOK, cloud)
}

func (s *Server) writeManagedCloudResponse(c *gin.Context, status int, cloud store.ManagedBrandCloud) {
	if cloud.Operational {
		capabilities, err := s.developerCapabilitiesForUser(c.Request.Context(), currentUserID(c), cloud.ID, cloud.MyRole)
		if err != nil {
			writeStoreError(c, err)
			return
		}
		cloud.Capabilities = capabilities
	}
	c.JSON(status, gin.H{"brand_cloud": cloud})
}
