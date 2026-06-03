package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"rtk_account_manager/internal/model"
)

type AppCertificateCreateInput struct {
	UserID              string
	Subject             string
	CSRSHA256           string
	CertificatePEM      string
	CertificateChainPEM string
	FingerprintSHA256   string
	SerialNumber        string
	IssuerRequestID     string
	NotBefore           time.Time
	NotAfter            time.Time
}

func (s *Store) GetValidAppCertificateForUser(ctx context.Context, userID string, now time.Time) (model.AppCertificate, error) {
	row := s.db.QueryRow(ctx, `
		SELECT id::text, user_id::text, subject, csr_sha256, certificate_pem, certificate_chain_pem,
		       fingerprint_sha256, serial_number, issuer_request_id, not_before, not_after,
		       revoked_at, created_at, updated_at
		FROM app_certificates
		WHERE user_id = $1
		  AND revoked_at IS NULL
		  AND not_before <= $2
		  AND not_after > $2
		ORDER BY not_after DESC
		LIMIT 1
	`, userID, now)
	return scanAppCertificate(row)
}

func (s *Store) CreateAppCertificate(ctx context.Context, in AppCertificateCreateInput) (model.AppCertificate, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.AppCertificate{}, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
		UPDATE app_certificates
		SET revoked_at = COALESCE(revoked_at, now()),
		    updated_at = now()
		WHERE user_id = $1
		  AND revoked_at IS NULL
	`, in.UserID); err != nil {
		return model.AppCertificate{}, err
	}

	row := tx.QueryRow(ctx, `
		INSERT INTO app_certificates (
			user_id, subject, csr_sha256, certificate_pem, certificate_chain_pem,
			fingerprint_sha256, serial_number, issuer_request_id, not_before, not_after
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id::text, user_id::text, subject, csr_sha256, certificate_pem, certificate_chain_pem,
		          fingerprint_sha256, serial_number, issuer_request_id, not_before, not_after,
		          revoked_at, created_at, updated_at
	`, in.UserID, in.Subject, in.CSRSHA256, in.CertificatePEM, in.CertificateChainPEM,
		in.FingerprintSHA256, in.SerialNumber, in.IssuerRequestID, in.NotBefore, in.NotAfter)
	cert, err := scanAppCertificate(row)
	if err != nil {
		return model.AppCertificate{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.AppCertificate{}, err
	}
	return cert, nil
}

type appCertificateRow interface {
	Scan(dest ...any) error
}

func scanAppCertificate(row appCertificateRow) (model.AppCertificate, error) {
	var cert model.AppCertificate
	err := row.Scan(
		&cert.ID,
		&cert.UserID,
		&cert.Subject,
		&cert.CSRSHA256,
		&cert.CertificatePEM,
		&cert.CertificateChainPEM,
		&cert.FingerprintSHA256,
		&cert.SerialNumber,
		&cert.IssuerRequestID,
		&cert.NotBefore,
		&cert.NotAfter,
		&cert.RevokedAt,
		&cert.CreatedAt,
		&cert.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.AppCertificate{}, ErrNotFound
		}
		return model.AppCertificate{}, err
	}
	return cert, nil
}
