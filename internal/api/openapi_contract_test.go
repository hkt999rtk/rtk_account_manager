package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/gin-gonic/gin"
)

func TestIntegrationResponsesMatchOpenAPIContract(t *testing.T) {
	env := newIntegrationEnv(t)
	contract := newResponseContract(t)

	registered := registerUser(t, env.router, "contract-owner@example.com", "Contract Org")
	registerRes := performJSON(env.router, http.MethodPost, "/v1/auth/login", map[string]any{
		"email":    "contract-owner@example.com",
		"password": "password123",
	}, "")
	contract.validate(t, http.MethodPost, "/v1/auth/login", registerRes)

	changePasswordRes := performJSON(env.router, http.MethodPatch, "/v1/me/password", map[string]any{
		"current_password": "password123",
		"new_password":     "contract-password123",
	}, registered.Tokens.AccessToken)
	contract.validate(t, http.MethodPatch, "/v1/me/password", changePasswordRes)

	orgUpdateRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+registered.Organization.ID, map[string]any{
		"name": "Contract Org Updated",
	}, registered.Tokens.AccessToken)
	contract.validate(t, http.MethodPatch, "/v1/orgs/"+registered.Organization.ID, orgUpdateRes)

	deviceRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+registered.Organization.ID+"/devices", devicePayload("contract-device", "CONTRACT-001"), registered.Tokens.AccessToken)
	contract.validate(t, http.MethodPost, "/v1/orgs/"+registered.Organization.ID+"/devices", deviceRes)
	device := decodeBody[deviceBody](t, deviceRes)

	badDeviceRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+registered.Organization.ID+"/devices", map[string]any{
		"name":     "contract-device",
		"category": "invalid",
	}, registered.Tokens.AccessToken)
	contract.validate(t, http.MethodPost, "/v1/orgs/"+registered.Organization.ID+"/devices", badDeviceRes)

	provisionRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+registered.Organization.ID+"/devices/"+device.Device.ID+"/provision", map[string]any{
		"video_cloud_devid": "contract-video-1",
		"activity_id":       "contract-activity-1",
		"clip_public_key":   "contract-clip-key-1",
		"operation_id":      "contract-provision-op-1",
	}, registered.Tokens.AccessToken)
	contract.validate(t, http.MethodPost, "/v1/orgs/"+registered.Organization.ID+"/devices/"+device.Device.ID+"/provision", provisionRes)

	reusedProvisionRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+registered.Organization.ID+"/devices/"+device.Device.ID+"/provision", map[string]any{
		"video_cloud_devid": "contract-video-1",
		"activity_id":       "contract-activity-1",
		"clip_public_key":   "contract-clip-key-1",
		"operation_id":      "contract-provision-op-1",
	}, registered.Tokens.AccessToken)
	contract.validate(t, http.MethodPost, "/v1/orgs/"+registered.Organization.ID+"/devices/"+device.Device.ID+"/provision", reusedProvisionRes)

	provisioningRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+registered.Organization.ID+"/devices/"+device.Device.ID+"/provisioning", nil, registered.Tokens.AccessToken)
	contract.validate(t, http.MethodGet, "/v1/orgs/"+registered.Organization.ID+"/devices/"+device.Device.ID+"/provisioning", provisioningRes)

	deactivateRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+registered.Organization.ID+"/devices/"+device.Device.ID+"/deactivate", map[string]any{
		"operation_id": "contract-deactivate-op-1",
		"reason":       "contract-test",
	}, registered.Tokens.AccessToken)
	contract.validate(t, http.MethodPost, "/v1/orgs/"+registered.Organization.ID+"/devices/"+device.Device.ID+"/deactivate", deactivateRes)
}

type responseContract struct {
	doc    *openapi3.T
	router routers.Router
}

func newResponseContract(t *testing.T) responseContract {
	t.Helper()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile("../../openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	return responseContract{doc: doc, router: router}
}

func (c responseContract) validate(t *testing.T, method, path string, res *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest(method, "http://localhost:8080"+path, nil)
	req.Header.Set("Content-Type", "application/json")
	route, pathParams, err := c.router.FindRoute(req)
	if err != nil {
		t.Fatal(err)
	}

	input := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{
			Request:    req,
			PathParams: pathParams,
			Route:      route,
			Options: &openapi3filter.Options{
				AuthenticationFunc: openapi3filter.NoopAuthenticationFunc,
			},
		},
		Status: res.Code,
		Header: http.Header{
			"Content-Type": []string{gin.MIMEJSON},
		},
		Body: io.NopCloser(bytes.NewReader(res.Body.Bytes())),
		Options: &openapi3filter.Options{
			AuthenticationFunc: openapi3filter.NoopAuthenticationFunc,
		},
	}
	if err := openapi3filter.ValidateResponse(context.Background(), input); err != nil {
		t.Fatalf("%s %s response %d does not match OpenAPI contract: %v\nbody: %s", method, path, res.Code, err, res.Body.String())
	}
	if res.Code >= 400 && res.Body.Len() == 0 {
		t.Fatal(fmt.Errorf("error response %d must include a body", res.Code))
	}
}
