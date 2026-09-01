package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"rtk_account_manager/internal/store"
)

func (s *Server) patchProductDeviceDisplay(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var req store.DeviceDisplayPatch
	// This display-only endpoint accepts neither duplicate keys nor explicit
	// nulls. Do not delegate to the legacy full-record decoder/update path.
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 16*1024))
	invalid := func() {
		writeError(c, http.StatusBadRequest, "invalid_request", "Expected unique name/model string fields")
	}
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		invalid()
		return
	}
	seen := map[string]bool{}
	for decoder.More() {
		key, err := decoder.Token()
		name, ok := key.(string)
		if err != nil || !ok || seen[name] || (name != "name" && name != "model") {
			invalid()
			return
		}
		seen[name] = true
		var value *string
		if err := decoder.Decode(&value); err != nil || value == nil {
			invalid()
			return
		}
		if name == "name" {
			req.Name = value
		} else {
			req.Model = value
		}
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') {
		invalid()
		return
	}
	if _, err = decoder.Token(); err != io.EOF {
		invalid()
		return
	}
	if (req.Name == nil && req.Model == nil) || (req.Name != nil && (strings.TrimSpace(*req.Name) == "" || utf8.RuneCountInString(strings.TrimSpace(*req.Name)) > 255)) || (req.Model != nil && utf8.RuneCountInString(strings.TrimSpace(*req.Model)) > 255) {
		writeError(c, http.StatusBadRequest, "invalid_display_fields", "Provide a nonblank name or model, up to 255 characters each")
		return
	}
	device, err := s.store.PatchProductDeviceDisplay(c.Request.Context(), currentUserID(c), c.Param("orgId"), c.Param("profileId"), c.Param("deviceId"), req)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"device": device})
}
