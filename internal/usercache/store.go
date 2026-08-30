package usercache

import (
	"context"
	"errors"
	"strings"

	"go.uber.org/zap"

	"rtk_account_manager/internal/api"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

type Config struct {
	Addr   string
	Prefix string
}

type Store struct {
	api.Store
	backing backingStore
	cache   cacheStore
	logger  *zap.Logger
}

type backingStore interface {
	Register(context.Context, store.RegisterInput) (store.RegisterResult, error)
	SignupDeveloper(context.Context, store.DeveloperSignupInput) (store.DeveloperSignupResult, error)
	GetUserPassword(context.Context, string) (model.User, string, error)
	GetUserPasswordByID(context.Context, string) (model.User, string, error)
	VerifyEmailToken(context.Context, string, string) (model.User, error)
	ActivateLoginToken(context.Context, string) (model.User, error)
	ResetPasswordWithToken(context.Context, string, string) (string, error)
	UpdateUserPassword(context.Context, string, string) error
	CreateEndUser(context.Context, store.EndUserCreateInput) (model.EndUser, error)
	GetEndUserPassword(context.Context, string) (store.EndUserLoginResult, error)
	GetEndUser(context.Context, string) (model.EndUser, error)
	GetUser(context.Context, string) (model.User, error)
	GetUserByEmail(context.Context, string) (model.User, error)
	DisableCurrentUser(context.Context, string) error
}

type cacheStore interface {
	GetPlatformUser(context.Context, string) (model.User, bool, error)
	GetPlatformUserIDByEmail(context.Context, string) (string, bool, error)
	GetPlatformAuth(context.Context, string) (model.User, string, bool, error)
	PutPlatformUser(context.Context, model.User) error
	PutPlatformAuth(context.Context, model.User, string) error
	DeletePlatformUser(context.Context, string) error
	FlushPlatformAuth(context.Context) error
	GetBrandCloudUser(context.Context, string) (model.BrandCloudUser, bool, error)
	GetBrandCloudUserIDByTenantEmail(context.Context, string, string) (string, bool, error)
	GetBrandCloudLogin(context.Context, string) (store.BrandCloudLoginResult, bool, error)
	PutBrandCloudUser(context.Context, model.BrandCloudUser) error
	PutBrandCloudLogin(context.Context, string, store.BrandCloudLoginResult) error
	DeleteBrandCloudUser(context.Context, string) error
	GetEndUser(context.Context, string) (model.EndUser, bool, error)
	GetEndUserIDByEmail(context.Context, string) (string, bool, error)
	GetEndUserLogin(context.Context, string) (store.EndUserLoginResult, bool, error)
	PutEndUser(context.Context, model.EndUser) error
	PutEndUserLogin(context.Context, store.EndUserLoginResult) error
	DeleteEndUser(context.Context, string) error
}

func NewStore(backing api.Store, cache cacheStore, logger *zap.Logger) *Store {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Store{
		Store:   backing,
		backing: backing,
		cache:   cache,
		logger:  logger,
	}
}

func (s *Store) Register(ctx context.Context, in store.RegisterInput) (store.RegisterResult, error) {
	result, err := s.backing.Register(ctx, in)
	if err != nil {
		return store.RegisterResult{}, err
	}
	s.putPlatformAuth(ctx, result.User, in.PasswordHash)
	return result, nil
}

func (s *Store) SignupDeveloper(ctx context.Context, in store.DeveloperSignupInput) (store.DeveloperSignupResult, error) {
	result, err := s.backing.SignupDeveloper(ctx, in)
	if err != nil {
		return store.DeveloperSignupResult{}, err
	}
	s.putPlatformAuth(ctx, result.User, in.PasswordHash)
	return result, nil
}

func (s *Store) GetUserPassword(ctx context.Context, email string) (model.User, string, error) {
	normalized := normalizeEmail(email)
	if userID, ok := s.getPlatformUserIDByEmail(ctx, normalized); ok {
		if user, hash, ok := s.getPlatformAuth(ctx, userID); ok {
			return user, hash, nil
		}
	}
	user, hash, err := s.backing.GetUserPassword(ctx, normalized)
	if err != nil {
		return model.User{}, "", err
	}
	s.putPlatformAuth(ctx, user, hash)
	return user, hash, nil
}

func (s *Store) GetUserPasswordByID(ctx context.Context, userID string) (model.User, string, error) {
	if user, hash, ok := s.getPlatformAuth(ctx, userID); ok {
		return user, hash, nil
	}
	user, hash, err := s.backing.GetUserPasswordByID(ctx, userID)
	if err != nil {
		return model.User{}, "", err
	}
	s.putPlatformAuth(ctx, user, hash)
	return user, hash, nil
}

func (s *Store) VerifyEmailToken(ctx context.Context, tokenHash, passwordHash string) (model.User, error) {
	user, err := s.backing.VerifyEmailToken(ctx, tokenHash, passwordHash)
	if err != nil {
		return model.User{}, err
	}
	s.deletePlatformUser(ctx, user.ID)
	s.putPlatformUser(ctx, user)
	return user, nil
}

func (s *Store) ActivateLoginToken(ctx context.Context, tokenHash string) (model.User, error) {
	user, err := s.backing.ActivateLoginToken(ctx, tokenHash)
	if err != nil {
		return model.User{}, err
	}
	s.deletePlatformUser(ctx, user.ID)
	s.putPlatformUser(ctx, user)
	return user, nil
}

func (s *Store) ResetPasswordWithToken(ctx context.Context, tokenHash, passwordHash string) (string, error) {
	email, err := s.backing.ResetPasswordWithToken(ctx, tokenHash, passwordHash)
	if err != nil {
		return "", err
	}
	if err := s.cache.FlushPlatformAuth(ctx); err != nil {
		s.warn("flush platform auth cache failed", err)
	}
	return email, nil
}

func (s *Store) UpdateUserPassword(ctx context.Context, userID, passwordHash string) error {
	if err := s.backing.UpdateUserPassword(ctx, userID, passwordHash); err != nil {
		return err
	}
	user, hash, err := s.backing.GetUserPasswordByID(ctx, userID)
	if err != nil {
		s.deletePlatformUser(ctx, userID)
		return nil
	}
	s.putPlatformAuth(ctx, user, hash)
	return nil
}

func (s *Store) GetUser(ctx context.Context, userID string) (model.User, error) {
	if user, ok := s.getPlatformUser(ctx, userID); ok {
		return user, nil
	}
	user, err := s.backing.GetUser(ctx, userID)
	if err != nil {
		return model.User{}, err
	}
	s.putPlatformUser(ctx, user)
	return user, nil
}

func (s *Store) GetUserByEmail(ctx context.Context, email string) (model.User, error) {
	normalized := normalizeEmail(email)
	if userID, ok := s.getPlatformUserIDByEmail(ctx, normalized); ok {
		if user, ok := s.getPlatformUser(ctx, userID); ok {
			return user, nil
		}
	}
	user, err := s.backing.GetUserByEmail(ctx, normalized)
	if err != nil {
		return model.User{}, err
	}
	s.putPlatformUser(ctx, user)
	return user, nil
}

func (s *Store) DisableCurrentUser(ctx context.Context, userID string) error {
	if err := s.backing.DisableCurrentUser(ctx, userID); err != nil {
		return err
	}
	s.deletePlatformUser(ctx, userID)
	return nil
}

func (s *Store) CreateEndUser(ctx context.Context, in store.EndUserCreateInput) (model.EndUser, error) {
	user, err := s.backing.CreateEndUser(ctx, in)
	if err != nil {
		return model.EndUser{}, err
	}
	if in.PasswordHash != "" {
		s.putEndUserLogin(ctx, store.EndUserLoginResult{EndUser: user, PasswordHash: in.PasswordHash})
	} else {
		s.putEndUser(ctx, user)
	}
	return user, nil
}

func (s *Store) GetEndUserPassword(ctx context.Context, email string) (store.EndUserLoginResult, error) {
	email = normalizeEmail(email)
	if userID, ok := s.getEndUserIDByEmail(ctx, email); ok {
		if result, ok := s.getEndUserLogin(ctx, userID); ok {
			return result, nil
		}
	}
	result, err := s.backing.GetEndUserPassword(ctx, email)
	if err != nil {
		return store.EndUserLoginResult{}, err
	}
	s.putEndUserLogin(ctx, result)
	return result, nil
}

func (s *Store) GetEndUser(ctx context.Context, endUserID string) (model.EndUser, error) {
	user, ok, err := s.cache.GetEndUser(ctx, strings.TrimSpace(endUserID))
	if err != nil {
		s.warn("get end user cache failed", err)
	} else if ok {
		return user, nil
	}
	user, err = s.backing.GetEndUser(ctx, endUserID)
	if err != nil {
		return model.EndUser{}, err
	}
	s.putEndUser(ctx, user)
	return user, nil
}

func (s *Store) getPlatformUser(ctx context.Context, userID string) (model.User, bool) {
	user, ok, err := s.cache.GetPlatformUser(ctx, strings.TrimSpace(userID))
	if err != nil {
		s.warn("get platform user cache failed", err)
		return model.User{}, false
	}
	return user, ok
}

func (s *Store) getPlatformUserIDByEmail(ctx context.Context, email string) (string, bool) {
	userID, ok, err := s.cache.GetPlatformUserIDByEmail(ctx, normalizeEmail(email))
	if err != nil {
		s.warn("get platform user email cache failed", err)
		return "", false
	}
	return userID, ok && strings.TrimSpace(userID) != ""
}

func (s *Store) getPlatformAuth(ctx context.Context, userID string) (model.User, string, bool) {
	user, hash, ok, err := s.cache.GetPlatformAuth(ctx, strings.TrimSpace(userID))
	if err != nil {
		s.warn("get platform auth cache failed", err)
		return model.User{}, "", false
	}
	return user, hash, ok
}

func (s *Store) putPlatformUser(ctx context.Context, user model.User) {
	if err := s.cache.PutPlatformUser(ctx, user); err != nil {
		s.warn("put platform user cache failed", err)
	}
}

func (s *Store) putPlatformAuth(ctx context.Context, user model.User, hash string) {
	if err := s.cache.PutPlatformAuth(ctx, user, hash); err != nil {
		s.warn("put platform auth cache failed", err)
	}
}

func (s *Store) deletePlatformUser(ctx context.Context, userID string) {
	if err := s.cache.DeletePlatformUser(ctx, userID); err != nil {
		s.warn("delete platform user cache failed", err)
	}
}

func (s *Store) getEndUserIDByEmail(ctx context.Context, email string) (string, bool) {
	userID, ok, err := s.cache.GetEndUserIDByEmail(ctx, email)
	if err != nil {
		s.warn("get end user email cache failed", err)
		return "", false
	}
	return userID, ok && strings.TrimSpace(userID) != ""
}

func (s *Store) getEndUserLogin(ctx context.Context, userID string) (store.EndUserLoginResult, bool) {
	result, ok, err := s.cache.GetEndUserLogin(ctx, userID)
	if err != nil {
		s.warn("get end user login cache failed", err)
		return store.EndUserLoginResult{}, false
	}
	return result, ok
}

func (s *Store) putEndUser(ctx context.Context, user model.EndUser) {
	if err := s.cache.PutEndUser(ctx, user); err != nil {
		s.warn("put end user cache failed", err)
	}
}

func (s *Store) putEndUserLogin(ctx context.Context, result store.EndUserLoginResult) {
	if err := s.cache.PutEndUserLogin(ctx, result); err != nil {
		s.warn("put end user login cache failed", err)
	}
}

func (s *Store) warn(message string, err error) {
	if err == nil || errors.Is(err, store.ErrNotFound) {
		return
	}
	if s.logger != nil {
		s.logger.Warn(message, zap.Error(err))
	}
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func normalizeTenantSlug(slug string) string {
	slug = strings.ToLower(strings.TrimSpace(slug))
	var b strings.Builder
	dash := false
	for _, r := range slug {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
