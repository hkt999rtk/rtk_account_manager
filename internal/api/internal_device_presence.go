package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/channel"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

// handleInternalDevicePresenceEvent accepts the same DeviceOnlineChanged
// envelope used by the video.account.events inbox. It is a direct-HTTP
// receiver, not a producer or a replacement for live owner reconciliation.
func (s *Server) handleInternalDevicePresenceEvent(c *gin.Context) {
	if !s.requireInternalAuthToken(c) {
		return
	}
	var envelope channel.Envelope
	if !bindStrict(c, &envelope) {
		return
	}
	if envelope.MessageType != channel.MessageTypeDeviceOnlineChanged {
		writeError(c, http.StatusBadRequest, "invalid_presence_event", "Only DeviceOnlineChanged is accepted")
		return
	}
	decoded, err := envelope.ValidateAndDecode(channel.StreamVideoAccountEvents)
	if err != nil {
		writeError(c, http.StatusBadRequest, "invalid_presence_event", err.Error())
		return
	}
	payload := decoded.(*channel.DeviceOnlineChangedPayload)
	now := time.Now().UTC()
	message, _, err := s.store.CreateOrGetInboxMessage(c.Request.Context(), store.DeviceMessageInboxCreateInput{
		MessageID:     envelope.MessageID,
		OperationID:   envelope.OperationID,
		CorrelationID: envelope.CorrelationID,
		CausationID:   trimPtr(&envelope.CausationID),
		Stream:        string(channel.StreamVideoAccountEvents),
		MessageType:   string(envelope.MessageType),
		SchemaVersion: envelope.SchemaVersion,
		PartitionKey:  envelope.PartitionKey,
		Payload: map[string]any{
			"org_id":            payload.OrgID,
			"account_device_id": payload.AccountDeviceID,
			"video_cloud_devid": payload.VideoCloudDevid,
			"status":            payload.Status,
			"last_seen_at":      payload.LastSeenAt,
		},
		Status:     model.DeviceMessageInboxStatusRetrying,
		ReceivedAt: now,
	})
	if err != nil {
		writeStoreError(c, err)
		return
	}
	if message.Status != model.DeviceMessageInboxStatusProcessed {
		projection := store.OnlineChangedProjection(*payload)
		if _, err := s.store.RecordInboxProcessTransition(c.Request.Context(), store.InboxProcessTransitionInput{
			MessageID:      envelope.MessageID,
			MessageStatus:  model.DeviceMessageInboxStatusProcessed,
			AttemptCount:   1,
			ProcessedAt:    &now,
			OrganizationID: payload.OrgID,
			DeviceID:       payload.AccountDeviceID,
			Projection:     &projection,
		}); err != nil {
			writeStoreError(c, err)
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "message_id": envelope.MessageID})
}
