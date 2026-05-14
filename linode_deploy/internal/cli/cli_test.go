package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlanCommandPrintsDeploymentPlan(t *testing.T) {
	var out bytes.Buffer
	err := Run(context.Background(), []string{"plan", "--config", filepath.Join("..", "..", "configs", "account-manager-staging.yaml")}, &out, &out)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "account-manager-staging.realtekconnect.com") || !strings.Contains(out.String(), "workers disabled by default") {
		t.Fatalf("unexpected plan output:\n%s", out.String())
	}
}

func TestDeployDryRunRequiresReleaseAndSecrets(t *testing.T) {
	var out bytes.Buffer
	err := Run(context.Background(), []string{
		"deploy",
		"--config", filepath.Join("..", "..", "configs", "account-manager-staging.yaml"),
		"--stack", "account-manager-staging",
		"--release", "v1.2.3",
		"--secrets-file", filepath.Join("testdata", "account-manager-staging.env"),
		"--dry-run",
		"--skip-infra-apply",
	}, &out, &out)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, want := range []string{"dry-run deploy account-manager-staging release v1.2.3", "rtk-account-manager-migrate.service", "redacted env inventory"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("deploy output missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "db-secret") || strings.Contains(out.String(), "access-secret") {
		t.Fatalf("deploy output leaked secret:\n%s", out.String())
	}
}

func TestDeployRejectsLatestRelease(t *testing.T) {
	var out bytes.Buffer
	err := Run(context.Background(), []string{
		"deploy",
		"--config", filepath.Join("..", "..", "configs", "account-manager-staging.yaml"),
		"--stack", "account-manager-staging",
		"--release", "latest",
		"--secrets-file", filepath.Join("testdata", "account-manager-staging.env"),
		"--dry-run",
	}, &out, &out)
	if err == nil || !strings.Contains(err.Error(), "latest is not allowed") {
		t.Fatalf("expected latest rejection, got %v", err)
	}
}
