package api

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

const maxDeviceBindJobItems = 5000

type deviceBindJobRequest struct {
	Items []deviceBindJobItemRequest `json:"items" binding:"required"`
}

type deviceBindJobItemRequest struct {
	DeviceName      string   `json:"device_name" binding:"required"`
	Category        string   `json:"category" binding:"required"`
	VideoCloudDevid string   `json:"video_cloud_devid" binding:"required"`
	ActivityID      string   `json:"activity_id" binding:"required"`
	ClipPublicKey   string   `json:"clip_public_key" binding:"required"`
	ServiceOptions  []string `json:"service_options" binding:"required"`
}

type deviceBindJobBody struct {
	Job     deviceBindJobSummary  `json:"job"`
	Results []deviceBindJobResult `json:"results"`
}

type deviceBindJobSummary struct {
	Status    string `json:"status"`
	Requested int    `json:"requested"`
	Created   int    `json:"created"`
	Existing  int    `json:"existing"`
	Failed    int    `json:"failed"`
}

type deviceBindJobResult struct {
	VideoCloudDevid string                     `json:"video_cloud_devid"`
	Status          string                     `json:"status"`
	AccountDeviceID string                     `json:"account_device_id,omitempty"`
	Device          *model.Device              `json:"device,omitempty"`
	ProvisionInput  store.DeviceProvisionInput `json:"provision_input,omitempty"`
	Error           *deviceBindJobItemError    `json:"error"`
}

type deviceBindJobItemError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (s *Server) createDeviceBindJob(c *gin.Context) {
	var req deviceBindJobRequest
	if !bindStrict(c, &req) {
		return
	}
	if len(req.Items) == 0 {
		writeError(c, http.StatusBadRequest, "invalid_request", "items must include at least one device")
		return
	}
	if len(req.Items) > maxDeviceBindJobItems {
		writeError(c, http.StatusBadRequest, "invalid_request", "items exceeds maximum of 5000")
		return
	}

	inputs := make([]store.BulkBindDeviceInput, 0, len(req.Items))
	for _, item := range req.Items {
		category, ok := parseDeviceBindJobCategory(c, item.Category)
		if !ok {
			return
		}
		inputs = append(inputs, store.BulkBindDeviceInput{
			DeviceName:      strings.TrimSpace(item.DeviceName),
			Category:        category,
			VideoCloudDevid: strings.TrimSpace(item.VideoCloudDevid),
			ActivityID:      strings.TrimSpace(item.ActivityID),
			ClipPublicKey:   strings.TrimSpace(item.ClipPublicKey),
			ServiceOptions:  item.ServiceOptions,
		})
	}

	result, err := s.store.BulkBindDevices(c.Request.Context(), c.Param("brandCloudId"), inputs)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	_ = s.store.CreateAuditEvent(c.Request.Context(), store.AuditEventInput{
		EventType:      "device_bind_job_completed",
		ActorUserID:    stringPtr(currentUserID(c)),
		OrganizationID: stringPtr(c.Param("brandCloudId")),
		SubjectType:    "brand_cloud",
		SubjectID:      c.Param("brandCloudId"),
		Payload: map[string]any{
			"requested": result.Requested,
			"created":   result.Created,
			"existing":  result.Existing,
			"failed":    result.Failed,
		},
	})
	c.JSON(http.StatusOK, deviceBindJobResponse(result))
}

func parseDeviceBindJobCategory(c *gin.Context, raw string) (model.DeviceCategory, bool) {
	switch model.DeviceCategory(strings.TrimSpace(raw)) {
	case model.DeviceCategoryIPCamera:
		return model.DeviceCategoryIPCamera, true
	case model.DeviceCategoryMQTT:
		return model.DeviceCategoryMQTT, true
	case model.DeviceCategoryGeneric:
		return model.DeviceCategoryGeneric, true
	default:
		writeError(c, http.StatusBadRequest, "invalid_request", "unsupported device category")
		return "", false
	}
}

func deviceBindJobResponse(result store.BulkBindDevicesResult) deviceBindJobBody {
	body := deviceBindJobBody{
		Job: deviceBindJobSummary{
			Status:    result.Status,
			Requested: result.Requested,
			Created:   result.Created,
			Existing:  result.Existing,
			Failed:    result.Failed,
		},
		Results: make([]deviceBindJobResult, 0, len(result.Results)),
	}
	for _, item := range result.Results {
		out := deviceBindJobResult{
			VideoCloudDevid: item.VideoCloudDevid,
			Status:          string(item.Status),
			AccountDeviceID: item.Device.ID,
			ProvisionInput:  item.ProvisionInput,
		}
		if item.Device.ID != "" {
			device := item.Device
			out.Device = &device
		}
		if item.Status == store.BulkBindDeviceStatusFailed {
			out.Error = &deviceBindJobItemError{Code: item.ErrorCode, Message: item.ErrorMessage}
		}
		body.Results = append(body.Results, out)
	}
	return body
}
