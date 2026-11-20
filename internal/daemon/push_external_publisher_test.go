package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

func TestLoadRecoveredConfig_ExternalPublisher(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return nil })
	repo, submitted := setupTestGitRepo(t, p, d, "external-publisher-restart")
	gitCmd(t, repo.WorkingPath, "remote", "add", "origin", p.RepoDir(repo.ID))
	const policy = `allow_repo_commands: true
push:
  external_publisher:
    paths: [.shipping-gate.json]
    reason: The external gate owns publication.
    remedy: Recover and use the external gate.
`
	trusted := commitTrustedRepoConfig(t, repo, policy)
	gitCmd(t, repo.WorkingPath, "checkout", "-b", "feature", submitted)
	run, err := d.InsertRun(repo.ID, "feature", submitted, submitted)
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewRunManager(d, p, nil)
	cfg, err := mgr.loadRecoveredConfig(context.Background(), run, repo, repo.WorkingPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TrustedConfigSHA != trusted || cfg.Push.ExternalPublisher == nil || cfg.Push.ExternalPublisher.Reason != "The external gate owns publication." {
		t.Fatalf("restart lost trusted policy: %+v", cfg.Push)
	}

	// A malformed current trusted declaration cannot become "unset" on restart.
	gitCmd(t, repo.WorkingPath, "checkout", "-b", "malformed-policy", trusted)
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, ".no-mistakes.yaml"), []byte("push: {external_publisher: {paths: []}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "commit", "-am", "malformed trusted policy")
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/main")
	gitCmd(t, repo.WorkingPath, "checkout", "feature")
	if _, err := mgr.loadRecoveredConfig(context.Background(), run, repo, repo.WorkingPath); err == nil || !strings.Contains(err.Error(), "push.external_publisher") {
		t.Fatalf("restart accepted malformed trusted policy: %v", err)
	}
}
