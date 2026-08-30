package api

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

type AppCertificateIssuer interface {
	IssueAppCertificate(context.Context, AppCertificateIssueRequest) (AppCertificateIssueResponse, error)
}

type AppCertificateIssueRequest struct {
	RequestID string
	UserID    string
	CSRPem    string
	TTLDays   *int
}

type AppCertificateIssueResponse struct {
	RequestID           string    `json:"request_id"`
	UserID              string    `json:"user_id"`
	Subject             string    `json:"subject"`
	SerialNumber        string    `json:"serial_number"`
	NotBefore           time.Time `json:"not_before"`
	NotAfter            time.Time `json:"not_after"`
	CertificatePEM      string    `json:"certificate_pem"`
	CertificateChainPEM string    `json:"certificate_chain_pem"`
	IssuedAt            time.Time `json:"issued_at"`
}

type appCertificateResponse struct {
	Status              string             `json:"status"`
	Subject             string             `json:"subject,omitempty"`
	CertificatePEM      string             `json:"certificate_pem,omitempty"`
	CertificateChainPEM string             `json:"certificate_chain_pem,omitempty"`
	FingerprintSHA256   string             `json:"fingerprint_sha256,omitempty"`
	SerialNumber        string             `json:"serial_number,omitempty"`
	IssuerRequestID     string             `json:"issuer_request_id,omitempty"`
	NotBefore           *time.Time         `json:"not_before,omitempty"`
	NotAfter            *time.Time         `json:"not_after,omitempty"`
	CertificateBundle   *certificateBundle `json:"certificate_bundle,omitempty"`
}

type loginResponse struct {
	User           model.User             `json:"user"`
	Tokens         tokenResponse          `json:"tokens"`
	AppCertificate appCertificateResponse `json:"app_certificate"`
}

var (
	errAppCertificateIssuerUnavailable = errors.New("app certificate issuer unavailable")
	errAppCertificateCSRInvalid        = errors.New("app certificate csr invalid")
)

func (s *Server) loginResponse(ctx context.Context, user model.User, tokens tokenResponse, csrPEM string) (loginResponse, error) {
	appCert, err := s.appCertificateForLogin(ctx, user.ID, csrPEM)
	if err != nil {
		return loginResponse{}, err
	}
	return loginResponse{
		User:           user,
		Tokens:         tokens,
		AppCertificate: appCert,
	}, nil
}

func (s *Server) appCertificateForLogin(ctx context.Context, userID, csrPEM string) (appCertificateResponse, error) {
	return s.appCertificateForSubject(ctx, "platform", "user", userID, "app-user:"+userID, csrPEM)
}

func (s *Server) appCertificateForBrandCloudLogin(ctx context.Context, brandCloudID, brandCloudUserID, csrPEM string) (appCertificateResponse, error) {
	return s.appCertificateForSubject(ctx, brandCloudID, "brand_cloud_user", brandCloudUserID, "app-brand-cloud-user:"+brandCloudUserID, csrPEM)
}

func (s *Server) appCertificateForEndUserLogin(ctx context.Context, endUserID, csrPEM string) (appCertificateResponse, error) {
	return s.appCertificateForSubject(ctx, "platform", "end_user", endUserID, "app-end-user:"+endUserID, csrPEM)
}

func (s *Server) appCertificateForSubject(ctx context.Context, tenantID, subjectType, subjectID, expectedSubject, csrPEM string) (appCertificateResponse, error) {
	now := s.now()
	existing, err := s.store.GetValidAppCertificateForSubject(ctx, subjectType, subjectID, now)
	if err == nil {
		return appCertificateFromModel(existing, "issued", tenantID)
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return appCertificateResponse{}, err
	}
	csrPEM = strings.TrimSpace(csrPEM)
	if csrPEM == "" {
		return appCertificateResponse{Status: "csr_required"}, nil
	}
	if s.appCertificateIssuer == nil {
		return appCertificateResponse{}, errAppCertificateIssuerUnavailable
	}
	return s.issueAppCertificateForSubject(ctx, tenantID, subjectType, subjectID, expectedSubject, csrPEM, nil, "")
}

func (s *Server) issueAppCertificateForSubject(ctx context.Context, tenantID, subjectType, subjectID, expectedSubject, csrPEM string, ttlDays *int, requestID string) (appCertificateResponse, error) {
	csrDER, err := validateAppCSRSubject(csrPEM, expectedSubject)
	if err != nil {
		return appCertificateResponse{}, err
	}
	if requestID == "" {
		requestID = "app-cert-" + subjectID + "-" + hashHexString(csrDER)[:16]
	}
	issuerResp, err := s.appCertificateIssuer.IssueAppCertificate(ctx, AppCertificateIssueRequest{
		RequestID: requestID,
		UserID:    subjectID,
		CSRPem:    csrPEM,
		TTLDays:   ttlDays,
	})
	if err != nil {
		return appCertificateResponse{}, err
	}
	leaf, fingerprint, err := certificateFingerprint(issuerResp.CertificatePEM)
	if err != nil {
		return appCertificateResponse{}, err
	}
	subject := strings.TrimSpace(issuerResp.Subject)
	if subject == "" {
		subject = expectedSubject
	}
	serial := strings.TrimSpace(issuerResp.SerialNumber)
	if serial == "" && leaf != nil && leaf.SerialNumber != nil {
		serial = leaf.SerialNumber.Text(16)
	}
	stored, err := s.store.CreateAppCertificate(ctx, store.AppCertificateCreateInput{
		UserID:              userIDForAppCertificate(subjectType, subjectID),
		SubjectType:         subjectType,
		SubjectID:           subjectID,
		Subject:             subject,
		CSRSHA256:           hashHexString(csrDER),
		CertificatePEM:      issuerResp.CertificatePEM,
		CertificateChainPEM: issuerResp.CertificateChainPEM,
		FingerprintSHA256:   fingerprint,
		SerialNumber:        serial,
		IssuerRequestID:     issuerResp.RequestID,
		NotBefore:           issuerResp.NotBefore,
		NotAfter:            issuerResp.NotAfter,
	})
	if err != nil {
		return appCertificateResponse{}, err
	}
	return appCertificateFromModel(stored, "issued", tenantID)
}

func userIDForAppCertificate(subjectType, subjectID string) string {
	if subjectType == "user" {
		return subjectID
	}
	return ""
}

func appCertificateFromModel(cert model.AppCertificate, status, tenantID string) (appCertificateResponse, error) {
	notBefore := cert.NotBefore
	notAfter := cert.NotAfter
	response := appCertificateResponse{
		Status:              status,
		Subject:             cert.Subject,
		CertificatePEM:      cert.CertificatePEM,
		CertificateChainPEM: cert.CertificateChainPEM,
		FingerprintSHA256:   cert.FingerprintSHA256,
		SerialNumber:        cert.SerialNumber,
		IssuerRequestID:     cert.IssuerRequestID,
		NotBefore:           &notBefore,
		NotAfter:            &notAfter,
	}
	bundle, err := newAppCertificateBundle(tenantID, cert.SubjectID, cert.IssuerRequestID, cert.CertificatePEM, cert.CertificateChainPEM, cert.CreatedAt)
	if err != nil {
		return appCertificateResponse{}, err
	}
	response.CertificateBundle = bundle
	return response, nil
}

func validateAppCSRSubject(csrPEM, expectedSubject string) ([]byte, error) {
	block, rest := pem.Decode([]byte(strings.TrimSpace(csrPEM)))
	if block == nil || block.Type != "CERTIFICATE REQUEST" || strings.TrimSpace(string(rest)) != "" {
		return nil, fmt.Errorf("%w: csr_pem must contain one certificate request", errAppCertificateCSRInvalid)
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errAppCertificateCSRInvalid, err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("%w: signature invalid", errAppCertificateCSRInvalid)
	}
	if !strings.EqualFold(strings.TrimSpace(csr.Subject.CommonName), expectedSubject) {
		return nil, fmt.Errorf("%w: csr common name must be %s", errAppCertificateCSRInvalid, expectedSubject)
	}
	return block.Bytes, nil
}

func certificateFingerprint(certPEM string) (*x509.Certificate, string, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(certPEM)))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, "", fmt.Errorf("%w: issued certificate is invalid", errAppCertificateCSRInvalid)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", errAppCertificateCSRInvalid, err)
	}
	return cert, hashHexString(cert.Raw), nil
}

func hashHexString(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func writeAppCertificateError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, errAppCertificateIssuerUnavailable):
		writeError(c, http.StatusServiceUnavailable, "app_certificate_issuer_unavailable", "App certificate issuer is unavailable")
	case errors.Is(err, errAppCertificateCSRInvalid):
		writeError(c, http.StatusBadRequest, "app_certificate_csr_invalid", "App certificate CSR is invalid")
	default:
		writeStoreError(c, err)
	}
}
