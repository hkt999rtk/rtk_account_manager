package auth

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

type TokenKind string

const (
	TokenKindAccess  TokenKind = "access"
	TokenKindRefresh TokenKind = "refresh"
)

type SubjectType string

const (
	SubjectTypeUser           SubjectType = "user"
	SubjectTypeBrandCloudUser SubjectType = "brand_cloud_user"
	SubjectTypeEndUser        SubjectType = "end_user"
	SubjectTypeDelegatedJob   SubjectType = "delegated_job"
)

type Claims struct {
	UserID               string      `json:"user_id,omitempty"`
	SubjectType          SubjectType `json:"subject_type"`
	BrandCloudUserID     string      `json:"brand_cloud_user_id,omitempty"`
	BrandCloudID         string      `json:"brand_cloud_id,omitempty"`
	TenantSlug           string      `json:"tenant_slug,omitempty"`
	EndUserID            string      `json:"end_user_id,omitempty"`
	JobAuthorizationID   string      `json:"job_authorization_id,omitempty"`
	JobID                string      `json:"job_id,omitempty"`
	ScopeHash            string      `json:"scope_hash,omitempty"`
	Capability           string      `json:"capability,omitempty"`
	ProductIDs           []string    `json:"product_ids,omitempty"`
	AuthorizationVersion int64       `json:"authorization_version,omitempty"`
	OwnershipVersion     int64       `json:"ownership_version,omitempty"`
	Kind                 TokenKind   `json:"kind"`
	jwt.RegisteredClaims
}

type Service struct {
	accessSecret  []byte
	refreshSecret []byte
	accessSigner  TokenSigner
	refreshSigner TokenSigner
	accessTTL     time.Duration
	refreshTTL    time.Duration
}

type TokenSigner interface {
	SignToken(signingString string) (string, error)
	Keyfunc(token *jwt.Token) (any, error)
	Alg() string
}

type RS256TokenSigner struct {
	Signer    crypto.Signer
	PublicKey *rsa.PublicKey
}

func NewService(accessSecret, refreshSecret string, accessTTL, refreshTTL time.Duration) *Service {
	return &Service{
		accessSecret:  []byte(accessSecret),
		refreshSecret: []byte(refreshSecret),
		accessTTL:     accessTTL,
		refreshTTL:    refreshTTL,
	}
}

func NewServiceWithSigners(accessSigner, refreshSigner TokenSigner, accessTTL, refreshTTL time.Duration) *Service {
	return &Service{
		accessSigner:  accessSigner,
		refreshSigner: refreshSigner,
		accessTTL:     accessTTL,
		refreshTTL:    refreshTTL,
	}
}

func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(hash), err
}

func CheckPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func RandomToken() (string, error) {
	var data [32]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(data[:]), nil
}

func (s *Service) IssueAccessToken(userID string) (string, time.Time, error) {
	return s.issueUser(userID, TokenKindAccess, s.accessSecret, s.accessSigner, s.accessTTL)
}

func (s *Service) IssueRefreshToken(userID string) (string, time.Time, error) {
	return s.issueUser(userID, TokenKindRefresh, s.refreshSecret, s.refreshSigner, s.refreshTTL)
}

func (s *Service) IssueEndUserAccessToken(endUserID string) (string, time.Time, error) {
	return s.issueEndUser(endUserID, TokenKindAccess, s.accessSecret, s.accessSigner, s.accessTTL)
}

func (s *Service) IssueEndUserRefreshToken(endUserID string) (string, time.Time, error) {
	return s.issueEndUser(endUserID, TokenKindRefresh, s.refreshSecret, s.refreshSigner, s.refreshTTL)
}

// IssueDelegatedJobAccessToken issues a non-refreshable, five-minute token for
// one persisted background-job grant. Callers must revalidate the grant before
// every issuance and every use.
func (s *Service) IssueDelegatedJobAccessToken(claims Claims) (string, time.Time, error) {
	claims.SubjectType = SubjectTypeDelegatedJob
	return s.issue(claims, "delegated_job:"+claims.JobAuthorizationID, TokenKindAccess, s.accessSecret, s.accessSigner, 5*time.Minute)
}

func (s *Service) ParseAccessToken(tokenString string) (*Claims, error) {
	return s.parse(tokenString, TokenKindAccess, s.accessSecret, s.accessSigner)
}

func (s *Service) ParseRefreshToken(tokenString string) (*Claims, error) {
	return s.parse(tokenString, TokenKindRefresh, s.refreshSecret, s.refreshSigner)
}

func (s *Service) issueUser(userID string, kind TokenKind, secret []byte, signer TokenSigner, ttl time.Duration) (string, time.Time, error) {
	return s.issue(Claims{
		UserID:      userID,
		SubjectType: SubjectTypeUser,
	}, userID, kind, secret, signer, ttl)
}

func (s *Service) issueEndUser(endUserID string, kind TokenKind, secret []byte, signer TokenSigner, ttl time.Duration) (string, time.Time, error) {
	return s.issue(Claims{
		SubjectType: SubjectTypeEndUser,
		EndUserID:   endUserID,
	}, "end_user:"+endUserID, kind, secret, signer, ttl)
}

func (s *Service) issue(claims Claims, subject string, kind TokenKind, secret []byte, signer TokenSigner, ttl time.Duration) (string, time.Time, error) {
	expiresAt := time.Now().UTC().Add(ttl)
	tokenID, err := randomTokenID()
	if err != nil {
		return "", time.Time{}, err
	}
	if claims.SubjectType == "" {
		claims.SubjectType = SubjectTypeUser
	}
	claims.Kind = kind
	claims.RegisteredClaims = jwt.RegisteredClaims{
		Subject:   subject,
		ExpiresAt: jwt.NewNumericDate(expiresAt),
		IssuedAt:  jwt.NewNumericDate(time.Now().UTC()),
		ID:        tokenID,
	}
	if signer != nil {
		return signClaimsWithSigner(claims, signer, expiresAt)
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
	return token, expiresAt, err
}

func (s *Service) parse(tokenString string, expectedKind TokenKind, secret []byte, signer TokenSigner) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(token *jwt.Token) (any, error) {
		if signer != nil {
			if token.Method.Alg() != signer.Alg() {
				return nil, fmt.Errorf("unexpected signing method")
			}
			return signer.Keyfunc(token)
		}
		if token.Method != jwt.SigningMethodHS256 {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return secret, nil
	})
	if err != nil {
		return nil, err
	}
	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token")
	}
	if claims.Kind != expectedKind {
		return nil, fmt.Errorf("invalid token kind")
	}
	// Tenant authentication is retired at the shared boundary, including callers
	// without an API persistence layer. Keep legacy fields only to recognize and
	// reject old or mixed credentials, never to select a human identity/scope.
	if claims.SubjectType == SubjectTypeBrandCloudUser || claims.BrandCloudUserID != "" || claims.TenantSlug != "" || strings.HasPrefix(claims.Subject, "brand_cloud_user:") {
		return nil, fmt.Errorf("retired tenant identity")
	}
	if claims.SubjectType == "" {
		claims.SubjectType = SubjectTypeUser
	}
	if claims.SubjectType == SubjectTypeDelegatedJob {
		if claims.UserID == "" || claims.BrandCloudID == "" || claims.JobAuthorizationID == "" || claims.JobID == "" || claims.ScopeHash == "" || claims.Capability == "" || claims.AuthorizationVersion <= 0 || claims.OwnershipVersion <= 0 {
			return nil, fmt.Errorf("invalid delegated job token")
		}
		return claims, nil
	}
	if claims.BrandCloudID != "" || claims.SubjectType != SubjectTypeUser && claims.SubjectType != SubjectTypeEndUser {
		return nil, fmt.Errorf("unsupported token subject")
	}
	return claims, nil
}

func signClaimsWithSigner(claims Claims, signer TokenSigner, expiresAt time.Time) (string, time.Time, error) {
	header, err := json.Marshal(map[string]string{"alg": signer.Alg(), "typ": "JWT"})
	if err != nil {
		return "", time.Time{}, err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", time.Time{}, err
	}
	signingString := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	signature, err := signer.SignToken(signingString)
	if err != nil {
		return "", time.Time{}, err
	}
	return signingString + "." + signature, expiresAt, nil
}

func (s RS256TokenSigner) Alg() string {
	return jwt.SigningMethodRS256.Alg()
}

func (s RS256TokenSigner) SignToken(signingString string) (string, error) {
	if s.Signer == nil {
		return "", fmt.Errorf("rsa token signer is required")
	}
	digest := sha256.Sum256([]byte(signingString))
	signature, err := s.Signer.Sign(rand.Reader, digest[:], crypto.SHA256)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(signature), nil
}

func (s RS256TokenSigner) Keyfunc(token *jwt.Token) (any, error) {
	if token == nil || !strings.EqualFold(token.Method.Alg(), s.Alg()) {
		return nil, fmt.Errorf("unexpected signing method")
	}
	if s.PublicKey != nil {
		return s.PublicKey, nil
	}
	if s.Signer != nil {
		if publicKey, ok := s.Signer.Public().(*rsa.PublicKey); ok {
			return publicKey, nil
		}
	}
	return nil, fmt.Errorf("rsa token public key is required")
}

func randomTokenID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(data[:]), nil
}
