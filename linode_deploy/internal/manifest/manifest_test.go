package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAccountManagerStagingManifest(t *testing.T) {
	path := filepath.Join("..", "..", "configs", "account-manager-staging.yaml")
	m, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.Stack != "account-manager-staging" {
		t.Fatalf("unexpected stack: %q", m.Stack)
	}
	acct := m.Instances["account-manager"]
	if acct.PrivateIP != "10.42.1.20" || acct.Networking != "vpc_only" {
		t.Fatalf("unexpected account-manager instance: %+v", acct)
	}
	if m.Deploy.Domain != "account-manager-staging.realtekconnect.com" {
		t.Fatalf("unexpected domain: %q", m.Deploy.Domain)
	}
	if !m.WorkersDisabledByDefault() {
		t.Fatal("workers should be disabled unless explicitly enabled")
	}
}

func TestManifestRejectsUnsafeAccountManagerShape(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	body := strings.Replace(validManifest(t), "private_ip: 10.42.1.20\n    firewall_profile", "private_ip: 10.42.1.20\n    networking: public_plus_vpc\n    firewall_profile", 1)
	body = strings.Replace(body, "networking: vpc_only\n    private_ip: 10.42.1.20", "private_ip: 10.42.1.20", 1)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "account-manager networking") {
		t.Fatalf("expected account-manager networking validation error, got %v", err)
	}
}

func TestManifestRequiresNoLatestRelease(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	body := strings.Replace(validManifest(t), "release: v0.1.0", "release: latest", 1)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "explicit release") {
		t.Fatalf("expected explicit release validation error, got %v", err)
	}
}

func validManifest(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "configs", "account-manager-staging.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
