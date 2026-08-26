package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

const developerPKITestTTLDays = 30

type developerPKITestAppRequest struct {
	TargetType string `json:"target_type" binding:"required"`
	TargetID   string `json:"target_id" binding:"required"`
	CSRPEM     string `json:"csr_pem" binding:"required"`
}

type brandCloudEndUserLookup interface {
	GetBrandCloudEndUser(context.Context, string, string) (model.BrandCloudEndUser, error)
}

type appCertificateRequestLookup interface {
	GetAppCertificateByIssuerRequestID(context.Context, string) (model.AppCertificate, error)
}

func developerPKITestIssuanceEnabled() bool {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv("DEVELOPER_PKI_TEST_TOOLS_ENABLED")), "true") {
		return false
	}
	environment := strings.ToLower(strings.TrimSpace(os.Getenv("ACCOUNT_MANAGER_ENV")))
	if environment == "" {
		environment = "local"
	}
	return environment == "local" || environment == "development" || environment == "dev" || environment == "staging"
}

func (s *Server) issueDeveloperPKITestAppCertificate(c *gin.Context) {
	if !developerPKITestIssuanceEnabled() {
		writeError(c, http.StatusNotFound, "developer_pki_test_tools_disabled", "PKI test tools are unavailable")
		return
	}
	if !requireIdempotencyKey(c) {
		return
	}
	member, err := s.store.GetDeveloperBrandCloudMember(c.Request.Context(), c.Param("brandCloudId"), currentUserID(c))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	if member.Role != model.RoleOwner && member.Role != model.RoleAdmin {
		writeError(c, http.StatusForbidden, "developer_brand_cloud_membership_required", "Brand Cloud PKI test issuance requires owner or admin role")
		return
	}
	var req developerPKITestAppRequest
	if !bind(c, &req) {
		return
	}
	brandCloudID := c.Param("brandCloudId")
	req.TargetType = strings.TrimSpace(req.TargetType)
	req.TargetID = strings.TrimSpace(req.TargetID)
	var expectedSubject string
	switch req.TargetType {
	case "brand_cloud_user":
		user, err := s.store.GetBrandCloudUser(c.Request.Context(), req.TargetID)
		if err != nil || user.BrandCloudID != brandCloudID {
			writeStoreError(c, store.ErrNotFound)
			return
		}
		expectedSubject = "app-brand-cloud-user:" + req.TargetID
	case "end_user":
		lookup, ok := s.store.(brandCloudEndUserLookup)
		if !ok {
			writeError(c, http.StatusServiceUnavailable, "developer_pki_target_lookup_unavailable", "End-user target lookup is unavailable")
			return
		}
		if _, err := lookup.GetBrandCloudEndUser(c.Request.Context(), brandCloudID, req.TargetID); err != nil {
			writeStoreError(c, err)
			return
		}
		expectedSubject = "app-end-user:" + req.TargetID
	default:
		writeError(c, http.StatusBadRequest, "invalid_target_type", "target_type must be brand_cloud_user or end_user")
		return
	}
	csrDER, err := validateAppCSRSubject(req.CSRPEM, expectedSubject)
	if err != nil {
		writeAppCertificateError(c, err)
		return
	}
	idempotencyKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	requestDigest := sha256.Sum256([]byte(brandCloudID + "\x00" + currentUserID(c) + "\x00" + idempotencyKey))
	issuerRequestID := "pki-test-app-" + hex.EncodeToString(requestDigest[:16])
	if lookup, ok := s.store.(appCertificateRequestLookup); ok {
		if existing, lookupErr := lookup.GetAppCertificateByIssuerRequestID(c.Request.Context(), issuerRequestID); lookupErr == nil {
			if existing.SubjectType != req.TargetType || existing.SubjectID != req.TargetID || existing.CSRSHA256 != hashHexString(csrDER) {
				writeError(c, http.StatusConflict, "idempotency_key_conflict", "Idempotency-Key was already used for a different request")
				return
			}
			response, responseErr := appCertificateFromModel(existing, "issued", brandCloudID)
			if responseErr != nil {
				writeAppCertificateError(c, responseErr)
				return
			}
			c.Header("Cache-Control", "no-store")
			c.JSON(http.StatusOK, response)
			return
		}
	}
	ttl := developerPKITestTTLDays
	response, err := s.issueAppCertificateForSubject(c.Request.Context(), brandCloudID, req.TargetType, req.TargetID, expectedSubject, req.CSRPEM, &ttl, issuerRequestID)
	if err != nil {
		writeAppCertificateError(c, err)
		return
	}
	actor, organization := currentUserID(c), brandCloudID
	_ = s.store.CreateAuditEvent(c.Request.Context(), store.AuditEventInput{
		EventType: "pki.test.app_certificate_issued", ActorUserID: &actor, OrganizationID: &organization,
		SubjectType: req.TargetType, SubjectID: req.TargetID,
		Payload: map[string]any{"request_id": issuerRequestID, "idempotency_key": idempotencyKey, "csr_sha256": hashHexString(csrDER), "ttl_days": ttl},
	})
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusCreated, response)
}
