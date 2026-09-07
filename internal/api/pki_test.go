package api

import (
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestPKIStepUpPreservesNonceAndRequestsFreshAssurance(t *testing.T) {
	t.Setenv("PKI_OIDC_MFA_ACR", "urn:rtk:mfa")
	location, err := pkiStepUpLocation("https://idp.example/authorize?nonce=nonce&state=state&client_id=console")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("nonce") != "nonce" || q.Get("state") != "state" || q.Get("acr_values") != "urn:rtk:mfa" || q.Get("max_age") != "0" || q.Get("prompt") != "login" {
		t.Fatal("step-up changed identity binding or omitted assurance")
	}
	t.Setenv("PKI_OIDC_MFA_ACR", "")
	if _, err = pkiStepUpLocation(location); err == nil {
		t.Fatal("unconfigured MFA class accepted")
	}
}

func TestPKILastAdminGuardReturnsConflict(t *testing.T) {
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	writeStoreError(ctx, &pgconn.PgError{Code: "23514", ConstraintName: "platform_admin_preserved", Message: "internal database details"})
	if response.Code != 409 || !strings.Contains(response.Body.String(), "platform_admin_required") || strings.Contains(response.Body.String(), "internal database details") {
		t.Fatalf("unexpected response %d %s", response.Code, response.Body.String())
	}
}
