package fake

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"rtk_account_manager/internal/payment"
)

func TestChargeUsesStableMerchantOrderIdempotency(t *testing.T) {
	provider := New("secret")
	provider.QueueCharge(
		Outcome{Result: payment.ProviderResult{State: payment.PaymentIntentStateSucceeded, ProviderTransactionReference: "txn-1"}},
		Outcome{Result: payment.ProviderResult{State: payment.PaymentIntentStateFailed}},
	)
	request := payment.ChargeRequest{MerchantOrderReference: "order-1"}
	first, err := provider.Charge(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := provider.Charge(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first.State != payment.PaymentIntentStateSucceeded || second.ProviderTransactionReference != "txn-1" {
		t.Fatalf("idempotent results first=%+v second=%+v", first, second)
	}
	if len(provider.ChargeCalls()) != 2 {
		t.Fatalf("charge calls=%d", len(provider.ChargeCalls()))
	}
}

func TestWebhookHMACAndStrictPayload(t *testing.T) {
	provider := New("secret")
	event := payment.WebhookEvent{
		ProviderEventReference: "event-1", MerchantOrderReference: "order-1",
		ProviderTransactionReference: "txn-1", AmountMinor: 50000,
		Currency: payment.CurrencyTWD, State: payment.PaymentIntentStateSucceeded,
		EventType: "payment.succeeded", ProviderCode: "00",
	}
	body, err := WebhookBody(event)
	if err != nil {
		t.Fatal(err)
	}
	header := http.Header{"X-Fake-Signature": []string{SignWebhook("secret", body)}}
	verified, err := provider.VerifyWebhook(context.Background(), payment.WebhookRequest{Header: header, Body: body})
	if err != nil || verified.ProviderEventReference != event.ProviderEventReference {
		t.Fatalf("verified=%+v err=%v", verified, err)
	}
	if _, err := provider.VerifyWebhook(context.Background(), payment.WebhookRequest{Header: http.Header{"X-Fake-Signature": []string{"00"}}, Body: body}); err == nil {
		t.Fatal("invalid signature should fail")
	}
	malformed := append(body[:len(body)-1], []byte(`,"unexpected":true}`)...)
	malformedHeader := http.Header{"X-Fake-Signature": []string{SignWebhook("secret", malformed)}}
	_, err = provider.VerifyWebhook(context.Background(), payment.WebhookRequest{Header: malformedHeader, Body: malformed})
	var providerErr *payment.ProviderError
	if !errors.As(err, &providerErr) || providerErr.Kind != payment.ProviderErrorInvalidRequest {
		t.Fatalf("malformed payload error=%v", err)
	}
}

func TestUnsupportedFakeProviderOperationsAreExplicit(t *testing.T) {
	provider := New("secret")
	if _, err := provider.CreateSetup(context.Background(), payment.SetupRequest{}); !errors.Is(err, payment.ErrProviderUnsupported) {
		t.Fatalf("setup err=%v", err)
	}
	if _, err := provider.Refund(context.Background(), payment.RefundRequest{}); !errors.Is(err, payment.ErrProviderUnsupported) {
		t.Fatalf("refund err=%v", err)
	}
	if !provider.Capabilities(context.Background()).MerchantInitiatedCharge {
		t.Fatal("fake provider should advertise merchant-initiated charge")
	}
}
