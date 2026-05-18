package artifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyLocalRequiresAccountManagerBundleContract(t *testing.T) {
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "rtk_account_manager-v1.2.3.tar.gz")
	writeBundle(t, bundlePath, "v1.2.3")

	bundle, err := VerifyLocal(bundlePath, "v1.2.3")
	if err != nil {
		t.Fatalf("VerifyLocal: %v", err)
	}
	if bundle.BundleName != "rtk_account_manager-v1.2.3.tar.gz" || bundle.Version != "v1.2.3" || bundle.SHA256 == "" {
		t.Fatalf("unexpected bundle: %+v", bundle)
	}
}

func TestValidateManifestRejectsWrongArtifactName(t *testing.T) {
	err := ValidateManifest(Manifest{
		Repo:         "hkt999rtk/rtk_account_manager",
		ArtifactName: "video_cloud",
		Version:      "v1.2.3",
		SourceCommit: "abc123",
		Bundle:       "video_cloud-v1.2.3.tar.gz",
		ArtifactPath: "releases/video_cloud-v1.2.3/v1.2.3.tar.gz",
		SHA256:       strings.Repeat("a", 64),
		CreatedAt:    "2026-05-14T00:00:00Z",
	}, "v1.2.3")
	if err == nil || !strings.Contains(err.Error(), "manifest artifact_name") {
		t.Fatalf("expected account-manager artifact validation error, got %v", err)
	}
}

func TestVerifyManifestAndChecksum(t *testing.T) {
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "rtk_account_manager-v1.2.3.tar.gz")
	writeBundle(t, bundlePath, "v1.2.3")
	raw, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	sha := hex.EncodeToString(sum[:])
	manifestBytes, err := json.Marshal(Manifest{
		Repo:         "hkt999rtk/rtk_account_manager",
		ArtifactName: "rtk_account_manager",
		Version:      "v1.2.3",
		SourceCommit: "abc123",
		Bundle:       "v1.2.3.tar.gz",
		ArtifactPath: "releases/rtk_account_manager-v1.2.3/v1.2.3.tar.gz",
		SHA256:       sha,
		CreatedAt:    "2026-05-14T00:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyManifestAndChecksum(manifestBytes, []byte(sha+"  v1.2.3.tar.gz\n"), raw, "v1.2.3"); err != nil {
		t.Fatalf("VerifyManifestAndChecksum: %v", err)
	}
}

func writeBundle(t *testing.T, path string, version string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	content := []byte("version=" + version + "\ngit_sha=abc123\n")
	if err := tw.WriteHeader(&tar.Header{Name: "rtk_account_manager-" + version + "/release-manifest.txt", Mode: 0o644, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}
