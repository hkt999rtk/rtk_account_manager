package api

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/mailer"
	"rtk_account_manager/internal/store"
)

const otpDigits = 6

func generateOTP() (string, error) {
	max := big.NewInt(1)
	for i := 0; i < otpDigits; i++ {
		max.Mul(max, big.NewInt(10))
	}
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%0*d", otpDigits, n.Int64()), nil
}

func (s *Server) issueEmailVerification(ctx context.Context, userID, email string, minResend time.Duration) error {
	code, err := generateOTP()
	if err != nil {
		return err
	}
	err = s.store.StartEmailVerification(ctx, store.EmailVerificationStartInput{
		UserID:            userID,
		CodeHash:          auth.HashToken(code),
		ExpiresAt:         time.Now().UTC().Add(s.authCodeOptions.EmailVerificationTTL),
		MinResendInterval: minResend,
	})
	if err != nil {
		return err
	}
	return s.mailer.Send(ctx, mailer.Message{
		Kind:      mailer.MessageKindEmailVerification,
		Recipient: email,
		Code:      code,
	})
}

func (s *Server) issuePasswordReset(ctx context.Context, userID, email string, minResend time.Duration) error {
	code, err := generateOTP()
	if err != nil {
		return err
	}
	err = s.store.StartPasswordReset(ctx, store.PasswordResetStartInput{
		UserID:            userID,
		CodeHash:          auth.HashToken(code),
		ExpiresAt:         time.Now().UTC().Add(s.authCodeOptions.PasswordResetTTL),
		MinResendInterval: minResend,
	})
	if err != nil {
		return err
	}
	return s.mailer.Send(ctx, mailer.Message{
		Kind:      mailer.MessageKindPasswordReset,
		Recipient: email,
		Code:      code,
	})
}

type verifyEmailRequest struct {
	Email string `json:"email" binding:"required,email"`
	Code  string `json:"code" binding:"required"`
}

func (s *Server) verifyEmail(c *gin.Context) {
	var req verifyEmailRequest
	if !bind(c, &req) {
		return
	}
	if !requireNonBlank(c, "code", req.Code) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	userID, err := s.store.GetUserIDByEmail(c.Request.Context(), email)
	if err != nil {
		writeError(c, http.StatusBadRequest, "invalid_verification", "Invalid email or code")
		return
	}
	user, err := s.store.ConsumeEmailVerification(c.Request.Context(), userID, auth.HashToken(strings.TrimSpace(req.Code)), s.authCodeOptions.OTPMaxAttempts)
	if err != nil {
		writeAuthCodeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"user": user})
}

type resendVerificationRequest struct {
	Email string `json:"email" binding:"required,email"`
}

func (s *Server) resendVerification(c *gin.Context) {
	var req resendVerificationRequest
	if !bind(c, &req) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	userID, err := s.store.GetUserIDByEmail(c.Request.Context(), email)
	if err != nil {
		// Do not leak whether the account exists.
		c.Status(http.StatusAccepted)
		return
	}
	if err := s.issueEmailVerification(c.Request.Context(), userID, email, s.authCodeOptions.OTPResendInterval); err != nil {
		switch {
		case errors.Is(err, store.ErrAlreadyVerified):
			writeError(c, http.StatusConflict, "already_verified", "Email is already verified")
		case errors.Is(err, store.ErrResendThrottled):
			writeError(c, http.StatusTooManyRequests, "resend_throttled", "Verification code was recently sent; try again later")
		default:
			writeError(c, http.StatusInternalServerError, "verification_send_failed", "Could not send verification email")
		}
		return
	}
	c.Status(http.StatusAccepted)
}

type forgotPasswordRequest struct {
	Email string `json:"email" binding:"required,email"`
}

func (s *Server) forgotPassword(c *gin.Context) {
	var req forgotPasswordRequest
	if !bind(c, &req) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	userID, err := s.store.GetUserIDByEmail(c.Request.Context(), email)
	if err != nil {
		// Do not leak whether the account exists.
		c.Status(http.StatusAccepted)
		return
	}
	if err := s.issuePasswordReset(c.Request.Context(), userID, email, s.authCodeOptions.OTPResendInterval); err != nil {
		if errors.Is(err, store.ErrResendThrottled) {
			// Treat throttle as silent success to avoid leaking timing info.
			c.Status(http.StatusAccepted)
			return
		}
		writeError(c, http.StatusInternalServerError, "reset_send_failed", "Could not send password reset")
		return
	}
	c.Status(http.StatusAccepted)
}

type resetPasswordRequest struct {
	Email       string `json:"email" binding:"required,email"`
	Code        string `json:"code" binding:"required"`
	NewPassword string `json:"new_password" binding:"required,min=8"`
}

func (s *Server) resetPassword(c *gin.Context) {
	var req resetPasswordRequest
	if !bind(c, &req) {
		return
	}
	if !requireNonBlank(c, "code", req.Code) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	userID, err := s.store.GetUserIDByEmail(c.Request.Context(), email)
	if err != nil {
		writeError(c, http.StatusBadRequest, "invalid_reset", "Invalid email or code")
		return
	}
	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "password_hash_failed", "Could not hash password")
		return
	}
	if err := s.store.ConsumePasswordReset(c.Request.Context(), userID, auth.HashToken(strings.TrimSpace(req.Code)), hash, s.authCodeOptions.OTPMaxAttempts); err != nil {
		writeAuthCodeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func writeAuthCodeError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrCodeMismatch):
		writeError(c, http.StatusBadRequest, "invalid_code", "Verification code is incorrect")
	case errors.Is(err, store.ErrCodeExpired):
		writeError(c, http.StatusBadRequest, "code_expired", "Verification code has expired")
	case errors.Is(err, store.ErrTooManyAttempts):
		writeError(c, http.StatusTooManyRequests, "too_many_attempts", "Too many verification attempts; request a new code")
	case errors.Is(err, store.ErrNotFound):
		writeError(c, http.StatusBadRequest, "invalid_code", "Verification code is incorrect")
	default:
		writeError(c, http.StatusInternalServerError, "internal_error", "Internal server error")
	}
}
