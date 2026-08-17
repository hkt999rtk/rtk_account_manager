package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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

func TestPaymentSimulatorSignatureRejectsMalformedAndAcceptsAuthenticBody(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	body := []byte(`{"setup_session_id":"test"}`)
	if validPaymentSimulatorSignature(secret, body, "not-hex") {
		t.Fatal("malformed signatures must be rejected")
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	if !validPaymentSimulatorSignature(secret, body, hex.EncodeToString(mac.Sum(nil))) {
		t.Fatal("authentic signatures must be accepted")
	}
}

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

func TestBillingDebitSourceValidationIsStrict(t *testing.T) {
	for _, valid := range []string{"pricing-engine", "billing.v2", "invoice_worker"} {
		if !validBillingDebitSource(valid) {
			t.Fatalf("source %q should be valid", valid)
		}
	}
	for _, invalid := range []string{"", "Pricing-Engine", "billing source", strings.Repeat("a", 65)} {
		if validBillingDebitSource(invalid) {
			t.Fatalf("source %q should be invalid", invalid)
		}
	}
}

func TestHostedPaymentSetupUsesStableSafeInputs(t *testing.T) {
	consent := consentRequest{Accepted: true, TextVersion: " payment-method-v1 ", TextSHA256: strings.Repeat("A", 64), Locale: " zh-TW "}
	first, err := paymentSetupRequestSHA256("account-1", "fake", consent, "user", "user-1")
	if err != nil {
		t.Fatal(err)
	}
	consent.TextVersion = "payment-method-v1"
	consent.TextSHA256 = strings.Repeat("a", 64)
	consent.Locale = "zh-TW"
	second, err := paymentSetupRequestSHA256("account-1", "fake", consent, "user", "user-1")
	if err != nil || first != second || len(first) != 64 {
		t.Fatalf("first=%q second=%q err=%v", first, second, err)
	}
	for _, valid := range []string{"https://payments.example/setup/one", "https://payments.example/setup?id=opaque"} {
		if !validHostedPaymentURL(valid) {
			t.Fatalf("URL %q should be valid", valid)
		}
	}
	for _, invalid := range []string{"http://payments.example/setup", "https://user:secret@payments.example/setup", "https://payments.example/setup#token", " https://payments.example/setup", "not-a-url"} {
		if validHostedPaymentURL(invalid) {
			t.Fatalf("URL %q should be invalid", invalid)
		}
	}
	for _, valid := range []string{"customer-opaque-1", strings.Repeat("x", 1024)} {
		if !validOpaqueProviderReference(valid) {
			t.Fatalf("opaque reference %q should be valid", valid)
		}
	}
	for _, invalid := range []string{"", " reference-with-whitespace", strings.Repeat("x", 1025)} {
		if validOpaqueProviderReference(invalid) {
			t.Fatalf("opaque reference %q should be invalid", invalid)
		}
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
