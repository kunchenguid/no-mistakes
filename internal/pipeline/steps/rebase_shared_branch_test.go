package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Reproduces the incident: a worker assembled several tickets onto a shared
// integration branch as deliberate --no-ff merges on a specific base, then ran
// the pipeline. origin/<default> had meanwhile advanced onto a different
// lineage that does not contain that base. The rebase step rebased the shared
// branch onto that newer default, linearized the merges, dropped the recorded
// base from ancestry, and force-pushed the shared ref - validating a base
// nobody built on and discarding the integration history.
//
// The rebase step must refuse loudly, naming the branch, and must not rewrite
// or force-push it.
func TestRebaseStep_RefusesToRewriteSharedIntegrationBranch(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	os.WriteFile(filepath.Join(dir, "root.txt"), []byte("root\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "root")
	gitCmd(t, dir, "push", "origin", "main")

	// Integration base B: where the worker started assembling tickets.
	gitCmd(t, dir, "checkout", "-b", "integration")
	os.WriteFile(filepath.Join(dir, "integration_base.txt"), []byte("integration base\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "integration base")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	// Assemble four tickets as deliberate --no-ff merges onto base B.
	for _, ticket := range []string{"t1", "t2", "t3", "t4"} {
		gitCmd(t, dir, "checkout", "-b", ticket, baseSHA)
		os.WriteFile(filepath.Join(dir, ticket+".txt"), []byte(ticket+"\n"), 0o644)
		gitCmd(t, dir, "add", "-A")
		gitCmd(t, dir, "commit", "-m", ticket+" work")
		gitCmd(t, dir, "checkout", "integration")
		gitCmd(t, dir, "merge", "--no-ff", "-m", "merge "+ticket, ticket)
	}
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "integration") // pre-existing shared branch

	// origin/main advances onto a DIFFERENT lineage that does not contain B.
	gitCmd(t, dir, "checkout", "main")
	os.WriteFile(filepath.Join(dir, "engine_a.txt"), []byte("engine a\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "engine work a")
	os.WriteFile(filepath.Join(dir, "engine_b.txt"), []byte("engine b\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "engine work b")
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "integration")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/integration"
	sctx.Repo.UpstreamURL = upstream

	step := &RebaseStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("expected the rebase step to refuse rewriting the shared integration branch, got outcome=%#v", outcome)
	}
	if outcome.AutoFixable {
		t.Fatalf("rewriting a shared integration branch is never safely auto-fixable")
	}
	if !strings.Contains(outcome.Findings, "integration") {
		t.Fatalf("expected findings to name the branch, got: %s", outcome.Findings)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatalf("parse findings: %v", err)
	}
	if len(findings.Items) != 1 || findings.Items[0].Action != types.ActionAskUser {
		t.Fatalf("expected one ask-user finding, got: %#v", findings.Items)
	}

	// The branch must not have been rewritten: HEAD unchanged, merge commits
	// still present, recorded base still an ancestor.
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("branch HEAD was rewritten: got %s, want %s", got, headSHA)
	}
	merges := gitCmd(t, dir, "rev-list", "--merges", baseSHA+"..HEAD")
	if strings.TrimSpace(merges) == "" {
		t.Fatalf("expected the four merge commits to survive, but the range is linear")
	}
	// gitCmd fatals on a non-zero exit; --is-ancestor exits 0 only when true, so
	// this asserts the recorded base is still reachable from HEAD after refusal.
	gitCmd(t, dir, "merge-base", "--is-ancestor", baseSHA, "HEAD")
}

// Ordinary gating on a fresh single-author branch must still work: the guard
// must not fire, and the rebase onto the advanced default branch must apply.
func TestRebaseStep_OrdinaryFeatureBranchStillRebases(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")

	// Fresh linear single-author feature branch off the base.
	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature change")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	// Default advances on its own lineage (base stays an ancestor).
	gitCmd(t, dir, "checkout", "main")
	os.WriteFile(filepath.Join(dir, "other.txt"), []byte("main update\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "main update")
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "feature")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream

	step := &RebaseStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != nil && outcome.NeedsApproval {
		t.Fatalf("guard must not fire on an ordinary single-author feature branch, got findings: %s", outcome.Findings)
	}
	// Rebase onto the advanced default must have applied.
	headLog := gitCmd(t, dir, "log", "--oneline")
	if !strings.Contains(headLog, "main update") {
		t.Fatalf("expected HEAD to include the advanced default after rebase; git log:\n%s", headLog)
	}
}
