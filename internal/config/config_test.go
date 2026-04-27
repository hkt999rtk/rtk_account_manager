package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnvSetsMissingValuesAndPreservesExistingEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte(`
# comment
DOTENV_ALPHA=from-file
DOTENV_BETA="quoted value"
DOTENV_GAMMA='single quoted'
export DOTENV_EXISTING=from-file
`), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("DOTENV_EXISTING", "from-env")
	if err := LoadDotEnv(path); err != nil {
		t.Fatal(err)
	}

	if got := os.Getenv("DOTENV_ALPHA"); got != "from-file" {
		t.Fatalf("expected DOTENV_ALPHA from file, got %q", got)
	}
	if got := os.Getenv("DOTENV_BETA"); got != "quoted value" {
		t.Fatalf("expected DOTENV_BETA to be unquoted, got %q", got)
	}
	if got := os.Getenv("DOTENV_GAMMA"); got != "single quoted" {
		t.Fatalf("expected DOTENV_GAMMA to be unquoted, got %q", got)
	}
	if got := os.Getenv("DOTENV_EXISTING"); got != "from-env" {
		t.Fatalf("expected existing env to win, got %q", got)
	}
}
