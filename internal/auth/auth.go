package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

type TokenKind string

const (
	TokenKindAccess  TokenKind = "access"
	TokenKindRefresh TokenKind = "refresh"
)

type Claims struct {
	UserID string    `json:"user_id"`
	Kind   TokenKind `json:"kind"`
	jwt.RegisteredClaims
}

type Service struct {
	accessSecret  []byte
	refreshSecret []byte
	accessTTL     time.Duration
	refreshTTL    time.Duration
}

func NewService(accessSecret, refreshSecret string, accessTTL, refreshTTL time.Duration) *Service {
	return &Service{
		accessSecret:  []byte(accessSecret),
		refreshSecret: []byte(refreshSecret),
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

func (s *Service) IssueAccessToken(userID string) (string, time.Time, error) {
	return s.issue(userID, TokenKindAccess, s.accessSecret, s.accessTTL)
}

func (s *Service) IssueRefreshToken(userID string) (string, time.Time, error) {
	return s.issue(userID, TokenKindRefresh, s.refreshSecret, s.refreshTTL)
}

func (s *Service) ParseAccessToken(tokenString string) (*Claims, error) {
	return s.parse(tokenString, TokenKindAccess, s.accessSecret)
}

func (s *Service) ParseRefreshToken(tokenString string) (*Claims, error) {
	return s.parse(tokenString, TokenKindRefresh, s.refreshSecret)
}

func (s *Service) issue(userID string, kind TokenKind, secret []byte, ttl time.Duration) (string, time.Time, error) {
	expiresAt := time.Now().UTC().Add(ttl)
	claims := Claims{
		UserID: userID,
		Kind:   kind,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(time.Now().UTC()),
		},
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
	return token, expiresAt, err
}

func (s *Service) parse(tokenString string, expectedKind TokenKind, secret []byte) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(token *jwt.Token) (any, error) {
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
	return claims, nil
}
