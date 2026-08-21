package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

// A per-run base-branch override (axi run --base) reaches every pipeline step as
// sctx.Repo.DefaultBranch. This test proves the rebase step honors it: a feature
// branched off a non-default branch ("dev") that is ahead of the repository's
// git-detected default ("main") must rebase onto origin/dev, picking up dev's
// later commits, rather than onto origin/main. It is the step-level guarantee
// behind the acceptance criterion "axi run --base dev rebases onto dev".
func TestRebaseStep_HonorsOverriddenDefaultBranch(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("base\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")
	gitCmd(t, dir, "push", "origin", "main")

	// dev diverges ahead of main with its own commit, then feature branches off
	// dev. dev is the active development branch the run wants to target.
	gitCmd(t, dir, "checkout", "-b", "dev")
	os.WriteFile(filepath.Join(dir, "dev.txt"), []byte("d1\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "dev commit 1")
	gitCmd(t, dir, "push", "origin", "dev")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("fix\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature fix")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	// dev advances again after feature branched. This commit is reachable only
	// through origin/dev, so its presence in the rebased HEAD proves the rebase
	// targeted dev and not main (main never saw it).
	gitCmd(t, dir, "checkout", "dev")
	os.WriteFile(filepath.Join(dir, "dev.txt"), []byte("d1\nd2\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "dev commit 2")
	gitCmd(t, dir, "push", "origin", "dev")
	gitCmd(t, dir, "checkout", "feature")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	// The per-run override: target dev, not the git-detected default "main".
	sctx.Repo.DefaultBranch = "dev"

	step := &RebaseStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != nil && outcome.NeedsApproval {
		t.Fatalf("expected a clean rebase onto origin/dev, got approval gate: %s", outcome.Findings)
	}

	headLog := gitCmd(t, dir, "log", "--oneline")
	if !strings.Contains(headLog, "dev commit 2") {
		t.Errorf("expected rebased HEAD to include origin/dev's later commit; git log:\n%s", headLog)
	}
	if !strings.Contains(headLog, "feature fix") {
		t.Errorf("expected rebased HEAD to retain the feature commit; git log:\n%s", headLog)
	}

	status := gitStatusPorcelain(t, dir)
	if status != "" {
		t.Fatalf("expected clean worktree after rebase, got: %s", status)
	}
}
