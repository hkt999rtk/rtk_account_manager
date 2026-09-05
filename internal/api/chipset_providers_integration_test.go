package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

type integrationChipsetFetcher struct {
	mu     sync.Mutex
	result ChipsetManifestFetchResult
	err    error
	calls  int
}

func (*integrationChipsetFetcher) ValidateURL(raw string) error {
	if raw != "https://provider.example.com/amebapro2.json" && raw != "https://provider.example.com/amebapro2-no-snapshot.json" {
		return errChipsetProviderHostNotAllowed
	}
	return nil
}

func (f *integrationChipsetFetcher) Fetch(context.Context, model.ChipsetProvider) (ChipsetManifestFetchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.result, f.err
}

func (f *integrationChipsetFetcher) set(result ChipsetManifestFetchResult, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.result, f.err = result, err
}

func (f *integrationChipsetFetcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestIntegrationChipsetProviderACLRefreshVisibilityAndAudit(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()
	fetcher := &integrationChipsetFetcher{result: integrationManifestResult("1.0.0")}
	env.server.ConfigureChipsetManifestFetcher(fetcher)

	admin := legacyCustomerForTest(t, env, "chipset-admin@example.com", "Chipset Admin")
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin = true WHERE id = $1`, admin.User.ID); err != nil {
		t.Fatal(err)
	}
	developer := legacyCustomerForTest(t, env, "chipset-developer@example.com", "Chipset Developer")
	readUser := legacyCustomerForTest(t, env, "chipset-reader@example.com", "Chipset Reader")
	editUser := legacyCustomerForTest(t, env, "chipset-editor@example.com", "Chipset Editor")
	publishUser := legacyCustomerForTest(t, env, "chipset-publisher@example.com", "Chipset Publisher")

	aclStore := store.New(env.db)
	bindPlatformCapability(t, ctx, aclStore, readUser.User.ID, "chipset_reader_test_"+readUser.User.ID, store.PermissionChipsetProviderRead)
	bindPlatformCapability(t, ctx, aclStore, editUser.User.ID, "chipset_editor_test_"+editUser.User.ID, store.PermissionChipsetProviderEdit)
	bindPlatformCapability(t, ctx, aclStore, publishUser.User.ID, "chipset_publisher_test_"+publishUser.User.ID, store.PermissionChipsetProviderPublish)

	if res := chipsetRequest(env.router, http.MethodGet, "/v1/admin/chipset-providers", nil, developer.Tokens.AccessToken, "", ""); res.Code != http.StatusForbidden {
		t.Fatalf("ordinary developer admin list = %d: %s", res.Code, res.Body.String())
	}
	if res := chipsetRequest(env.router, http.MethodPost, "/v1/admin/chipset-providers", providerWriteBody(), editUser.Tokens.AccessToken, "", "req-missing-key"); res.Code != http.StatusBadRequest {
		t.Fatalf("missing idempotency key = %d: %s", res.Code, res.Body.String())
	}
	create := chipsetRequest(env.router, http.MethodPost, "/v1/admin/chipset-providers", providerWriteBody(), editUser.Tokens.AccessToken, "create-ameba", "req-create")
	if create.Code != http.StatusCreated {
		t.Fatalf("edit-only create = %d: %s", create.Code, create.Body.String())
	}
	var created struct {
		Provider    model.ChipsetProvider `json:"provider"`
		AuditResult string                `json:"audit_result"`
	}
	decodeRecorder(t, create, &created)
	if created.Provider.Status != model.ChipsetProviderStatusDraft || !created.Provider.Unavailable || created.AuditResult != "accepted" {
		t.Fatalf("created provider = %+v", created)
	}
	invalidCreate := chipsetRequest(env.router, http.MethodPost, "/v1/admin/chipset-providers", map[string]any{"name": " ", "manifest_url": "http://provider.example.com/invalid.json"}, editUser.Tokens.AccessToken, "invalid-create", "req-invalid-create")
	if invalidCreate.Code != http.StatusBadRequest {
		t.Fatalf("invalid provider create = %d: %s", invalidCreate.Code, invalidCreate.Body.String())
	}
	malformedCreate := chipsetRequest(env.router, http.MethodPost, "/v1/admin/chipset-providers", "not-an-object", editUser.Tokens.AccessToken, "malformed-create", "req-malformed-create")
	if malformedCreate.Code != http.StatusBadRequest {
		t.Fatalf("malformed provider create = %d: %s", malformedCreate.Code, malformedCreate.Body.String())
	}
	updatedBody := map[string]any{"name": "Ameba IoT Updated", "manifest_url": "https://provider.example.com/amebapro2.json"}
	if res := chipsetRequest(env.router, http.MethodPatch, "/v1/admin/chipset-providers/"+created.Provider.ID, updatedBody, editUser.Tokens.AccessToken, "", "req-update-no-key"); res.Code != http.StatusBadRequest {
		t.Fatalf("update without idempotency key = %d: %s", res.Code, res.Body.String())
	}
	if res := chipsetRequest(env.router, http.MethodPatch, "/v1/admin/chipset-providers/"+created.Provider.ID, "not-an-object", editUser.Tokens.AccessToken, "malformed-update", "req-malformed-update"); res.Code != http.StatusBadRequest {
		t.Fatalf("malformed provider update = %d: %s", res.Code, res.Body.String())
	}
	updated := chipsetRequest(env.router, http.MethodPatch, "/v1/admin/chipset-providers/"+created.Provider.ID, updatedBody, editUser.Tokens.AccessToken, "update-draft", "req-update-draft")
	if updated.Code != http.StatusOK || !bytes.Contains(updated.Body.Bytes(), []byte(`"name":"Ameba IoT Updated"`)) {
		t.Fatalf("draft provider update = %d: %s", updated.Code, updated.Body.String())
	}
	missingUpdate := chipsetRequest(env.router, http.MethodPatch, "/v1/admin/chipset-providers/00000000-0000-0000-0000-000000000000", updatedBody, editUser.Tokens.AccessToken, "update-missing", "req-update-missing")
	if missingUpdate.Code != http.StatusNotFound {
		t.Fatalf("missing provider update = %d: %s", missingUpdate.Code, missingUpdate.Body.String())
	}
	invalidUpdate := chipsetRequest(env.router, http.MethodPatch, "/v1/admin/chipset-providers/"+created.Provider.ID, map[string]any{"name": "Ameba", "manifest_url": "http://provider.example.com/manifest.json"}, editUser.Tokens.AccessToken, "invalid-update", "req-invalid-update")
	if invalidUpdate.Code != http.StatusBadRequest {
		t.Fatalf("invalid provider update = %d: %s", invalidUpdate.Code, invalidUpdate.Body.String())
	}

	if res := chipsetRequest(env.router, http.MethodGet, "/v1/admin/chipset-providers", nil, readUser.Tokens.AccessToken, "", ""); res.Code != http.StatusOK {
		t.Fatalf("read-only list = %d: %s", res.Code, res.Body.String())
	}
	if res := chipsetRequest(env.router, http.MethodGet, "/v1/admin/chipset-providers/"+created.Provider.ID, nil, readUser.Tokens.AccessToken, "", ""); res.Code != http.StatusOK || !bytes.Contains(res.Body.Bytes(), []byte(`"manifest_url":"https://provider.example.com/amebapro2.json"`)) {
		t.Fatalf("read-only detail = %d: %s", res.Code, res.Body.String())
	}
	if res := chipsetRequest(env.router, http.MethodGet, "/v1/admin/chipset-providers/00000000-0000-0000-0000-000000000000", nil, readUser.Tokens.AccessToken, "", ""); res.Code != http.StatusNotFound {
		t.Fatalf("missing provider detail = %d: %s", res.Code, res.Body.String())
	}
	if res := chipsetRequest(env.router, http.MethodPost, "/v1/admin/chipset-providers/"+created.Provider.ID+"/publish", nil, readUser.Tokens.AccessToken, "read-cannot-publish", "req-read"); res.Code != http.StatusForbidden {
		t.Fatalf("read-only publish = %d: %s", res.Code, res.Body.String())
	}
	if res := chipsetRequest(env.router, http.MethodPost, "/v1/admin/chipset-providers/"+created.Provider.ID+"/refresh", nil, publishUser.Tokens.AccessToken, "", "req-action-no-key"); res.Code != http.StatusBadRequest {
		t.Fatalf("action without idempotency key = %d: %s", res.Code, res.Body.String())
	}
	if res := chipsetRequest(env.router, http.MethodPost, "/v1/admin/chipset-providers/"+created.Provider.ID+"/invalid", nil, publishUser.Tokens.AccessToken, "invalid-action", "req-invalid-action"); res.Code != http.StatusBadRequest {
		t.Fatalf("invalid provider action = %d: %s", res.Code, res.Body.String())
	}
	if res := chipsetRequest(env.router, http.MethodPost, "/v1/admin/chipset-providers/00000000-0000-0000-0000-000000000000/refresh", nil, publishUser.Tokens.AccessToken, "refresh-missing", "req-refresh-missing"); res.Code != http.StatusNotFound {
		t.Fatalf("missing provider refresh = %d: %s", res.Code, res.Body.String())
	}
	if res := chipsetRequest(env.router, http.MethodPost, "/v1/admin/chipset-providers/00000000-0000-0000-0000-000000000000/publish", nil, publishUser.Tokens.AccessToken, "publish-missing", "req-publish-missing"); res.Code != http.StatusNotFound {
		t.Fatalf("missing provider publish = %d: %s", res.Code, res.Body.String())
	}
	if res := chipsetRequest(env.router, http.MethodPost, "/v1/admin/chipset-providers/00000000-0000-0000-0000-000000000000/unpublish", nil, publishUser.Tokens.AccessToken, "unpublish-missing", "req-unpublish-missing"); res.Code != http.StatusNotFound {
		t.Fatalf("missing provider unpublish = %d: %s", res.Code, res.Body.String())
	}
	env.server.ConfigureChipsetManifestFetcher(nil)
	if res := chipsetRequest(env.router, http.MethodPost, "/v1/admin/chipset-providers/"+created.Provider.ID+"/refresh", nil, publishUser.Tokens.AccessToken, "refresh-no-fetcher", "req-refresh-no-fetcher"); res.Code != http.StatusBadRequest {
		t.Fatalf("refresh without configured fetcher = %d: %s", res.Code, res.Body.String())
	}
	env.server.ConfigureChipsetManifestFetcher(fetcher)
	second := chipsetRequest(env.router, http.MethodPost, "/v1/admin/chipset-providers", map[string]any{"name": "Ameba no snapshot", "manifest_url": "https://provider.example.com/amebapro2-no-snapshot.json"}, editUser.Tokens.AccessToken, "create-no-snapshot", "req-create-no-snapshot")
	if second.Code != http.StatusCreated {
		t.Fatalf("create no-snapshot provider = %d: %s", second.Code, second.Body.String())
	}
	var secondCreated struct {
		Provider model.ChipsetProvider `json:"provider"`
	}
	decodeRecorder(t, second, &secondCreated)
	fetcher.set(ChipsetManifestFetchResult{NotModified: true}, nil)
	noSnapshot := chipsetRequest(env.router, http.MethodPost, "/v1/admin/chipset-providers/"+secondCreated.Provider.ID+"/publish", nil, publishUser.Tokens.AccessToken, "publish-no-snapshot", "req-publish-no-snapshot")
	if noSnapshot.Code != http.StatusConflict {
		t.Fatalf("provider publish without snapshot = %d: %s", noSnapshot.Code, noSnapshot.Body.String())
	}
	fetcher.set(integrationManifestResult("1.0.0"), nil)
	assertDeveloperChipsets(t, env.router, developer.Tokens.AccessToken, 0, "", false)

	publish := chipsetRequest(env.router, http.MethodPost, "/v1/admin/chipset-providers/"+created.Provider.ID+"/publish", nil, publishUser.Tokens.AccessToken, "publish-ameba", "req-publish")
	if publish.Code != http.StatusOK {
		t.Fatalf("publish-only publish = %d: %s", publish.Code, publish.Body.String())
	}
	assertDeveloperChipsets(t, env.router, developer.Tokens.AccessToken, 1, "1.0.0", false)
	detail := chipsetRequest(env.router, http.MethodGet, "/v1/developer/chipsets/9b4a572f-3e66-5ea6-a355-22f3221b8909", nil, developer.Tokens.AccessToken, "", "")
	if detail.Code != http.StatusOK || !bytes.Contains(detail.Body.Bytes(), []byte(`"version":"1.0.0"`)) || bytes.Contains(detail.Body.Bytes(), []byte("manifest_url")) {
		t.Fatalf("developer chipset detail = %d: %s", detail.Code, detail.Body.String())
	}
	var boardDetail struct {
		Chipset model.DeveloperChipset `json:"chipset"`
	}
	decodeRecorder(t, detail, &boardDetail)
	if boardDetail.Chipset.ICModel != "RTL8735B" || len(boardDetail.Chipset.Boards) != 1 || !reflect.DeepEqual(boardDetail.Chipset.Boards[0], validChipsetBoard()) || !reflect.DeepEqual(boardDetail.Chipset.SDKReleases[0].SupportedBoardKeys, []string{"amb82-mini"}) {
		t.Fatalf("database/API round-trip lost board fields: %#v", boardDetail.Chipset)
	}
	missingDetail := chipsetRequest(env.router, http.MethodGet, "/v1/developer/chipsets/missing", nil, developer.Tokens.AccessToken, "", "")
	if missingDetail.Code != http.StatusNotFound {
		t.Fatalf("missing developer chipset detail = %d: %s", missingDetail.Code, missingDetail.Body.String())
	}

	if res := chipsetRequest(env.router, http.MethodPatch, "/v1/admin/chipset-providers/"+created.Provider.ID, providerWriteBody(), editUser.Tokens.AccessToken, "edit-published", "req-edit-published"); res.Code != http.StatusConflict {
		t.Fatalf("edit published provider = %d: %s", res.Code, res.Body.String())
	}

	fetcher.set(integrationManifestResult("2.0.0"), nil)
	refresh := chipsetRequest(env.router, http.MethodPost, "/v1/admin/chipset-providers/"+created.Provider.ID+"/refresh", nil, publishUser.Tokens.AccessToken, "refresh-v2", "req-refresh-v2")
	if refresh.Code != http.StatusOK {
		t.Fatalf("manual refresh = %d: %s", refresh.Code, refresh.Body.String())
	}
	assertDeveloperChipsets(t, env.router, developer.Tokens.AccessToken, 1, "2.0.0", false)

	fetcher.set(ChipsetManifestFetchResult{}, errors.New("provider offline"))
	failed := chipsetRequest(env.router, http.MethodPost, "/v1/admin/chipset-providers/"+created.Provider.ID+"/refresh", nil, publishUser.Tokens.AccessToken, "refresh-failed", "req-refresh-failed")
	if failed.Code != http.StatusBadGateway || bytes.Contains(failed.Body.Bytes(), []byte("provider offline")) {
		t.Fatalf("failed refresh = %d: %s", failed.Code, failed.Body.String())
	}
	assertDeveloperChipsets(t, env.router, developer.Tokens.AccessToken, 1, "2.0.0", true)

	fetcher.set(ChipsetManifestFetchResult{NotModified: true}, nil)
	before := fetcher.callCount()
	refreshCtx, cancel := context.WithCancel(context.Background())
	env.server.RunChipsetProviderRefresh(refreshCtx, 0)
	done := make(chan struct{})
	go func() {
		env.server.RunChipsetProviderRefresh(refreshCtx, time.Millisecond)
		close(done)
	}()
	deadline := time.Now().Add(time.Second)
	refreshed := false
	for time.Now().Before(deadline) {
		var stale bool
		err := env.db.QueryRow(ctx, `SELECT stale FROM chipset_information_providers WHERE id::text = $1`, created.Provider.ID).Scan(&stale)
		if err == nil && !stale {
			refreshed = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if fetcher.callCount() == before {
		t.Fatal("background refresh did not fetch a published provider")
	}
	if !refreshed {
		t.Fatal("background refresh did not persist the successful refresh")
	}
	assertDeveloperChipsets(t, env.router, developer.Tokens.AccessToken, 1, "2.0.0", false)

	audit := chipsetRequest(env.router, http.MethodGet, "/v1/admin/audit-events?subject_type=chipset_information_provider", nil, admin.Tokens.AccessToken, "", "")
	if audit.Code != http.StatusOK || !bytes.Contains(audit.Body.Bytes(), []byte(`"request_id":"req-create"`)) || !bytes.Contains(audit.Body.Bytes(), []byte("chipset_provider.refresh_failed")) {
		t.Fatalf("provider audit evidence = %d: %s", audit.Code, audit.Body.String())
	}

	unpublish := chipsetRequest(env.router, http.MethodPost, "/v1/admin/chipset-providers/"+created.Provider.ID+"/unpublish", nil, publishUser.Tokens.AccessToken, "unpublish-ameba", "req-unpublish")
	if unpublish.Code != http.StatusOK {
		t.Fatalf("unpublish = %d: %s", unpublish.Code, unpublish.Body.String())
	}
	assertDeveloperChipsets(t, env.router, developer.Tokens.AccessToken, 0, "", false)
}

func TestChipsetDeveloperHandlersRejectBrandCloudSubject(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := &Server{}
	for name, handler := range map[string]func(*gin.Context){
		"list":   server.listDeveloperChipsets,
		"detail": server.getDeveloperChipset,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			requestContext, _ := gin.CreateTestContext(recorder)
			requestContext.Set("subjectType", auth.SubjectTypeBrandCloudUser)
			requestContext.Request = httptest.NewRequest(http.MethodGet, "/", nil)
			handler(requestContext)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestChipsetProviderHandlersSurfaceCanceledStoreOperations(t *testing.T) {
	env := newIntegrationEnv(t)
	env.server.ConfigureChipsetManifestFetcher(&integrationChipsetFetcher{})
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	requestContext := func(method, target string, body any) (*gin.Context, *httptest.ResponseRecorder) {
		var payload []byte
		if body != nil {
			payload, _ = json.Marshal(body)
		}
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(method, target, bytes.NewReader(payload)).WithContext(canceled)
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.Header.Set("Idempotency-Key", "canceled-operation")
		return c, recorder
	}

	for name, handler := range map[string]func(*gin.Context){
		"admin list":       env.server.listChipsetProviders,
		"developer list":   env.server.listDeveloperChipsets,
		"developer detail": env.server.getDeveloperChipset,
	} {
		t.Run(name, func(t *testing.T) {
			c, recorder := requestContext(http.MethodGet, "/", nil)
			handler(c)
			if recorder.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
			}
		})
	}

	c, recorder := requestContext(http.MethodPost, "/", providerWriteBody())
	env.server.createChipsetProvider(c)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("create status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if _, _, err := env.server.refreshChipsetProvider(canceled, "missing", "", "", ""); err == nil {
		t.Fatal("expected canceled refresh lookup to fail")
	}
	if result := env.server.auditChipsetProvider(canceled, "", "provider", "test", "failed", "", "", ""); result != "failed" {
		t.Fatalf("audit result = %q", result)
	}
}

func integrationManifestResult(version string) ChipsetManifestFetchResult {
	return ChipsetManifestFetchResult{
		ManifestVersion: "1",
		ManifestSHA256:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ETag:            `"` + version + `"`,
		Chipsets: []model.DeveloperChipset{{
			ID:          "9b4a572f-3e66-5ea6-a355-22f3221b8909",
			ChipsetKey:  "realtek-amebapro2",
			Vendor:      "Realtek",
			Name:        "AmebaPRO2",
			ICModel:     "RTL8735B",
			Boards:      []model.ChipsetBoard{validChipsetBoard()},
			SDKReleases: []model.ChipsetSDKRelease{{Name: "Ameba Arduino Pro2", Version: version, SupportedBoardKeys: []string{"amb82-mini"}, Recommended: true, SupportedModels: []string{}, Endpoints: []model.ChipsetEndpoint{{Type: "github", Title: "GitHub", URL: "https://github.com/Ameba-AIoT/ameba-arduino-pro2"}}}},
		}},
	}
}

func bindPlatformCapability(t *testing.T, ctx context.Context, st *store.Store, userID, roleName, permission string) {
	t.Helper()
	if _, err := st.CreateRole(ctx, store.RoleCreateInput{Name: roleName, ScopeType: store.ScopeTypePlatform}); err != nil {
		t.Fatal(err)
	}
	if err := st.BindRolePermission(ctx, roleName, permission, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRoleAssignment(ctx, store.RoleAssignmentCreateInput{RoleName: roleName, ActorType: store.ActorTypeUser, ActorID: userID, ScopeType: store.ScopeTypePlatform}); err != nil {
		t.Fatal(err)
	}
}

func providerWriteBody() map[string]any {
	return map[string]any{"name": "Ameba IoT", "manifest_url": "https://provider.example.com/amebapro2.json"}
}

func chipsetRequest(router *gin.Engine, method, path string, body any, token, idempotencyKey, requestID string) *httptest.ResponseRecorder {
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	if requestID != "" {
		req.Header.Set("X-Request-Id", requestID)
	}
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)
	return res
}

func assertDeveloperChipsets(t *testing.T, router *gin.Engine, token string, count int, version string, stale bool) {
	t.Helper()
	res := chipsetRequest(router, http.MethodGet, "/v1/developer/chipsets", nil, token, "", "")
	if res.Code != http.StatusOK {
		t.Fatalf("developer chipsets = %d: %s", res.Code, res.Body.String())
	}
	var body struct {
		Chipsets []model.DeveloperChipset `json:"chipsets"`
	}
	decodeRecorder(t, res, &body)
	if len(body.Chipsets) != count {
		t.Fatalf("developer chipsets count = %d, want %d: %s", len(body.Chipsets), count, res.Body.String())
	}
	if count > 0 && (body.Chipsets[0].SDKReleases[0].Version != version || body.Chipsets[0].Stale != stale) {
		t.Fatalf("developer chipset = %+v, want version=%s stale=%v", body.Chipsets[0], version, stale)
	}
}

func decodeRecorder(t *testing.T, res *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(res.Body.Bytes(), target); err != nil {
		t.Fatalf("decode response: %v: %s", err, res.Body.String())
	}
}
