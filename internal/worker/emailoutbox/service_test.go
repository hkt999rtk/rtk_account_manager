package emailoutbox

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"rtk_account_manager/internal/emaildelivery"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

type fakeStore struct {
	items       []model.EmailOutbox
	transitions []store.EmailOutboxTransitionInput
}

func (f *fakeStore) ClaimEmailOutboxReady(context.Context, time.Time, time.Time, int) ([]model.EmailOutbox, error) {
	return f.items, nil
}

func (f *fakeStore) TransitionEmailOutbox(_ context.Context, in store.EmailOutboxTransitionInput) (bool, error) {
	f.transitions = append(f.transitions, in)
	return true, nil
}

type fakeSender struct {
	err      error
	messages []emaildelivery.Message
}

func (f *fakeSender) Send(_ context.Context, message emaildelivery.Message) error {
	f.messages = append(f.messages, message)
	return f.err
}

func TestServiceSendsAndClearsPayload(t *testing.T) {
	now := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	cipher := testCipher(t)
	item := encryptedItem(t, cipher, now, nil)
	repository := &fakeStore{items: []model.EmailOutbox{item}}
	sender := &fakeSender{}
	service := NewService(repository, cipher, emaildelivery.Renderer{
		From: "no-reply@realtekconnect.com", BaseURL: "https://example.com", Now: func() time.Time { return now },
	}, sender, Options{Now: func() time.Time { return now }, Jitter: func(time.Duration) time.Duration { return 0 }})
	stats, err := service.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sent != 1 || len(sender.messages) != 1 || len(repository.transitions) != 1 {
		t.Fatalf("unexpected result: stats=%+v sent=%d transitions=%d", stats, len(sender.messages), len(repository.transitions))
	}
	transition := repository.transitions[0]
	if transition.Status != model.EmailOutboxStatusSent || !transition.ClearPayload || transition.SentAt == nil {
		t.Fatalf("unexpected transition: %+v", transition)
	}
}

func TestServiceRetriesTransientAndDeadLettersPermanent(t *testing.T) {
	now := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	cipher := testCipher(t)
	for _, test := range []struct {
		name       string
		sendErr    error
		wantStatus model.EmailOutboxStatus
	}{
		{"transient", &emaildelivery.DeliveryError{Err: errors.New("timeout"), Transient: true}, model.EmailOutboxStatusRetrying},
		{"permanent", &emaildelivery.DeliveryError{Err: errors.New("rejected"), Transient: false}, model.EmailOutboxStatusDeadLettered},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := &fakeStore{items: []model.EmailOutbox{encryptedItem(t, cipher, now, nil)}}
			service := NewService(repository, cipher, emaildelivery.Renderer{
				From: "no-reply@realtekconnect.com", BaseURL: "https://example.com",
			}, &fakeSender{err: test.sendErr}, Options{
				Now: func() time.Time { return now }, RetryBase: 30 * time.Second,
				Jitter: func(time.Duration) time.Duration { return 0 },
			})
			if _, err := service.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := repository.transitions[0].Status; got != test.wantStatus {
				t.Fatalf("status = %s, want %s", got, test.wantStatus)
			}
			if test.wantStatus == model.EmailOutboxStatusRetrying && !repository.transitions[0].AvailableAt.Equal(now.Add(30*time.Second)) {
				t.Fatalf("retry time = %s", repository.transitions[0].AvailableAt)
			}
		})
	}
}

func TestServiceExpiresWithoutSending(t *testing.T) {
	now := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	expired := now.Add(-time.Second)
	cipher := testCipher(t)
	repository := &fakeStore{items: []model.EmailOutbox{encryptedItem(t, cipher, now, &expired)}}
	sender := &fakeSender{}
	service := NewService(repository, cipher, emaildelivery.Renderer{}, sender, Options{Now: func() time.Time { return now }})
	stats, err := service.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Expired != 1 || len(sender.messages) != 0 || !repository.transitions[0].ClearPayload {
		t.Fatalf("unexpected expired result: stats=%+v sends=%d transition=%+v", stats, len(sender.messages), repository.transitions[0])
	}
}

func testCipher(t *testing.T) *emaildelivery.Cipher {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = 7
	}
	cipher, err := emaildelivery.NewCipher(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	return cipher
}

func encryptedItem(t *testing.T, cipher *emaildelivery.Cipher, now time.Time, expiresAt *time.Time) model.EmailOutbox {
	t.Helper()
	nonce, ciphertext, err := cipher.Encrypt(emaildelivery.Payload{RecipientEmail: "user@example.com", Token: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	return model.EmailOutbox{
		ID: "outbox-1", MessageType: "email_verification",
		PayloadNonce: nonce, PayloadCiphertext: ciphertext,
		Status: model.EmailOutboxStatusSending, ExpiresAt: expiresAt,
		CreatedAt: now,
	}
}
