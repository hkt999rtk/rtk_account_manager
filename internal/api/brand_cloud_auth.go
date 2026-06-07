package api

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/auth"
)

func (s *Server) brandCloudLogin(c *gin.Context) {
	var req loginRequest
	if !bind(c, &req) {
		return
	}
	result, err := s.store.GetBrandCloudUserPassword(c.Request.Context(), c.Param("tenantSlug"), strings.ToLower(strings.TrimSpace(req.Email)))
	if err != nil || !auth.CheckPassword(result.PasswordHash, req.Password) || result.BrandCloudUser.SignupPendingVerification {
		writeError(c, http.StatusUnauthorized, "invalid_credentials", "Invalid email or password")
		return
	}
	tokens, err := s.issueBrandCloudTokens(c, result.BrandCloudUser.ID, result.BrandCloud.ID, valueOrEmpty(result.BrandCloud.TenantSlug))
	if err != nil {
		writeError(c, http.StatusInternalServerError, "token_issue_failed", "Could not issue tokens")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"brand_cloud":        result.BrandCloud,
		"brand_cloud_user":   result.BrandCloudUser,
		"brand_cloud_member": result.Member,
		"tokens":             tokens,
	})
}

func (s *Server) brandCloudRefresh(c *gin.Context) {
	var req refreshRequest
	if !bind(c, &req) {
		return
	}
	if !requireNonBlank(c, "refresh_token", req.RefreshToken) {
		return
	}
	claims, err := s.auth.ParseRefreshToken(req.RefreshToken)
	if err != nil || claims.SubjectType != auth.SubjectTypeBrandCloudUser || claims.BrandCloudUserID == "" || claims.BrandCloudID == "" {
		writeError(c, http.StatusUnauthorized, "invalid_refresh_token", "Invalid refresh token")
		return
	}
	if claims.TenantSlug != strings.TrimSpace(c.Param("tenantSlug")) {
		writeError(c, http.StatusUnauthorized, "invalid_refresh_token", "Invalid refresh token")
		return
	}
	accessToken, accessExpiresAt, err := s.auth.IssueBrandCloudAccessToken(claims.BrandCloudUserID, claims.BrandCloudID, claims.TenantSlug)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "token_issue_failed", "Could not issue tokens")
		return
	}
	refreshToken, refreshExpiresAt, err := s.auth.IssueBrandCloudRefreshToken(claims.BrandCloudUserID, claims.BrandCloudID, claims.TenantSlug)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "token_issue_failed", "Could not issue tokens")
		return
	}
	if err := s.store.RotateBrandCloudRefreshToken(c.Request.Context(), auth.HashToken(req.RefreshToken), auth.HashToken(refreshToken), claims.BrandCloudUserID, claims.BrandCloudID, refreshExpiresAt); err != nil {
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

func (s *Server) brandCloudLogout(c *gin.Context) {
	if currentSubjectType(c) != auth.SubjectTypeBrandCloudUser || currentTenantSlug(c) != strings.TrimSpace(c.Param("tenantSlug")) {
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
	if err := s.store.RevokeBrandCloudRefreshToken(c.Request.Context(), auth.HashToken(req.RefreshToken)); err != nil {
		writeStoreError(c, err)
		return
	}
	c.Status(http.StatusOK)
}

func (s *Server) brandCloudMe(c *gin.Context) {
	if currentSubjectType(c) != auth.SubjectTypeBrandCloudUser || currentTenantSlug(c) != strings.TrimSpace(c.Param("tenantSlug")) {
		writeError(c, http.StatusNotFound, "not_found", "Resource not found")
		return
	}
	user, err := s.store.GetBrandCloudUser(c.Request.Context(), currentBrandCloudUserID(c))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	member, err := s.store.GetBrandCloudMember(c.Request.Context(), currentBrandCloudID(c), currentBrandCloudUserID(c))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	brandCloud, err := s.store.GetBrandCloudByTenantSlug(c.Request.Context(), currentTenantSlug(c))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"brand_cloud": brandCloud, "brand_cloud_user": user, "brand_cloud_member": member})
}

func (s *Server) issueBrandCloudTokens(c *gin.Context, brandCloudUserID, brandCloudID, tenantSlug string) (tokenResponse, error) {
	accessToken, accessExpiresAt, err := s.auth.IssueBrandCloudAccessToken(brandCloudUserID, brandCloudID, tenantSlug)
	if err != nil {
		return tokenResponse{}, err
	}
	refreshToken, refreshExpiresAt, err := s.auth.IssueBrandCloudRefreshToken(brandCloudUserID, brandCloudID, tenantSlug)
	if err != nil {
		return tokenResponse{}, err
	}
	if err := s.store.SaveBrandCloudRefreshToken(c.Request.Context(), brandCloudUserID, brandCloudID, auth.HashToken(refreshToken), refreshExpiresAt); err != nil {
		return tokenResponse{}, err
	}
	return tokenResponse{
		AccessToken:           accessToken,
		AccessTokenExpiresAt:  accessExpiresAt,
		RefreshToken:          refreshToken,
		RefreshTokenExpiresAt: refreshExpiresAt,
	}, nil
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
