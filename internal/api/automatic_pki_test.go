package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/store"
)

type automaticJobs struct {
	job            store.DevicePKIJob
	active, locked bool
	receipt        store.DevicePKIReceipt
	code           string
}

func (j *automaticJobs) ClaimDevicePKI(context.Context) (store.DevicePKIJob, error) {
	return j.job, nil
}
func (j *automaticJobs) WithDevicePKIContext(_ context.Context, _ store.DevicePKIJob, run func() error) (bool, error) {
	if !j.active {
		return false, nil
	}
	j.locked = true
	defer func() { j.locked = false }()
	return true, run()
}
func (j *automaticJobs) FinishDevicePKI(_ context.Context, _ store.DevicePKIJob, r store.DevicePKIReceipt, code string) error {
	j.receipt, j.code = r, code
	return r.Validate()
}

func TestAutomaticWorkerStopsOnCancellation(t *testing.T) {
	s := &Server{pkiClient: &pkiProxy{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	finished := make(chan struct{})
	go func() {
		s.RunDevicePKI(ctx, &automaticJobs{})
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("automatic Device PKI worker did not stop after cancellation")
	}
}

func TestAutomaticWorkerUsesBoundMachineIdentityAndClassifiesReceipts(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer := auth.RS256TokenSigner{Signer: key, PublicKey: &key.PublicKey}
	s := New(nil, auth.NewServiceWithSigners(signer, signer, time.Minute, time.Hour))
	j := &automaticJobs{active: true, job: store.DevicePKIJob{CloudID: "576aaf1f-a694-4873-be1a-76dfcf501a31", ProductID: "c8267396-dfed-4b6e-841c-6e5bb018191a", OperationID: "95c24e7d-80be-4128-9365-7a37ca594438"}}
	status, body, calls := 200, `{"pki_status":"pending"}`, 0
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if !j.locked {
			t.Error("request escaped active business lock")
		}
		raw, _ := io.ReadAll(r.Body)
		token, err := jwt.Parse(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), func(*jwt.Token) (any, error) { return &key.PublicKey, nil }, jwt.WithValidMethods([]string{"RS256"}))
		if err != nil || !token.Valid {
			t.Error("invalid service assertion")
			w.WriteHeader(403)
			return
		}
		claims := token.Claims.(jwt.MapClaims)
		hash := sha256.Sum256(raw)
		if claims["sub"] != "service:account-manager:pki-provisioner" || !reflect.DeepEqual(claims["roles"], []any{"pki_provisioner"}) || claims["mfa"] != false || claims["auth_time"] != float64(0) || claims["active_context"] != true || claims["environment"] != "dev" || claims["body_sha256"] != hex.EncodeToString(hash[:]) || claims["idempotency_key"] != j.job.OperationID || r.Header.Get("Idempotency-Key") != j.job.OperationID || r.Method != "POST" || r.URL.Path != "/v1/pki/automatic" {
			t.Error("machine request not bound to its job, context and exact privilege")
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	defer controller.Close()
	base, _ := url.Parse(controller.URL)
	s.pkiClient = &pkiProxy{base: base, client: controller.Client(), environment: "dev"}
	for _, tc := range []struct {
		name             string
		status           int
		body, want, code string
	}{
		{"wait", 200, `{"pki_status":"pending"}`, "pending", ""},
		{"temporary", 503, `provider confidential error`, "pending", "provider_unavailable"},
		{"permission", 403, `provider confidential error`, "failed", "reconciliation_required"},
		{"malformed", 200, `{"pki_status":"ready","issuer_id":"bad"}`, "failed", "reconciliation_required"},
		{"ready", 200, `{"pki_status":"ready","issuer_id":"6baf97c3-7d83-4ec2-85b0-9e092f5a4df0","operation_id":"8db4183b-4d3a-4960-adb3-796b576f14fb"}`, "ready", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body = tc.status, tc.body
			if err := s.runDevicePKIOnce(context.Background(), j); err != nil || j.receipt.Status != tc.want || j.code != tc.code {
				t.Fatal("wrong durable receipt", j.receipt.Status, j.code, err)
			}
		})
	}
	j.active = false
	before := calls
	if err := s.runDevicePKIOnce(context.Background(), j); err != nil || calls != before || j.receipt.Status != "cancelled" {
		t.Fatal("inactive Cloud/Product reached provider", err)
	}
	controller.Close()
	j.active = true
	if err := s.runDevicePKIOnce(context.Background(), j); err != nil || j.receipt.Status != "pending" || j.code != "provider_unavailable" {
		t.Fatal("outage not retryable", err)
	}
}
