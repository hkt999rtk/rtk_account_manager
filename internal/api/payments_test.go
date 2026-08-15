package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/payment"
	"rtk_account_manager/internal/paymentstore"
	"rtk_account_manager/internal/store"
)

func TestWritePaymentErrorNormalizesCustomerSafeFailures(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"not found", paymentstore.ErrNotFound, http.StatusNotFound, "not_found"},
		{"amount", payment.ErrInvalidAmount, http.StatusBadRequest, "PAYMENT_AMOUNT_INVALID"},
		{"currency", payment.ErrInvalidCurrency, http.StatusBadRequest, "PAYMENT_CURRENCY_UNSUPPORTED"},
		{"inactive method", payment.ErrPaymentMethodInactive, http.StatusConflict, "PAYMENT_METHOD_INACTIVE"},
		{"capability", payment.ErrCapabilityUnsupported, http.StatusConflict, "PAYMENT_CAPABILITY_UNSUPPORTED"},
		{"provider", payment.ErrProviderUnsupported, http.StatusConflict, "PAYMENT_CAPABILITY_UNSUPPORTED"},
		{"idempotency", paymentstore.ErrIdempotencyConflict, http.StatusConflict, "PAYMENT_INTENT_CONFLICT"},
		{"closed", paymentstore.ErrAccountClosed, http.StatusConflict, "BILLING_ACCOUNT_SUSPENDED"},
		{"conflict", paymentstore.ErrConflict, http.StatusConflict, "AUTO_TOPUP_POLICY_CONFLICT"},
		{"policy", payment.ErrInvalidPolicy, http.StatusConflict, "AUTO_TOPUP_POLICY_CONFLICT"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			requestContext, _ := gin.CreateTestContext(recorder)
			writePaymentError(requestContext, test.err)
			if recorder.Code != test.status || !strings.Contains(recorder.Body.String(), test.code) {
				t.Fatalf("status/body = %d/%s, want %d containing %s", recorder.Code, recorder.Body.String(), test.status, test.code)
			}
		})
	}
}

func TestUnavailablePaymentReferenceResolverFailsClosed(t *testing.T) {
	if _, err := (unavailablePaymentReferenceResolver{}).ResolveMethodReference(context.Background(), []byte("ciphertext")); err == nil {
		t.Fatal("expected HTTP process reference resolution to remain unavailable")
	}
}

func TestConfigurePaymentsRequiresStore(t *testing.T) {
	if err := New(nil, nil).ConfigurePayments(PaymentAPIOptions{}); err == nil {
		t.Fatal("expected missing payment store to fail configuration")
	}
}

func TestPaymentActorUsesAuthenticatedSubjectIdentity(t *testing.T) {
	brandContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	brandContext.Set("subjectType", auth.SubjectTypeBrandCloudUser)
	brandContext.Set("brandCloudUserID", "brand-user-1")
	if got := paymentActorType(brandContext); got != store.ActorTypeBrandCloudUser {
		t.Fatalf("brand actor type=%q", got)
	}
	if got := paymentActorID(brandContext); got != "brand-user-1" {
		t.Fatalf("brand actor id=%q", got)
	}

	userContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	userContext.Set("subjectType", auth.SubjectTypePlatformUser)
	userContext.Set("userID", "user-1")
	if got := paymentActorType(userContext); got != store.ActorTypeUser {
		t.Fatalf("user actor type=%q", got)
	}
	if got := paymentActorID(userContext); got != "user-1" {
		t.Fatalf("user actor id=%q", got)
	}
}

func TestWritePaymentErrorFallsBackWithoutLeakingDetails(t *testing.T) {
	recorder := httptest.NewRecorder()
	requestContext, _ := gin.CreateTestContext(recorder)
	writePaymentError(requestContext, errors.New("provider-secret-detail"))
	if recorder.Code != http.StatusInternalServerError || strings.Contains(recorder.Body.String(), "provider-secret-detail") {
		t.Fatalf("unsafe fallback status/body = %d/%s", recorder.Code, recorder.Body.String())
	}
}
