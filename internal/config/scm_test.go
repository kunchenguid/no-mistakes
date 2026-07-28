package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadGlobal_SCM(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := []byte("scm:\n  cli_wrapper: [\"op\", \"plugin\", \"run\", \"--\"]\n  gh_config_dir: /gh-config\n")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(cfg.SCM.CLIWrapper, " "), "op plugin run --"; got != want {
		t.Fatalf("scm.cli_wrapper = %q, want %q", got, want)
	}
	if cfg.SCM.GHConfigDir != "/gh-config" {
		t.Fatalf("scm.gh_config_dir = %q, want %q", cfg.SCM.GHConfigDir, "/gh-config")
	}
}

func TestLoadGlobal_SCMDefaultsEmpty(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("log_level: info\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatal(err)
	}
	// Empty must stay empty: a default wrapper would silently change which
	// credentials every gh call authenticates with.
	if len(cfg.SCM.CLIWrapper) != 0 || cfg.SCM.GHConfigDir != "" {
		t.Fatalf("scm = %+v, want zero value", cfg.SCM)
	}
}
