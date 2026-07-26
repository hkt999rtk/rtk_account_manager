package emaildelivery

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSendMailHTTPClientSendsOpenAPIRequest(t *testing.T) {
	var received sendMailHTTPRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/send" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer opaque-token" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&received); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"sent"}`))
	}))
	defer server.Close()

	client, err := NewSendMailHTTPClient(SendMailHTTPConfig{
		BaseURL: server.URL, BearerToken: "opaque-token", Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	message := Message{
		EnvelopeFrom: "ignored@example.com",
		Recipient:    "user@example.com",
		Subject:      "Verify",
		Text:         "plain",
		HTML:         "<p>html</p>",
		Data:         []byte("ignored MIME"),
	}
	if err := client.Send(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if len(received.To) != 1 || received.To[0] != "user@example.com" ||
		received.Subject != "Verify" || received.Text != "plain" || received.HTML != "<p>html</p>" {
		t.Fatalf("request = %+v", received)
	}
}

func TestSendMailHTTPClientClassifiesResponses(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		transient bool
		wantError bool
	}{
		{"accepted", http.StatusAccepted, `{"status":"sent"}`, false, false},
		{"bad request", http.StatusBadRequest, `{"error":"bad"}`, false, true},
		{"unauthorized", http.StatusUnauthorized, `{"error":"unauthorized"}`, false, true},
		{"forbidden", http.StatusForbidden, `{"error":"forbidden"}`, false, true},
		{"timeout", http.StatusRequestTimeout, `{"error":"timeout"}`, true, true},
		{"rate limited", http.StatusTooManyRequests, `{"error":"limited"}`, true, true},
		{"SMTP upstream failed", http.StatusBadGateway, `{"error":"upstream"}`, true, true},
		{"invalid accepted response", http.StatusAccepted, `{"status":"unknown"}`, false, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			client, err := NewSendMailHTTPClient(SendMailHTTPConfig{
				BaseURL: server.URL, BearerToken: "must-not-appear", Timeout: time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			err = client.Send(context.Background(), Message{
				Recipient: "user@example.com", Subject: "subject", Text: "body",
			})
			if (err != nil) != test.wantError {
				t.Fatalf("error = %v, wantError=%v", err, test.wantError)
			}
			if err != nil {
				if IsTransient(err) != test.transient {
					t.Fatalf("transient=%v, want %v: %v", IsTransient(err), test.transient, err)
				}
				if strings.Contains(err.Error(), "must-not-appear") || strings.Contains(err.Error(), test.body) {
					t.Fatalf("error leaked credential or response body: %v", err)
				}
			}
		})
	}
}

func TestSendMailHTTPClientValidatesConfigurationAndMessage(t *testing.T) {
	for _, config := range []SendMailHTTPConfig{
		{},
		{BaseURL: "https://user:pass@example.com", BearerToken: "token"},
		{BaseURL: "https://example.com/send", BearerToken: "token"},
		{BaseURL: "https://example.com"},
	} {
		if _, err := NewSendMailHTTPClient(config); err == nil {
			t.Fatalf("invalid config accepted: %+v", config)
		}
	}
	client, err := NewSendMailHTTPClient(SendMailHTTPConfig{
		BaseURL: "https://example.com", BearerToken: "token",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("invalid message reached HTTP transport")
			return nil, nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range []Message{
		{Subject: "subject", Text: "body"},
		{Recipient: "user@example.com", Text: "body"},
		{Recipient: "user@example.com", Subject: "subject"},
	} {
		if err := client.Send(context.Background(), message); err == nil || IsTransient(err) {
			t.Fatalf("invalid message error = %v", err)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}
