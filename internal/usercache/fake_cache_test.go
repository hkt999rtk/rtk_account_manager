package usercache

import (
	"context"

	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

type fakeCache struct {
	platformUsers   map[string]model.User
	platformEmailID map[string]string
	platformAuth    map[string]authRecord
	brandUsers      map[string]model.BrandCloudUser
	brandEmailID    map[string]string
	brandLogin      map[string]store.BrandCloudLoginResult
	endUsers        map[string]model.EndUser
	endEmailID      map[string]string
	endLogin        map[string]store.EndUserLoginResult

	getUserErr    error
	cacheReadErr  error
	cacheWriteErr error
}

func newFakeCache() *fakeCache {
	return &fakeCache{
		platformUsers:   map[string]model.User{},
		platformEmailID: map[string]string{},
		platformAuth:    map[string]authRecord{},
		brandUsers:      map[string]model.BrandCloudUser{},
		brandEmailID:    map[string]string{},
		brandLogin:      map[string]store.BrandCloudLoginResult{},
		endUsers:        map[string]model.EndUser{},
		endEmailID:      map[string]string{},
		endLogin:        map[string]store.EndUserLoginResult{},
	}
}

func (f *fakeCache) GetPlatformUser(_ context.Context, userID string) (model.User, bool, error) {
	if f.getUserErr != nil {
		return model.User{}, false, f.getUserErr
	}
	user, ok := f.platformUsers[userID]
	return user, ok, nil
}

func (f *fakeCache) GetPlatformUserIDByEmail(_ context.Context, email string) (string, bool, error) {
	if f.cacheReadErr != nil {
		return "", false, f.cacheReadErr
	}
	userID, ok := f.platformEmailID[normalizeEmail(email)]
	return userID, ok, nil
}

func (f *fakeCache) GetPlatformAuth(_ context.Context, userID string) (model.User, string, bool, error) {
	if f.cacheReadErr != nil {
		return model.User{}, "", false, f.cacheReadErr
	}
	record, ok := f.platformAuth[userID]
	return record.user, record.hash, ok, nil
}

func (f *fakeCache) PutPlatformUser(_ context.Context, user model.User) error {
	if f.cacheWriteErr != nil {
		return f.cacheWriteErr
	}
	if user.ID == "" {
		return store.ErrNotFound
	}
	f.platformUsers[user.ID] = user
	f.platformEmailID[normalizeEmail(user.Email)] = user.ID
	return nil
}

func (f *fakeCache) PutPlatformAuth(_ context.Context, user model.User, hash string) error {
	if f.cacheWriteErr != nil {
		return f.cacheWriteErr
	}
	if err := f.PutPlatformUser(context.Background(), user); err != nil {
		return err
	}
	f.platformAuth[user.ID] = authRecord{user: user, hash: hash}
	return nil
}

func (f *fakeCache) DeletePlatformUser(_ context.Context, userID string) error {
	if f.cacheWriteErr != nil {
		return f.cacheWriteErr
	}
	user, ok := f.platformUsers[userID]
	if ok {
		delete(f.platformEmailID, normalizeEmail(user.Email))
	}
	delete(f.platformUsers, userID)
	delete(f.platformAuth, userID)
	return nil
}

func (f *fakeCache) FlushPlatformAuth(context.Context) error {
	f.platformAuth = map[string]authRecord{}
	return nil
}

func (f *fakeCache) GetBrandCloudUser(_ context.Context, userID string) (model.BrandCloudUser, bool, error) {
	user, ok := f.brandUsers[userID]
	return user, ok, nil
}

func (f *fakeCache) GetBrandCloudUserIDByTenantEmail(_ context.Context, tenantSlug, email string) (string, bool, error) {
	if f.cacheReadErr != nil {
		return "", false, f.cacheReadErr
	}
	userID, ok := f.brandEmailID[normalizeTenantSlug(tenantSlug)+":"+normalizeEmail(email)]
	return userID, ok, nil
}

func (f *fakeCache) GetBrandCloudLogin(_ context.Context, userID string) (store.BrandCloudLoginResult, bool, error) {
	if f.cacheReadErr != nil {
		return store.BrandCloudLoginResult{}, false, f.cacheReadErr
	}
	result, ok := f.brandLogin[userID]
	return result, ok, nil
}

func (f *fakeCache) PutBrandCloudUser(_ context.Context, user model.BrandCloudUser) error {
	if f.cacheWriteErr != nil {
		return f.cacheWriteErr
	}
	f.brandUsers[user.ID] = user
	return nil
}

func (f *fakeCache) PutBrandCloudLogin(_ context.Context, tenantSlug string, result store.BrandCloudLoginResult) error {
	if f.cacheWriteErr != nil {
		return f.cacheWriteErr
	}
	user := result.BrandCloudUser
	f.brandUsers[user.ID] = user
	f.brandEmailID[normalizeTenantSlug(tenantSlug)+":"+normalizeEmail(user.Email)] = user.ID
	f.brandLogin[user.ID] = result
	return nil
}

func (f *fakeCache) DeleteBrandCloudUser(_ context.Context, userID string) error {
	if f.cacheWriteErr != nil {
		return f.cacheWriteErr
	}
	delete(f.brandUsers, userID)
	delete(f.brandLogin, userID)
	for key, value := range f.brandEmailID {
		if value == userID {
			delete(f.brandEmailID, key)
		}
	}
	return nil
}

func (f *fakeCache) GetEndUser(_ context.Context, userID string) (model.EndUser, bool, error) {
	user, ok := f.endUsers[userID]
	return user, ok, nil
}

func (f *fakeCache) GetEndUserIDByEmail(_ context.Context, email string) (string, bool, error) {
	if f.cacheReadErr != nil {
		return "", false, f.cacheReadErr
	}
	userID, ok := f.endEmailID[normalizeEmail(email)]
	return userID, ok, nil
}

func (f *fakeCache) GetEndUserLogin(_ context.Context, userID string) (store.EndUserLoginResult, bool, error) {
	if f.cacheReadErr != nil {
		return store.EndUserLoginResult{}, false, f.cacheReadErr
	}
	result, ok := f.endLogin[userID]
	return result, ok, nil
}

func (f *fakeCache) PutEndUser(_ context.Context, user model.EndUser) error {
	if f.cacheWriteErr != nil {
		return f.cacheWriteErr
	}
	f.endUsers[user.ID] = user
	f.endEmailID[normalizeEmail(user.PrimaryEmail)] = user.ID
	return nil
}

func (f *fakeCache) PutEndUserLogin(_ context.Context, result store.EndUserLoginResult) error {
	if f.cacheWriteErr != nil {
		return f.cacheWriteErr
	}
	if err := f.PutEndUser(context.Background(), result.EndUser); err != nil {
		return err
	}
	f.endLogin[result.EndUser.ID] = result
	return nil
}

func (f *fakeCache) DeleteEndUser(_ context.Context, userID string) error {
	user, ok := f.endUsers[userID]
	if ok {
		delete(f.endEmailID, normalizeEmail(user.PrimaryEmail))
	}
	delete(f.endUsers, userID)
	delete(f.endLogin, userID)
	return nil
}
