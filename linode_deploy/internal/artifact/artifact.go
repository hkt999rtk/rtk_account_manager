package artifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type Manifest struct {
	Repo         string `json:"repo"`
	ArtifactName string `json:"artifact_name"`
	Version      string `json:"version"`
	SourceCommit string `json:"source_commit"`
	Bundle       string `json:"bundle"`
	ArtifactPath string `json:"artifact_path"`
	SHA256       string `json:"sha256"`
	CreatedAt    string `json:"created_at"`
}

type Bundle struct {
	Version    string
	Path       string
	BundleName string
	SHA256     string
	Manifest   Manifest
}

func VerifyLocal(path string, version string) (Bundle, error) {
	if path == "" {
		return Bundle{}, fmt.Errorf("release bundle is required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Bundle{}, fmt.Errorf("read release bundle: %w", err)
	}
	if err := verifyReleaseManifest(bytes.NewReader(raw), version); err != nil {
		return Bundle{}, err
	}
	sum := sha256.Sum256(raw)
	name := filepath.Base(path)
	expected := bundleName(version)
	if name != expected {
		return Bundle{}, fmt.Errorf("release bundle name %q must be %q", name, expected)
	}
	return Bundle{Version: version, Path: path, BundleName: name, SHA256: hex.EncodeToString(sum[:])}, nil
}

func VerifyManifestAndChecksum(manifestBytes, checksumBytes, bundleBytes []byte, version string) error {
	var m Manifest
	if err := json.Unmarshal(manifestBytes, &m); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	if err := ValidateManifest(m, version); err != nil {
		return err
	}
	fields := strings.Fields(string(checksumBytes))
	if len(fields) == 0 {
		return fmt.Errorf("empty checksum")
	}
	if fields[0] != m.SHA256 {
		return fmt.Errorf("checksum object does not match manifest")
	}
	sum := sha256.Sum256(bundleBytes)
	if hex.EncodeToString(sum[:]) != m.SHA256 {
		return fmt.Errorf("bundle checksum mismatch")
	}
	return verifyReleaseManifest(bytes.NewReader(bundleBytes), version)
}

func ValidateManifest(m Manifest, version string) error {
	expectedArtifactName := "rtk_account_manager"
	expectedBundle := objectBundleName(version)
	expectedPath := "releases/" + expectedArtifactName + "-" + version + "/" + expectedBundle
	switch {
	case version == "" || strings.EqualFold(version, "latest"):
		return fmt.Errorf("explicit release version is required")
	case m.Repo != "" && m.Repo != "hkt999rtk/rtk_account_manager":
		return fmt.Errorf("manifest repo %q must be hkt999rtk/rtk_account_manager", m.Repo)
	case m.ArtifactName != "" && m.ArtifactName != expectedArtifactName:
		return fmt.Errorf("manifest artifact_name %q must be %q", m.ArtifactName, expectedArtifactName)
	case m.Version != version:
		return fmt.Errorf("manifest version %q does not match requested release %q", m.Version, version)
	case m.Bundle != expectedBundle:
		return fmt.Errorf("manifest bundle %q must be %q", m.Bundle, expectedBundle)
	case m.ArtifactPath != expectedPath:
		return fmt.Errorf("manifest artifact_path %q must be %q", m.ArtifactPath, expectedPath)
	case m.SourceCommit == "":
		return fmt.Errorf("manifest source_commit is required")
	case m.SHA256 == "":
		return fmt.Errorf("manifest sha256 is required")
	case m.CreatedAt == "":
		return fmt.Errorf("manifest created_at is required")
	}
	return nil
}

func bundleName(version string) string {
	return "rtk_account_manager-" + version + ".tar.gz"
}

func objectBundleName(version string) string {
	return version + ".tar.gz"
}

func verifyReleaseManifest(r io.Reader, version string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("open release bundle: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read release bundle: %w", err)
		}
		if filepath.Base(h.Name) != "release-manifest.txt" && filepath.Base(h.Name) != "manifest.txt" {
			continue
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			return fmt.Errorf("read release manifest: %w", err)
		}
		for _, line := range strings.Split(string(content), "\n") {
			k, v, ok := strings.Cut(line, "=")
			if ok && strings.TrimSpace(k) == "version" {
				if strings.TrimSpace(v) != version {
					return fmt.Errorf("release manifest version %q does not match requested release %q", strings.TrimSpace(v), version)
				}
				return nil
			}
		}
		return fmt.Errorf("release manifest missing version")
	}
	return fmt.Errorf("release bundle missing release-manifest.txt")
}
