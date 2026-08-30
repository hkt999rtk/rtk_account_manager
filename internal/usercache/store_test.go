package usercache

import (
	"context"
	"errors"
	"testing"
	"time"

	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

func TestStoreGetUserReadThroughCachesPostgresMiss(t *testing.T) {
	ctx := context.Background()
	backing := &fakeBacking{
		userByID: map[string]model.User{
			"user-1": testUser("user-1", "owner@example.com"),
		},
	}
	cache := newFakeCache()
	cached := &Store{backing: backing, cache: cache}

	user, err := cached.GetUser(ctx, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if user.Email != "owner@example.com" {
		t.Fatalf("unexpected user: %+v", user)
	}
	if backing.getUserCalls != 1 {
		t.Fatalf("expected one postgres read, got %d", backing.getUserCalls)
	}

	user, err = cached.GetUser(ctx, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if user.Email != "owner@example.com" {
		t.Fatalf("unexpected cached user: %+v", user)
	}
	if backing.getUserCalls != 1 {
		t.Fatalf("expected second read to hit redis cache, got %d postgres reads", backing.getUserCalls)
	}
}

func TestStoreGetUserFallsBackToPostgresWhenRedisUnavailable(t *testing.T) {
	ctx := context.Background()
	backing := &fakeBacking{
		userByID: map[string]model.User{
			"user-1": testUser("user-1", "owner@example.com"),
		},
	}
	cache := newFakeCache()
	cache.getUserErr = errors.New("redis down")
	cached := &Store{backing: backing, cache: cache}

	user, err := cached.GetUser(ctx, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if user.ID != "user-1" {
		t.Fatalf("unexpected user: %+v", user)
	}
	if backing.getUserCalls != 1 {
		t.Fatalf("expected postgres fallback, got %d reads", backing.getUserCalls)
	}
}

func TestStoreGetUserPasswordReadThroughCachesAuthProjection(t *testing.T) {
	ctx := context.Background()
	user := testUser("user-1", "owner@example.com")
	backing := &fakeBacking{
		passwordByEmail: map[string]authRecord{
			"owner@example.com": {user: user, hash: "hash-1"},
		},
	}
	cache := newFakeCache()
	cached := &Store{backing: backing, cache: cache}

	got, hash, err := cached.GetUserPassword(ctx, " OWNER@example.com ")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != user.ID || hash != "hash-1" {
		t.Fatalf("unexpected auth record: %+v %q", got, hash)
	}
	if backing.getPasswordCalls != 1 {
		t.Fatalf("expected one postgres auth read, got %d", backing.getPasswordCalls)
	}

	got, hash, err = cached.GetUserPassword(ctx, "owner@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != user.ID || hash != "hash-1" {
		t.Fatalf("unexpected cached auth record: %+v %q", got, hash)
	}
	if backing.getPasswordCalls != 1 {
		t.Fatalf("expected auth cache hit, got %d postgres reads", backing.getPasswordCalls)
	}
}

func TestStoreRegisterRefreshesUserAuthCacheAfterCommit(t *testing.T) {
	ctx := context.Background()
	user := testUser("user-1", "owner@example.com")
	backing := &fakeBacking{
		registerResult: store.RegisterResult{User: user},
	}
	cache := newFakeCache()
	cached := &Store{backing: backing, cache: cache}

	result, err := cached.Register(ctx, store.RegisterInput{
		Email:        "owner@example.com",
		PasswordHash: "hash-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.User.ID != "user-1" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if _, ok := cache.platformAuth["user-1"]; !ok {
		t.Fatal("expected register to populate auth cache")
	}
}

func TestStoreProvisionBrandCloudAccountRefreshesRotatedAuthCache(t *testing.T) {
	ctx := context.Background()
	user := testUser("user-1", "owner@example.com")
	backing := &fakeBacking{
		brandAccountResult: store.BrandCloudAccountResult{Action: "assigned", User: user},
	}
	cache := newFakeCache()
	if err := cache.PutPlatformAuth(ctx, user, "old-hash"); err != nil {
		t.Fatal(err)
	}
	cached := &Store{backing: backing, cache: cache}

	result, err := cached.ProvisionBrandCloudAccount(ctx, "actor-1", "org-1", store.BrandCloudAccountInput{
		Email:          user.Email,
		PasswordHash:   "new-hash",
		RotatePassword: true,
		ActivationMode: "immediate",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.User.ID != user.ID || backing.provisionBrandAccountCalls != 1 {
		t.Fatalf("result=%+v calls=%d", result, backing.provisionBrandAccountCalls)
	}
	_, hash, ok, err := cache.GetPlatformAuth(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || hash != "new-hash" {
		t.Fatalf("cached auth ok=%v hash=%q", ok, hash)
	}
}

func TestStoreDisableCurrentUserDeletesCachedUser(t *testing.T) {
	ctx := context.Background()
	backing := &fakeBacking{}
	cache := newFakeCache()
	user := testUser("user-1", "owner@example.com")
	if err := cache.PutPlatformAuth(ctx, user, "hash-1"); err != nil {
		t.Fatal(err)
	}
	cached := &Store{backing: backing, cache: cache}

	if err := cached.DisableCurrentUser(ctx, "user-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := cache.platformUsers["user-1"]; ok {
		t.Fatal("expected platform user cache to be deleted")
	}
	if _, ok := cache.platformAuth["user-1"]; ok {
		t.Fatal("expected platform auth cache to be deleted")
	}
}

func TestStoreGetEndUserPasswordReadThroughCachesLoginProjection(t *testing.T) {
	ctx := context.Background()
	user := model.EndUser{
		ID:           "end-user-1",
		PrimaryEmail: "consumer@example.com",
		Status:       "active",
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	backing := &fakeBacking{
		endPasswordByEmail: map[string]store.EndUserLoginResult{
			"consumer@example.com": {
				EndUser:      user,
				PasswordHash: "end-hash",
			},
		},
	}
	cache := newFakeCache()
	cached := &Store{backing: backing, cache: cache}

	result, err := cached.GetEndUserPassword(ctx, " Consumer@Example.com ")
	if err != nil {
		t.Fatal(err)
	}
	if result.EndUser.ID != "end-user-1" || result.PasswordHash != "end-hash" {
		t.Fatalf("unexpected end user login result: %+v", result)
	}
	if backing.getEndPasswordCalls != 1 {
		t.Fatalf("expected one postgres read, got %d", backing.getEndPasswordCalls)
	}

	result, err = cached.GetEndUserPassword(ctx, "consumer@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if result.EndUser.ID != "end-user-1" || result.PasswordHash != "end-hash" {
		t.Fatalf("unexpected cached end user login result: %+v", result)
	}
	if backing.getEndPasswordCalls != 1 {
		t.Fatalf("expected end user cache hit, got %d postgres reads", backing.getEndPasswordCalls)
	}
}

func TestStoreAdditionalPlatformMutationsRefreshCache(t *testing.T) {
	ctx := context.Background()
	user := testUser("user-1", "owner@example.com")
	backing := &fakeBacking{
		developerResult: store.DeveloperSignupResult{User: user},
		passwordByID: map[string]authRecord{
			"user-1": {user: user, hash: "new-hash"},
		},
		verifyUser:   user,
		activateUser: user,
	}
	cache := newFakeCache()
	cached := &Store{backing: backing, cache: cache}

	if _, err := cached.SignupDeveloper(ctx, store.DeveloperSignupInput{PasswordHash: "hash-1"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := cache.platformAuth["user-1"]; !ok {
		t.Fatal("expected signup developer to populate auth cache")
	}
	if _, err := cached.VerifyEmailToken(ctx, "verify-token", "new-password-hash"); err != nil {
		t.Fatal(err)
	}
	if _, ok := cache.platformUsers["user-1"]; !ok {
		t.Fatal("expected verify token to refresh profile cache")
	}
	cache.platformAuth["user-1"] = authRecord{user: user, hash: "stale-hash"}
	if _, err := cached.ActivateLoginToken(ctx, "login-token"); err != nil {
		t.Fatal(err)
	}
	if _, ok := cache.platformAuth["user-1"]; ok {
		t.Fatal("expected login activation to remove stale auth cache")
	}
	if err := cached.UpdateUserPassword(ctx, "user-1", "new-hash"); err != nil {
		t.Fatal(err)
	}
	if got := cache.platformAuth["user-1"].hash; got != "new-hash" {
		t.Fatalf("expected refreshed password hash, got %q", got)
	}
	if _, err := cached.ResetPasswordWithToken(ctx, "reset-token", "reset-hash"); err != nil {
		t.Fatal(err)
	}
	if len(cache.platformAuth) != 0 {
		t.Fatal("expected reset password to flush platform auth cache")
	}
}

func TestStoreAdditionalReadThroughPaths(t *testing.T) {
	ctx := context.Background()
	user := testUser("user-1", "owner@example.com")
	endUser := model.EndUser{ID: "end-user-1", PrimaryEmail: "consumer@example.com", Status: "active"}
	backing := &fakeBacking{
		userByEmail: map[string]model.User{
			"owner@example.com": user,
		},
		passwordByID: map[string]authRecord{
			"user-1": {user: user, hash: "hash-1"},
		},
		endUserByID: map[string]model.EndUser{
			"end-user-1": endUser,
		},
	}
	cache := newFakeCache()
	cached := &Store{backing: backing, cache: cache}

	if got, err := cached.GetUserByEmail(ctx, " Owner@Example.com "); err != nil || got.ID != "user-1" {
		t.Fatalf("unexpected user by email: %+v %v", got, err)
	}
	if got, hash, err := cached.GetUserPasswordByID(ctx, "user-1"); err != nil || got.ID != "user-1" || hash != "hash-1" {
		t.Fatalf("unexpected password by id: %+v %q %v", got, hash, err)
	}
	if got, err := cached.GetEndUser(ctx, "end-user-1"); err != nil || got.ID != "end-user-1" {
		t.Fatalf("unexpected end user: %+v %v", got, err)
	}
}

func TestStoreEndUserMutationRefreshesCache(t *testing.T) {
	ctx := context.Background()
	endUser := model.EndUser{ID: "end-user-1", PrimaryEmail: "consumer@example.com", Status: "active"}
	backing := &fakeBacking{
		createEndUser: endUser,
	}
	cache := newFakeCache()
	cached := &Store{backing: backing, cache: cache}

	if _, err := cached.CreateEndUser(ctx, store.EndUserCreateInput{PasswordHash: "end-hash"}); err != nil {
		t.Fatal(err)
	}
	if got := cache.endLogin["end-user-1"].PasswordHash; got != "end-hash" {
		t.Fatalf("expected end user login cache, got %q", got)
	}
}

func TestStoreConstructorAndHelpers(t *testing.T) {
	cache := newFakeCache()
	cached := NewStore(nil, cache, nil)
	if cached.logger == nil {
		t.Fatal("expected constructor to install no-op logger")
	}
	cached.warn("not found is ignored", store.ErrNotFound)
	cached.warn("nil error is ignored", nil)

	cases := map[string]string{
		" ACME Inc. ": "acme-inc",
		"!!!":         "",
		"a---b":       "a-b",
	}
	for input, want := range cases {
		if got := normalizeTenantSlug(input); got != want {
			t.Fatalf("normalizeTenantSlug(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestStoreIgnoresCacheReadAndWriteErrors(t *testing.T) {
	ctx := context.Background()
	user := testUser("user-1", "owner@example.com")
	cacheErr := errors.New("redis unavailable")
	backing := &fakeBacking{
		userByEmail: map[string]model.User{
			"owner@example.com": user,
		},
		passwordByEmail: map[string]authRecord{
			"owner@example.com": {user: user, hash: "hash-1"},
		},
		endPasswordByEmail: map[string]store.EndUserLoginResult{
			"consumer@example.com": {
				EndUser:      model.EndUser{ID: "end-user-1", PrimaryEmail: "consumer@example.com", Status: "active"},
				PasswordHash: "end-hash",
			},
		},
	}
	cache := newFakeCache()
	cache.cacheReadErr = cacheErr
	cache.cacheWriteErr = cacheErr
	cached := &Store{backing: backing, cache: cache}

	if _, err := cached.GetUserByEmail(ctx, "owner@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cached.GetUserPassword(ctx, "owner@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := cached.GetEndUserPassword(ctx, "consumer@example.com"); err != nil {
		t.Fatal(err)
	}
}

func testUser(id, email string) model.User {
	now := time.Date(2026, 6, 21, 8, 0, 0, 0, time.UTC)
	return model.User{
		ID:            id,
		Email:         email,
		EmailVerified: true,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
}

type authRecord struct {
	user model.User
	hash string
}

type fakeBacking struct {
	userByID                   map[string]model.User
	userByEmail                map[string]model.User
	passwordByEmail            map[string]authRecord
	passwordByID               map[string]authRecord
	registerResult             store.RegisterResult
	developerResult            store.DeveloperSignupResult
	brandAccountResult         store.BrandCloudAccountResult
	verifyUser                 model.User
	activateUser               model.User
	brandPasswordByTenantEmail map[string]store.BrandCloudLoginResult
	brandUserByID              map[string]model.BrandCloudUser
	brandUserResult            store.BrandCloudUserResult
	activateBrandResult        store.BrandCloudLoginResult
	endPasswordByEmail         map[string]store.EndUserLoginResult
	endUserByID                map[string]model.EndUser
	createEndUser              model.EndUser

	getUserCalls               int
	getPasswordCalls           int
	getBrandPasswordCalls      int
	getEndPasswordCalls        int
	provisionBrandAccountCalls int
}

func (f *fakeBacking) Register(context.Context, store.RegisterInput) (store.RegisterResult, error) {
	return f.registerResult, nil
}

func (f *fakeBacking) SignupDeveloper(context.Context, store.DeveloperSignupInput) (store.DeveloperSignupResult, error) {
	return f.developerResult, nil
}

func (f *fakeBacking) ProvisionBrandCloudAccount(context.Context, string, string, store.BrandCloudAccountInput) (store.BrandCloudAccountResult, error) {
	f.provisionBrandAccountCalls++
	return f.brandAccountResult, nil
}

func (f *fakeBacking) GetUserPassword(_ context.Context, email string) (model.User, string, error) {
	f.getPasswordCalls++
	record, ok := f.passwordByEmail[normalizeEmail(email)]
	if !ok {
		return model.User{}, "", store.ErrNotFound
	}
	return record.user, record.hash, nil
}

func (f *fakeBacking) GetUserPasswordByID(_ context.Context, userID string) (model.User, string, error) {
	record, ok := f.passwordByID[userID]
	if !ok {
		return model.User{}, "", store.ErrNotFound
	}
	return record.user, record.hash, nil
}

func (f *fakeBacking) VerifyEmailToken(context.Context, string, string) (model.User, error) {
	return f.verifyUser, nil
}

func (f *fakeBacking) ActivateLoginToken(context.Context, string) (model.User, error) {
	return f.activateUser, nil
}

func (f *fakeBacking) ResetPasswordWithToken(context.Context, string, string) (string, error) {
	return "owner@example.com", nil
}

func (f *fakeBacking) UpdateUserPassword(context.Context, string, string) error {
	return nil
}

func (f *fakeBacking) GetBrandCloudUserPassword(_ context.Context, tenantSlug, email string) (store.BrandCloudLoginResult, error) {
	f.getBrandPasswordCalls++
	result, ok := f.brandPasswordByTenantEmail[normalizeTenantSlug(tenantSlug)+":"+normalizeEmail(email)]
	if !ok {
		return store.BrandCloudLoginResult{}, store.ErrNotFound
	}
	return result, nil
}

func (f *fakeBacking) ActivateBrandCloudLoginToken(context.Context, string, string) (store.BrandCloudLoginResult, error) {
	return f.activateBrandResult, nil
}

func (f *fakeBacking) GetBrandCloudUser(_ context.Context, userID string) (model.BrandCloudUser, error) {
	user, ok := f.brandUserByID[userID]
	if !ok {
		return model.BrandCloudUser{}, store.ErrNotFound
	}
	return user, nil
}

func (f *fakeBacking) CreateBrandCloudUser(context.Context, string, string, store.BrandCloudUserInput) (store.BrandCloudUserResult, error) {
	return f.brandUserResult, nil
}

func (f *fakeBacking) DisableBrandCloudUser(context.Context, string, string, string) (model.BrandCloudUser, error) {
	return f.brandUserByID["brand-user-1"], nil
}

func (f *fakeBacking) EnableBrandCloudUser(context.Context, string, string, string) (model.BrandCloudUser, error) {
	return f.brandUserByID["brand-user-1"], nil
}

func (f *fakeBacking) ApproveBrandCloudUser(context.Context, string, string, string) (model.BrandCloudUser, error) {
	return f.brandUserByID["brand-user-1"], nil
}

func (f *fakeBacking) DeleteBrandCloudUser(context.Context, string, string, string) error {
	return nil
}

func (f *fakeBacking) CreateEndUser(context.Context, store.EndUserCreateInput) (model.EndUser, error) {
	return f.createEndUser, nil
}

func (f *fakeBacking) GetEndUserPassword(_ context.Context, email string) (store.EndUserLoginResult, error) {
	f.getEndPasswordCalls++
	result, ok := f.endPasswordByEmail[normalizeEmail(email)]
	if !ok {
		return store.EndUserLoginResult{}, store.ErrNotFound
	}
	return result, nil
}

func (f *fakeBacking) GetEndUser(_ context.Context, userID string) (model.EndUser, error) {
	user, ok := f.endUserByID[userID]
	if !ok {
		return model.EndUser{}, store.ErrNotFound
	}
	return user, nil
}

func (f *fakeBacking) GetUser(_ context.Context, userID string) (model.User, error) {
	f.getUserCalls++
	user, ok := f.userByID[userID]
	if !ok {
		return model.User{}, store.ErrNotFound
	}
	return user, nil
}

func (f *fakeBacking) GetUserByEmail(_ context.Context, email string) (model.User, error) {
	user, ok := f.userByEmail[normalizeEmail(email)]
	if !ok {
		return model.User{}, store.ErrNotFound
	}
	return user, nil
}

func (f *fakeBacking) DisableCurrentUser(context.Context, string) error {
	return nil
}
