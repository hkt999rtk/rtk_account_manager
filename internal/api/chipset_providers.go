package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

const (
	permissionChipsetProviderRead    = "platform.chipset_sdk.read"
	permissionChipsetProviderEdit    = "platform.chipset_sdk.edit"
	permissionChipsetProviderPublish = "platform.chipset_sdk.publish"
)

type chipsetProviderWriteRequest struct {
	Name        string `json:"name" binding:"required"`
	ManifestURL string `json:"manifest_url" binding:"required"`
}

func (s *Server) ConfigureChipsetManifestFetcher(fetcher ChipsetManifestFetcher) {
	s.chipsetManifestFetcher = fetcher
}

func (s *Server) listChipsetProviders(c *gin.Context) {
	providers, err := s.store.ListChipsetProviders(c.Request.Context())
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"providers": providers})
}

func (s *Server) getChipsetProvider(c *gin.Context) {
	provider, chipsets, err := s.store.GetChipsetProvider(c.Request.Context(), c.Param("providerId"))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"provider": provider, "chipsets": chipsets})
}

func (s *Server) createChipsetProvider(c *gin.Context) {
	if !requireIdempotencyKey(c) {
		return
	}
	var req chipsetProviderWriteRequest
	if !bind(c, &req) {
		return
	}
	if err := s.validateChipsetProviderWrite(req); err != nil {
		writeProviderError(c, err)
		return
	}
	provider, err := s.store.CreateChipsetProvider(c.Request.Context(), store.ChipsetProviderWriteInput{Name: req.Name, ManifestURL: req.ManifestURL, ActorUserID: currentUserID(c)})
	if err != nil {
		writeStoreError(c, err)
		return
	}
	auditResult := s.auditChipsetProvider(c.Request.Context(), currentUserID(c), provider.ID, "chipset_provider.created", "accepted", "", c.GetHeader("X-Request-Id"), c.GetHeader("Idempotency-Key"))
	c.JSON(http.StatusCreated, gin.H{"provider": provider, "chipsets": []model.DeveloperChipset{}, "audit_result": auditResult})
}

func (s *Server) updateChipsetProvider(c *gin.Context) {
	if !requireIdempotencyKey(c) {
		return
	}
	var req chipsetProviderWriteRequest
	if !bind(c, &req) {
		return
	}
	if err := s.validateChipsetProviderWrite(req); err != nil {
		writeProviderError(c, err)
		return
	}
	provider, err := s.store.UpdateChipsetProvider(c.Request.Context(), c.Param("providerId"), store.ChipsetProviderWriteInput{Name: req.Name, ManifestURL: req.ManifestURL, ActorUserID: currentUserID(c)})
	if err != nil {
		writeStoreError(c, err)
		return
	}
	auditResult := s.auditChipsetProvider(c.Request.Context(), currentUserID(c), provider.ID, "chipset_provider.updated", "accepted", "", c.GetHeader("X-Request-Id"), c.GetHeader("Idempotency-Key"))
	c.JSON(http.StatusOK, gin.H{"provider": provider, "audit_result": auditResult})
}

func (s *Server) actOnChipsetProvider(c *gin.Context) {
	if !requireIdempotencyKey(c) {
		return
	}
	providerID := c.Param("providerId")
	action := c.Param("action")
	switch action {
	case "publish":
		provider, _, err := s.refreshChipsetProvider(c.Request.Context(), providerID, currentUserID(c), c.GetHeader("X-Request-Id"), c.GetHeader("Idempotency-Key"))
		if err != nil {
			writeProviderError(c, err)
			return
		}
		provider, err = s.store.SetChipsetProviderStatus(c.Request.Context(), providerID, model.ChipsetProviderStatusPublished)
		if err != nil {
			writeStoreError(c, err)
			return
		}
		auditResult := s.auditChipsetProvider(c.Request.Context(), currentUserID(c), providerID, "chipset_provider.published", "accepted", "", c.GetHeader("X-Request-Id"), c.GetHeader("Idempotency-Key"))
		c.JSON(http.StatusOK, gin.H{"provider": provider, "audit_result": auditResult})
	case "unpublish":
		provider, err := s.store.SetChipsetProviderStatus(c.Request.Context(), providerID, model.ChipsetProviderStatusUnpublished)
		if err != nil {
			writeStoreError(c, err)
			return
		}
		auditResult := s.auditChipsetProvider(c.Request.Context(), currentUserID(c), providerID, "chipset_provider.unpublished", "accepted", "", c.GetHeader("X-Request-Id"), c.GetHeader("Idempotency-Key"))
		c.JSON(http.StatusOK, gin.H{"provider": provider, "audit_result": auditResult})
	case "refresh":
		provider, auditResult, err := s.refreshChipsetProvider(c.Request.Context(), providerID, currentUserID(c), c.GetHeader("X-Request-Id"), c.GetHeader("Idempotency-Key"))
		if err != nil {
			writeProviderError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"provider": provider, "audit_result": auditResult})
	default:
		writeError(c, http.StatusBadRequest, "invalid_action", "Action must be publish, unpublish, or refresh")
	}
}

func (s *Server) listDeveloperChipsets(c *gin.Context) {
	if currentSubjectType(c) != auth.SubjectTypePlatformUser {
		writeError(c, http.StatusForbidden, "forbidden", "Developer session required")
		return
	}
	chipsets, err := s.store.ListPublishedChipsets(c.Request.Context())
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"chipsets": chipsets})
}

func (s *Server) getDeveloperChipset(c *gin.Context) {
	if currentSubjectType(c) != auth.SubjectTypePlatformUser {
		writeError(c, http.StatusForbidden, "forbidden", "Developer session required")
		return
	}
	chipsets, err := s.store.ListPublishedChipsets(c.Request.Context())
	if err != nil {
		writeStoreError(c, err)
		return
	}
	for _, chipset := range chipsets {
		if chipset.ID == c.Param("chipsetId") {
			c.JSON(http.StatusOK, gin.H{"chipset": chipset})
			return
		}
	}
	writeStoreError(c, store.ErrNotFound)
}

func (s *Server) validateChipsetProviderWrite(req chipsetProviderWriteRequest) error {
	if strings.TrimSpace(req.Name) == "" || len(strings.TrimSpace(req.Name)) > 200 || s.chipsetManifestFetcher == nil {
		return errChipsetProviderURLInvalid
	}
	return s.chipsetManifestFetcher.ValidateURL(req.ManifestURL)
}

func (s *Server) refreshChipsetProvider(ctx context.Context, providerID, actorUserID, requestID, idempotencyKey string) (model.ChipsetProvider, string, error) {
	provider, _, err := s.store.GetChipsetProvider(ctx, providerID)
	if err != nil {
		return model.ChipsetProvider{}, "failed", err
	}
	if s.chipsetManifestFetcher == nil {
		return model.ChipsetProvider{}, "failed", errChipsetProviderHostNotAllowed
	}
	attemptedAt := time.Now().UTC()
	result, err := s.chipsetManifestFetcher.Fetch(ctx, provider)
	if err != nil {
		_, _ = s.store.MarkChipsetProviderRefreshFailed(ctx, providerID, sanitizedProviderError(err), attemptedAt)
		auditResult := s.auditChipsetProvider(ctx, actorUserID, providerID, "chipset_provider.refresh_failed", "failed", sanitizedProviderError(err), requestID, idempotencyKey)
		return model.ChipsetProvider{}, auditResult, err
	}
	if result.NotModified {
		if provider.LastSuccessfulRefreshAt == nil {
			_, _ = s.store.MarkChipsetProviderRefreshFailed(ctx, providerID, sanitizedProviderError(errChipsetProviderSnapshotRequired), attemptedAt)
			auditResult := s.auditChipsetProvider(ctx, actorUserID, providerID, "chipset_provider.refresh_failed", "failed", sanitizedProviderError(errChipsetProviderSnapshotRequired), requestID, idempotencyKey)
			return model.ChipsetProvider{}, auditResult, errChipsetProviderSnapshotRequired
		}
		provider, err = s.store.MarkChipsetProviderNotModified(ctx, providerID, attemptedAt)
	} else {
		provider, err = s.store.CommitChipsetProviderRefresh(ctx, store.ChipsetProviderRefreshInput{ProviderID: providerID, ManifestVersion: result.ManifestVersion, ManifestSHA256: result.ManifestSHA256, ETag: result.ETag, LastModified: result.LastModified, Chipsets: result.Chipsets, AttemptedAt: attemptedAt})
	}
	if err != nil {
		return model.ChipsetProvider{}, "failed", err
	}
	auditResult := s.auditChipsetProvider(ctx, actorUserID, providerID, "chipset_provider.refreshed", "accepted", "", requestID, idempotencyKey)
	return provider, auditResult, nil
}

func (s *Server) auditChipsetProvider(ctx context.Context, actorUserID, providerID, eventType, result, message, requestID, idempotencyKey string) string {
	var actor *string
	if strings.TrimSpace(actorUserID) != "" {
		actor = &actorUserID
	}
	if err := s.store.CreateAuditEvent(ctx, store.AuditEventInput{EventType: eventType, ActorUserID: actor, SubjectType: "chipset_information_provider", SubjectID: providerID, Payload: map[string]any{"result": result, "message": message, "request_id": strings.TrimSpace(requestID), "idempotency_key": strings.TrimSpace(idempotencyKey)}}); err != nil {
		return "failed"
	}
	return "accepted"
}

func (s *Server) RunChipsetProviderRefresh(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			providers, err := s.store.ListChipsetProviders(ctx)
			if err != nil {
				continue
			}
			for _, provider := range providers {
				if provider.Status == model.ChipsetProviderStatusPublished {
					_, _, _ = s.refreshChipsetProvider(ctx, provider.ID, "", "", "")
				}
			}
		}
	}
}

func requireIdempotencyKey(c *gin.Context) bool {
	if strings.TrimSpace(c.GetHeader("Idempotency-Key")) == "" {
		writeError(c, http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key header is required")
		return false
	}
	return true
}

func writeProviderError(c *gin.Context, err error) {
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrConflict) {
		writeStoreError(c, err)
		return
	}
	status := http.StatusBadRequest
	if errors.Is(err, errChipsetProviderSnapshotRequired) {
		status = http.StatusConflict
	}
	if !errors.Is(err, errChipsetProviderURLInvalid) && !errors.Is(err, errChipsetProviderHostNotAllowed) && !errors.Is(err, errChipsetProviderAddressNotPublic) && !errors.Is(err, errChipsetManifestInvalid) && !errors.Is(err, errChipsetManifestVersionUnsupported) {
		if !errors.Is(err, errChipsetProviderSnapshotRequired) {
			status = http.StatusBadGateway
		}
	}
	writeError(c, status, providerErrorCode(err), sanitizedProviderError(err))
}
