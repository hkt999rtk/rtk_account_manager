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
	SubjectType         string
	SubjectID           string
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
	return s.GetValidAppCertificateForSubject(ctx, "platform_user", userID, now)
}

func (s *Store) GetValidAppCertificateForSubject(ctx context.Context, subjectType, subjectID string, now time.Time) (model.AppCertificate, error) {
	row := s.db.QueryRow(ctx, `
		SELECT id::text, COALESCE(user_id::text, ''), subject_type, subject_id, subject, csr_sha256, certificate_pem, certificate_chain_pem,
		       fingerprint_sha256, serial_number, issuer_request_id, not_before, not_after,
		       revoked_at, created_at, updated_at
		FROM app_certificates
		WHERE subject_type = $1
		  AND subject_id = $2
		  AND revoked_at IS NULL
		  AND not_before <= $3
		  AND not_after > $3
		ORDER BY not_after DESC
		LIMIT 1
	`, subjectType, subjectID, now)
	return scanAppCertificate(row)
}

func (s *Store) CreateAppCertificate(ctx context.Context, in AppCertificateCreateInput) (model.AppCertificate, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.AppCertificate{}, err
	}
	defer tx.Rollback(ctx)

	subjectType := in.SubjectType
	subjectID := in.SubjectID
	if subjectType == "" {
		subjectType = "platform_user"
	}
	if subjectID == "" {
		subjectID = in.UserID
	}
	if _, err := tx.Exec(ctx, `
		UPDATE app_certificates
		SET revoked_at = COALESCE(revoked_at, now()),
		    updated_at = now()
		WHERE subject_type = $1
		  AND subject_id = $2
		  AND revoked_at IS NULL
	`, subjectType, subjectID); err != nil {
		return model.AppCertificate{}, err
	}

	row := tx.QueryRow(ctx, `
		INSERT INTO app_certificates (
			user_id, subject_type, subject_id, subject, csr_sha256, certificate_pem, certificate_chain_pem,
			fingerprint_sha256, serial_number, issuer_request_id, not_before, not_after
		)
		VALUES (NULLIF($1, '')::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING id::text, COALESCE(user_id::text, ''), subject_type, subject_id, subject, csr_sha256, certificate_pem, certificate_chain_pem,
		          fingerprint_sha256, serial_number, issuer_request_id, not_before, not_after,
		          revoked_at, created_at, updated_at
	`, in.UserID, subjectType, subjectID, in.Subject, in.CSRSHA256, in.CertificatePEM, in.CertificateChainPEM,
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

func (s *Store) RevokeValidAppCertificatesForBrandCloudUser(ctx context.Context, brandCloudID, brandCloudUserID string) (int64, error) {
	tag, err := s.db.Exec(ctx, `
		UPDATE app_certificates ac
		SET revoked_at = COALESCE(ac.revoked_at, now()),
		    updated_at = now()
		FROM brand_cloud_users bcu
		LEFT JOIN users u ON u.email = bcu.email
		WHERE bcu.id = $1
		  AND bcu.brand_cloud_id = $2
		  AND (
		    (ac.subject_type = 'brand_cloud_user' AND ac.subject_id = bcu.id::text)
		    OR (ac.subject_type = 'platform_user' AND u.id IS NOT NULL AND ac.subject_id = u.id::text)
		  )
		  AND ac.revoked_at IS NULL
		  AND ac.not_before <= now()
		  AND ac.not_after > now()
	`, brandCloudUserID, brandCloudID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

type appCertificateRow interface {
	Scan(dest ...any) error
}

func scanAppCertificate(row appCertificateRow) (model.AppCertificate, error) {
	var cert model.AppCertificate
	err := row.Scan(
		&cert.ID,
		&cert.UserID,
		&cert.SubjectType,
		&cert.SubjectID,
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
