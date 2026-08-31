package api

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"rtk_account_manager/internal/billingbootstrap"
	"rtk_account_manager/internal/store"
)

func TestIntegrationBillingCloudCreationAcrossRealServices(t *testing.T) {
	binary, dir, dsn := os.Getenv("TEST_BILLING_BOOTSTRAP_BINARY"), os.Getenv("TEST_BILLING_BOOTSTRAP_DIR"), os.Getenv("TEST_BILLING_BOOTSTRAP_DSN")
	if binary == "" || dir == "" || dsn == "" {
		t.Skip("requires independent Billing API binary and isolated Billing database")
	}
	if !filepath.IsAbs(binary) || !filepath.IsAbs(dir) {
		t.Fatal("fixture paths must be absolute")
	}
	env := newIntegrationEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, binary, "-test.run=^TestBillingCloudCreationWithExternalAccountManager$", "-test.timeout=45s")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TEST_BILLING_EXTERNAL_BOOTSTRAP=1")
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		input.Close()
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		input.Close()
		output.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		input.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Error("Billing fixture failed", err)
			}
		case <-time.After(5 * time.Second):
			cancel()
			<-done
			t.Error("Billing fixture did not stop")
		}
	})
	encoder, decoder := json.NewEncoder(input), json.NewDecoder(output)
	const token = "isolated-real-am-billing-creation-credential"
	if encoder.Encode(map[string]string{"DSN": dsn, "Token": token}) != nil {
		t.Fatal("fixture configuration")
	}
	var ready struct{ URL string }
	if decoder.Decode(&ready) != nil || ready.URL == "" {
		t.Fatal("Billing fixture did not start")
	}
	client, err := billingbootstrap.New(billingbootstrap.Config{BaseURL: ready.URL, Token: token})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := billingbootstrap.NewWorker(env.store, client, nil)
	if err != nil {
		t.Fatal(err)
	}
	// This uses the real signup transaction (without delivering an email in this
	// local protocol test); HTTP signup/email activation have their own coverage.
	created, err := env.store.SignupDeveloper(ctx, store.DeveloperSignupInput{Email: "billing-cross-service@example.com", PasswordHash: "isolated-hash", OrganizationName: "Cross-service billing cloud", SignupPendingVerification: true})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := worker.RunOnce(ctx); err != nil || n != 0 {
		t.Fatal("lost commit response acknowledged", n, err)
	}
	var eventID, leaseID string
	var delivered bool
	if err := env.db.QueryRow(ctx, `SELECT event_id::text,COALESCE(lease_id::text,''),delivered_at IS NOT NULL FROM brand_cloud_billing_creation_outbox WHERE cloud_id=$1`, created.BrandCloud.ID).Scan(&eventID, &leaseID, &delivered); err != nil || delivered || leaseID != "" {
		t.Fatal("lost reply did not retain retry", err)
	}
	type witness struct {
		AccountID, OwnerID, Evidence       string
		Accounts, Periods, Audits, Balance int64
	}
	inspect := func() witness {
		t.Helper()
		if encoder.Encode(map[string]string{"CloudID": created.BrandCloud.ID}) != nil {
			t.Fatal("send witness query")
		}
		var out witness
		if decoder.Decode(&out) != nil {
			t.Fatal("read witness")
		}
		return out
	}
	first := inspect()
	if first.OwnerID != created.User.ID || first.Accounts != 1 || first.Periods != 1 || first.Audits != 1 || first.Balance != 0 {
		t.Fatalf("partial Billing commit: %+v", first)
	}
	if _, err := env.db.Exec(ctx, `UPDATE brand_cloud_billing_creation_outbox SET available_at=clock_timestamp()-interval '1 second' WHERE event_id=$1`, eventID); err != nil {
		t.Fatal(err)
	}
	worker, err = billingbootstrap.NewWorker(store.New(env.db), client, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := worker.RunOnce(ctx); err != nil || n != 1 {
		t.Fatal("restart did not recover receipt", n, err)
	}
	if second := inspect(); second != first {
		t.Fatal("receipt replay changed Billing state")
	}
	var accountID, digest string
	if err := env.db.QueryRow(ctx, `SELECT billing_account_id::text,receipt_sha256,delivered_at IS NOT NULL FROM brand_cloud_billing_creation_outbox WHERE event_id=$1`, eventID).Scan(&accountID, &digest, &delivered); err != nil || !delivered || accountID != first.AccountID || digest != first.Evidence {
		t.Fatal("AM did not bind exact Billing receipt", err)
	}
	if n, err := worker.RunOnce(ctx); err != nil || n != 0 {
		t.Fatal("delivered event repeated", n, err)
	}
}
