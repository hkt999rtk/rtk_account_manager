package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAppCertificateCreateRotatesActiveCertificate(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()

	registered, err := env.store.Register(ctx, RegisterInput{
		Email:            "app-cert-store@example.com",
		PasswordHash:     "hash",
		OrganizationName: "App Cert Store Org",
	})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	first, err := env.store.CreateAppCertificate(ctx, AppCertificateCreateInput{
		UserID:              registered.User.ID,
		Subject:             "app-user:" + registered.User.ID,
		CSRSHA256:           "csr-1",
		CertificatePEM:      "cert-1",
		CertificateChainPEM: "chain-1",
		FingerprintSHA256:   "fingerprint-1",
		SerialNumber:        "1",
		IssuerRequestID:     "issuer-1",
		NotBefore:           now.Add(-time.Minute),
		NotAfter:            now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	active, err := env.store.GetValidAppCertificateForUser(ctx, registered.User.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if active.ID != first.ID || active.FingerprintSHA256 != "fingerprint-1" {
		t.Fatalf("unexpected first active certificate: %+v", active)
	}
	issued, err := env.store.GetAppCertificateByIssuerRequestID(ctx, "issuer-1")
	if err != nil || issued.ID != first.ID {
		t.Fatalf("certificate issuer lookup = %+v, %v", issued, err)
	}
	if err := env.store.AuthorizeActiveAppCertificateForSubject(ctx, "user", registered.User.ID, "FINGERPRINT-1", now); err != nil {
		t.Fatalf("authorize first active certificate: %v", err)
	}

	second, err := env.store.CreateAppCertificate(ctx, AppCertificateCreateInput{
		UserID:              registered.User.ID,
		Subject:             "app-user:" + registered.User.ID,
		CSRSHA256:           "csr-2",
		CertificatePEM:      "cert-2",
		CertificateChainPEM: "chain-2",
		FingerprintSHA256:   "fingerprint-2",
		SerialNumber:        "2",
		IssuerRequestID:     "issuer-2",
		NotBefore:           now.Add(-time.Minute),
		NotAfter:            now.Add(2 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	active, err = env.store.GetValidAppCertificateForUser(ctx, registered.User.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if active.ID != second.ID || active.FingerprintSHA256 != "fingerprint-2" {
		t.Fatalf("unexpected rotated active certificate: %+v", active)
	}
	if err := env.store.AuthorizeActiveAppCertificateForSubject(ctx, "user", registered.User.ID, "fingerprint-1", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("authorize revoked certificate error = %v, want ErrNotFound", err)
	}
	if err := env.store.AuthorizeActiveAppCertificateForSubject(ctx, "user", registered.User.ID, "fingerprint-2", now); err != nil {
		t.Fatalf("authorize rotated active certificate: %v", err)
	}

	var revokedCount int
	if err := env.db.QueryRow(ctx, `
		SELECT count(*)::int
		FROM app_certificates
		WHERE id = $1 AND revoked_at IS NOT NULL
	`, first.ID).Scan(&revokedCount); err != nil {
		t.Fatal(err)
	}
	if revokedCount != 1 {
		t.Fatalf("expected first certificate to be revoked, count=%d", revokedCount)
	}
}
