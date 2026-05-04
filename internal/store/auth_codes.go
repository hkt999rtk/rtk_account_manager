package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"rtk_account_manager/internal/model"
)

var (
	ErrCodeMismatch = errors.New("verification code does not match")
	ErrCodeExpired  = errors.New("verification code expired")
	ErrTooManyAttempts = errors.New("too many verification attempts")
	ErrAlreadyVerified = errors.New("email already verified")
	ErrResendThrottled = errors.New("verification code recently sent")
)

type EmailVerificationStartInput struct {
	UserID            string
	CodeHash          string
	ExpiresAt         time.Time
	MinResendInterval time.Duration
}

func (s *Store) StartEmailVerification(ctx context.Context, in EmailVerificationStartInput) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var verifiedAt *time.Time
	err = tx.QueryRow(ctx, `
		SELECT email_verified_at FROM users WHERE id = $1 AND disabled_at IS NULL FOR UPDATE
	`, in.UserID).Scan(&verifiedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if verifiedAt != nil {
		return ErrAlreadyVerified
	}

	if in.MinResendInterval > 0 {
		var existingCreatedAt *time.Time
		err = tx.QueryRow(ctx, `
			SELECT created_at FROM email_verifications WHERE user_id = $1 FOR UPDATE
		`, in.UserID).Scan(&existingCreatedAt)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if existingCreatedAt != nil && time.Since(*existingCreatedAt) < in.MinResendInterval {
			return ErrResendThrottled
		}
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO email_verifications (user_id, code_hash, expires_at, attempts, created_at)
		VALUES ($1, $2, $3, 0, now())
		ON CONFLICT (user_id) DO UPDATE
		SET code_hash = EXCLUDED.code_hash,
		    expires_at = EXCLUDED.expires_at,
		    attempts = 0,
		    created_at = now()
	`, in.UserID, in.CodeHash, in.ExpiresAt); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

func (s *Store) ConsumeEmailVerification(ctx context.Context, userID, codeHash string, maxAttempts int) (model.User, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.User{}, err
	}
	defer tx.Rollback(ctx)

	var storedHash string
	var expiresAt time.Time
	var attempts int
	err = tx.QueryRow(ctx, `
		SELECT code_hash, expires_at, attempts
		FROM email_verifications
		WHERE user_id = $1
		FOR UPDATE
	`, userID).Scan(&storedHash, &expiresAt, &attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.User{}, ErrNotFound
	}
	if err != nil {
		return model.User{}, err
	}

	if time.Now().UTC().After(expiresAt) {
		if _, err := tx.Exec(ctx, `DELETE FROM email_verifications WHERE user_id = $1`, userID); err != nil {
			return model.User{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return model.User{}, err
		}
		return model.User{}, ErrCodeExpired
	}

	if storedHash != codeHash {
		newAttempts := attempts + 1
		if maxAttempts > 0 && newAttempts >= maxAttempts {
			if _, err := tx.Exec(ctx, `DELETE FROM email_verifications WHERE user_id = $1`, userID); err != nil {
				return model.User{}, err
			}
			if err := tx.Commit(ctx); err != nil {
				return model.User{}, err
			}
			return model.User{}, ErrTooManyAttempts
		}
		if _, err := tx.Exec(ctx, `
			UPDATE email_verifications SET attempts = attempts + 1 WHERE user_id = $1
		`, userID); err != nil {
			return model.User{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return model.User{}, err
		}
		return model.User{}, ErrCodeMismatch
	}

	var user model.User
	err = tx.QueryRow(ctx, `
		UPDATE users
		SET email_verified_at = now()
		WHERE id = $1 AND disabled_at IS NULL
		RETURNING id::text, email, display_name, created_at, updated_at, disabled_at, email_verified_at
	`, userID).Scan(&user.ID, &user.Email, &user.DisplayName, &user.CreatedAt, &user.UpdatedAt, &user.DisabledAt, &user.EmailVerifiedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.User{}, ErrNotFound
	}
	if err != nil {
		return model.User{}, err
	}

	if _, err := tx.Exec(ctx, `DELETE FROM email_verifications WHERE user_id = $1`, userID); err != nil {
		return model.User{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return model.User{}, err
	}
	return user, nil
}

type PasswordResetStartInput struct {
	UserID            string
	CodeHash          string
	ExpiresAt         time.Time
	MinResendInterval time.Duration
}

func (s *Store) StartPasswordReset(ctx context.Context, in PasswordResetStartInput) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var existing string
	err = tx.QueryRow(ctx, `
		SELECT id::text FROM users WHERE id = $1 AND disabled_at IS NULL FOR UPDATE
	`, in.UserID).Scan(&existing)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}

	if in.MinResendInterval > 0 {
		var existingCreatedAt *time.Time
		err = tx.QueryRow(ctx, `
			SELECT created_at FROM password_resets WHERE user_id = $1 FOR UPDATE
		`, in.UserID).Scan(&existingCreatedAt)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if existingCreatedAt != nil && time.Since(*existingCreatedAt) < in.MinResendInterval {
			return ErrResendThrottled
		}
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO password_resets (user_id, code_hash, expires_at, attempts, created_at)
		VALUES ($1, $2, $3, 0, now())
		ON CONFLICT (user_id) DO UPDATE
		SET code_hash = EXCLUDED.code_hash,
		    expires_at = EXCLUDED.expires_at,
		    attempts = 0,
		    created_at = now()
	`, in.UserID, in.CodeHash, in.ExpiresAt); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

func (s *Store) ConsumePasswordReset(ctx context.Context, userID, codeHash, newPasswordHash string, maxAttempts int) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var storedHash string
	var expiresAt time.Time
	var attempts int
	err = tx.QueryRow(ctx, `
		SELECT code_hash, expires_at, attempts
		FROM password_resets
		WHERE user_id = $1
		FOR UPDATE
	`, userID).Scan(&storedHash, &expiresAt, &attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}

	if time.Now().UTC().After(expiresAt) {
		if _, err := tx.Exec(ctx, `DELETE FROM password_resets WHERE user_id = $1`, userID); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		return ErrCodeExpired
	}

	if storedHash != codeHash {
		newAttempts := attempts + 1
		if maxAttempts > 0 && newAttempts >= maxAttempts {
			if _, err := tx.Exec(ctx, `DELETE FROM password_resets WHERE user_id = $1`, userID); err != nil {
				return err
			}
			if err := tx.Commit(ctx); err != nil {
				return err
			}
			return ErrTooManyAttempts
		}
		if _, err := tx.Exec(ctx, `
			UPDATE password_resets SET attempts = attempts + 1 WHERE user_id = $1
		`, userID); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		return ErrCodeMismatch
	}

	tag, err := tx.Exec(ctx, `
		UPDATE users
		SET password_hash = $2
		WHERE id = $1 AND disabled_at IS NULL
	`, userID, newPasswordHash)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}

	if _, err := tx.Exec(ctx, `DELETE FROM password_resets WHERE user_id = $1`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE refresh_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL
	`, userID); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

func (s *Store) GetUserIDByEmail(ctx context.Context, email string) (string, error) {
	var userID string
	err := s.db.QueryRow(ctx, `
		SELECT id::text FROM users WHERE email = $1 AND disabled_at IS NULL
	`, email).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return userID, err
}
