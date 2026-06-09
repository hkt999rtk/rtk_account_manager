package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

type endUserLoginRequest struct {
	Email       string  `json:"email" binding:"required,email"`
	Password    string  `json:"password" binding:"required,min=8"`
	DisplayName *string `json:"display_name"`
	AppCSRPem   string  `json:"app_csr_pem,omitempty"`
}

type endUserLoginResponse struct {
	EndUser        model.EndUser          `json:"end_user"`
	Tokens         tokenResponse          `json:"tokens"`
	AppCertificate appCertificateResponse `json:"app_certificate"`
}

type endUserClaimResolveResponse struct {
	ClaimID        string                     `json:"claim_id"`
	Device         model.Device               `json:"device"`
	BrandLink      model.BrandCloudEndUser    `json:"brand_cloud_end_user"`
	DeviceBinding  model.DeviceUserBinding    `json:"device_user_binding"`
	ProvisionInput store.DeviceProvisionInput `json:"provision_input"`
}

func (s *Server) appEndUserLogin(c *gin.Context) {
	var req endUserLoginRequest
	if !bind(c, &req) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	password := req.Password
	result, err := s.store.GetEndUserPassword(c.Request.Context(), email)
	if errors.Is(err, store.ErrNotFound) {
		hash, hashErr := auth.HashPassword(password)
		if hashErr != nil {
			writeError(c, http.StatusInternalServerError, "password_hash_failed", "Could not hash password")
			return
		}
		endUser, createErr := s.store.CreateEndUser(c.Request.Context(), store.EndUserCreateInput{
			Email:        email,
			PasswordHash: hash,
			DisplayName:  trimStringPtr(req.DisplayName),
		})
		if createErr != nil {
			writeStoreError(c, createErr)
			return
		}
		result = store.EndUserLoginResult{EndUser: endUser, PasswordHash: hash}
	} else if err != nil {
		writeStoreError(c, err)
		return
	}
	if !auth.CheckPassword(result.PasswordHash, password) {
		writeError(c, http.StatusUnauthorized, "invalid_credentials", "Invalid email or password")
		return
	}
	tokens, err := s.issueEndUserTokens(c, result.EndUser.ID)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "token_issue_failed", "Could not issue tokens")
		return
	}
	appCert, err := s.appCertificateForEndUserLogin(c.Request.Context(), result.EndUser.ID, req.AppCSRPem)
	if err != nil {
		writeAppCertificateError(c, err)
		return
	}
	c.JSON(http.StatusOK, endUserLoginResponse{EndUser: result.EndUser, Tokens: tokens, AppCertificate: appCert})
}

func (s *Server) appEndUserRefresh(c *gin.Context) {
	var req refreshRequest
	if !bind(c, &req) {
		return
	}
	if !requireNonBlank(c, "refresh_token", req.RefreshToken) {
		return
	}
	claims, err := s.auth.ParseRefreshToken(req.RefreshToken)
	if err != nil || claims.SubjectType != auth.SubjectTypeEndUser || claims.EndUserID == "" {
		writeError(c, http.StatusUnauthorized, "invalid_refresh_token", "Invalid refresh token")
		return
	}
	accessToken, accessExpiresAt, err := s.auth.IssueEndUserAccessToken(claims.EndUserID)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "token_issue_failed", "Could not issue tokens")
		return
	}
	refreshToken, refreshExpiresAt, err := s.auth.IssueEndUserRefreshToken(claims.EndUserID)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "token_issue_failed", "Could not issue tokens")
		return
	}
	if err := s.store.RotateEndUserRefreshToken(c.Request.Context(), auth.HashToken(req.RefreshToken), auth.HashToken(refreshToken), claims.EndUserID, refreshExpiresAt); err != nil {
		writeError(c, http.StatusUnauthorized, "invalid_refresh_token", "Invalid refresh token")
		return
	}
	c.JSON(http.StatusOK, gin.H{"tokens": tokenResponse{
		AccessToken:           accessToken,
		AccessTokenExpiresAt:  accessExpiresAt,
		RefreshToken:          refreshToken,
		RefreshTokenExpiresAt: refreshExpiresAt,
	}})
}

func (s *Server) appEndUserLogout(c *gin.Context) {
	if currentSubjectType(c) != auth.SubjectTypeEndUser {
		writeError(c, http.StatusNotFound, "not_found", "Resource not found")
		return
	}
	var req refreshRequest
	if !bind(c, &req) {
		return
	}
	if !requireNonBlank(c, "refresh_token", req.RefreshToken) {
		return
	}
	if err := s.store.RevokeEndUserRefreshToken(c.Request.Context(), auth.HashToken(req.RefreshToken)); err != nil {
		writeStoreError(c, err)
		return
	}
	c.Status(http.StatusOK)
}

func (s *Server) appEndUserMe(c *gin.Context) {
	if currentSubjectType(c) != auth.SubjectTypeEndUser {
		writeError(c, http.StatusNotFound, "not_found", "Resource not found")
		return
	}
	user, err := s.store.GetEndUser(c.Request.Context(), currentEndUserID(c))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"end_user": user})
}

func (s *Server) appEndUserResolveDeviceClaim(c *gin.Context) {
	if currentSubjectType(c) != auth.SubjectTypeEndUser {
		writeError(c, http.StatusNotFound, "not_found", "Resource not found")
		return
	}
	var req claimResolveRequest
	if !bindStrict(c, &req) {
		return
	}
	claimToken := strings.TrimSpace(req.ClaimToken)
	deviceName := strings.TrimSpace(req.DeviceName)
	if !requireNonBlank(c, "claim_token", claimToken) ||
		!requireNonBlank(c, "device_name", deviceName) {
		return
	}
	result, err := s.store.ResolveEndUserDeviceClaimToken(c.Request.Context(), store.EndUserDeviceClaimResolveInput{
		TokenHash:  auth.HashToken(claimToken),
		EndUserID:  currentEndUserID(c),
		DeviceName: deviceName,
		Now:        time.Now().UTC(),
	})
	if err != nil {
		writeClaimResolveError(c, err)
		return
	}
	c.JSON(http.StatusCreated, endUserClaimResolveResponse{
		ClaimID:        result.Claim.ID,
		Device:         result.Device,
		BrandLink:      result.BrandLink,
		DeviceBinding:  result.DeviceBinding,
		ProvisionInput: result.ProvisionInput,
	})
}

func (s *Server) issueEndUserTokens(c *gin.Context, endUserID string) (tokenResponse, error) {
	accessToken, accessExpiresAt, err := s.auth.IssueEndUserAccessToken(endUserID)
	if err != nil {
		return tokenResponse{}, err
	}
	refreshToken, refreshExpiresAt, err := s.auth.IssueEndUserRefreshToken(endUserID)
	if err != nil {
		return tokenResponse{}, err
	}
	if err := s.store.SaveEndUserRefreshToken(c.Request.Context(), endUserID, auth.HashToken(refreshToken), refreshExpiresAt); err != nil {
		return tokenResponse{}, err
	}
	return tokenResponse{
		AccessToken:           accessToken,
		AccessTokenExpiresAt:  accessExpiresAt,
		RefreshToken:          refreshToken,
		RefreshTokenExpiresAt: refreshExpiresAt,
	}, nil
}
