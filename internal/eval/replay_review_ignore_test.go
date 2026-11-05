package eval

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestReplayConfigPreservesTrustedReviewIgnores(t *testing.T) {
	ctx := context.Background()
	workDir := t.TempDir()
	mustGit(t, ctx, workDir, "init")
	mustGit(t, ctx, workDir, "config", "user.name", "test")
	mustGit(t, ctx, workDir, "config", "user.email", "test@example.com")
	configPath := filepath.Join(workDir, ".no-mistakes.yaml")
	if err := os.WriteFile(configPath, []byte("ignore_patterns: ['vendor/**']\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustGit(t, ctx, workDir, "add", ".")
	mustGit(t, ctx, workDir, "commit", "-m", "trusted configuration")
	trustedSHA := mustGit(t, ctx, workDir, "rev-parse", "HEAD")
	if err := os.WriteFile(configPath, []byte("ignore_patterns: ['*']\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Case{Dir: t.TempDir()}
	c.TrustedConfigSHA = trustedSHA
	dir := filepath.Join(c.Dir, "config")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"global.yaml": "{}\n", "repo-config.yaml": "ignore_patterns: ['*']\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := replayConfig(ctx, c, workDir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.IgnorePatterns, []string{"*"}) || !slices.Equal(cfg.TrustedIgnorePatterns, []string{"vendor/**"}) {
		t.Fatalf("replay lost ignore provenance: pushed=%v trusted=%v", cfg.IgnorePatterns, cfg.TrustedIgnorePatterns)
	}
	c.TrustedConfigSHA = "missing-trusted-ref"
	if _, err := replayConfig(ctx, c, workDir); err == nil {
		t.Fatal("replay accepted unreadable trusted ignore provenance")
	}
}
