package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type cliSyncFixture struct {
	local, remote, base, old, pushed, runID string
}

func newCLISyncFixture(t *testing.T) cliSyncFixture {
	t.Helper()
	nmHome := filepath.Join(t.TempDir(), "nm-home")
	t.Setenv("NM_HOME", nmHome)
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	cliGit(t, root, "init", "--bare", remote)
	local := filepath.Join(root, "operator")
	cliGit(t, root, "init", "-b", "main", local)
	cliGit(t, local, "config", "user.name", "Test")
	cliGit(t, local, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(local, "file.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, local, "add", "file.txt")
	cliGit(t, local, "commit", "-m", "base")
	base := cliGit(t, local, "rev-parse", "HEAD")
	cliGit(t, local, "checkout", "-b", "feature/sync")
	if err := os.WriteFile(filepath.Join(local, "file.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, local, "commit", "-am", "feature")
	old := cliGit(t, local, "rev-parse", "HEAD")

	pipeline := filepath.Join(root, "pipeline")
	cliGit(t, root, "clone", local, pipeline)
	cliGit(t, pipeline, "config", "user.name", "Pipeline")
	cliGit(t, pipeline, "config", "user.email", "pipeline@example.com")
	cliGit(t, pipeline, "checkout", "feature/sync")
	if err := os.WriteFile(filepath.Join(pipeline, "fix.txt"), []byte("fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, pipeline, "add", "fix.txt")
	cliGit(t, pipeline, "commit", "-m", "pipeline fix")
	pushed := cliGit(t, pipeline, "rev-parse", "HEAD")
	cliGit(t, pipeline, "push", remote, "HEAD:refs/heads/feature/sync")

	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	registeredRoot, err := git.FindGitRoot(local)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.InsertRepo(registeredRoot, remote, "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature/sync", old, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunHeadSHA(run.ID, pushed); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPushBinding(run.ID, db.PushBinding{HeadSHA: pushed, TargetKind: "upstream", TargetFingerprint: branchsync.TargetFingerprint(remote), Ref: "refs/heads/feature/sync"}); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	chdir(t, local)
	return cliSyncFixture{local: local, remote: remote, base: base, old: old, pushed: pushed, runID: run.ID}
}

func rewriteCLIPipelineHead(t *testing.T, f *cliSyncFixture, commits []pipelineCommitForCLI) {
	t.Helper()
	root := filepath.Dir(f.local)
	pipeline := filepath.Join(root, "pipeline-rewrite")
	cliGit(t, root, "clone", f.local, pipeline)
	cliGit(t, pipeline, "config", "user.name", "Pipeline")
	cliGit(t, pipeline, "config", "user.email", "pipeline@example.com")
	cliGit(t, pipeline, "checkout", "-B", "feature/sync", f.base)
	for _, commit := range commits {
		for name, contents := range commit.files {
			if err := os.WriteFile(filepath.Join(pipeline, name), []byte(contents), 0o644); err != nil {
				t.Fatal(err)
			}
			cliGit(t, pipeline, "add", name)
		}
		cliGit(t, pipeline, "commit", "-m", commit.message)
	}
	f.pushed = cliGit(t, pipeline, "rev-parse", "HEAD")
	cliGit(t, pipeline, "push", "--force", f.remote, "HEAD:refs/heads/feature/sync")

	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.UpdateRunHeadSHA(f.runID, f.pushed); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPushBinding(f.runID, db.PushBinding{HeadSHA: f.pushed, TargetKind: "upstream", TargetFingerprint: branchsync.TargetFingerprint(f.remote), Ref: "refs/heads/feature/sync"}); err != nil {
		t.Fatal(err)
	}
}

type pipelineCommitForCLI struct {
	message string
	files   map[string]string
}

func TestSyncHelpAndReferenceExposeGuardedModes(t *testing.T) {
	human := newSyncCmd()
	agent := newAxiSyncCmd()
	for name, content := range map[string]string{"human help": human.Long, "axi help": agent.Long} {
		for _, want := range []string{"fast-forward", "clean", "push", "equivalent", "reset semantics"} {
			if !strings.Contains(content, want) {
				t.Errorf("%s missing %q: %s", name, want, content)
			}
		}
	}
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "src", "content", "docs", "reference", "cli.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## no-mistakes sync", "## no-mistakes axi sync", "no-mistakes axi sync --check"} {
		if !strings.Contains(string(doc), want) {
			t.Errorf("CLI reference missing %q", want)
		}
	}
}

func TestAxiSyncCheckAndApplyReturnFullStructuredState(t *testing.T) {
	f := newCLISyncFixture(t)
	fetchHeadPath := filepath.Join(f.local, ".git", "FETCH_HEAD")
	fetchHeadBefore, _ := os.ReadFile(fetchHeadPath)
	out, err := executeCmd("axi", "sync", "--check")
	if err != nil {
		t.Fatalf("check: %v\n%s", err, out)
	}
	for _, want := range []string{"branch_sync:", "state: behind", "safety: safe_fast_forward", "freshness: live", f.old, f.pushed, "refs/heads/feature/sync", "command: no-mistakes axi sync"} {
		if !strings.Contains(out, want) {
			t.Errorf("check missing %q:\n%s", want, out)
		}
	}
	fetchHeadAfter, _ := os.ReadFile(fetchHeadPath)
	if !bytes.Equal(fetchHeadBefore, fetchHeadAfter) {
		t.Fatal("explicit check mutated FETCH_HEAD")
	}
	out, err = executeCmd("axi", "sync")
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	for _, want := range []string{"state: synchronized", "changed: true", "relation: equal"} {
		if !strings.Contains(out, want) {
			t.Errorf("apply missing %q:\n%s", want, out)
		}
	}
	if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != f.pushed {
		t.Fatalf("HEAD = %s", got)
	}
}

func TestAxiSyncEquivalentDivergedCheckAndApply(t *testing.T) {
	f := newCLISyncFixture(t)
	rewriteCLIPipelineHead(t, &f, []pipelineCommitForCLI{
		{message: "feature rebased", files: map[string]string{"file.txt": "feature\n"}},
		{message: "pipeline doc", files: map[string]string{"doc.txt": "pipeline doc\n"}},
	})

	out, err := executeCmd("axi", "sync", "--check")
	if err != nil {
		t.Fatalf("check: %v\n%s", err, out)
	}
	for _, want := range []string{"state: diverged", "safety: safe_equivalent_advance", "relation: diverged", "command: no-mistakes axi sync"} {
		if !strings.Contains(out, want) {
			t.Errorf("check missing %q:\n%s", want, out)
		}
	}

	out, err = executeCmd("axi", "sync")
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	for _, want := range []string{"state: synchronized", "changed: true", "relation: equal"} {
		if !strings.Contains(out, want) {
			t.Errorf("apply missing %q:\n%s", want, out)
		}
	}
	if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != f.pushed {
		t.Fatalf("HEAD = %s, want %s", got, f.pushed)
	}
	if got := cliGit(t, f.local, "rev-parse", "refs/no-mistakes/sync-anchor/"+f.runID); got != f.old {
		t.Fatalf("anchor = %s, want %s", got, f.old)
	}
}

func TestAxiSyncBlockedDirtyUsesExitOneAndStructuredError(t *testing.T) {
	f := newCLISyncFixture(t)
	if err := os.WriteFile(filepath.Join(f.local, "dirty.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := executeCmd("axi", "sync")
	var ee *exitError
	if err == nil || !asExitError(err, &ee) || ee.code != 1 {
		t.Fatalf("error = %#v", err)
	}
	for _, want := range []string{"state: dirty", "safety: blocked_dirty", "error:", "command: git status", f.old, f.pushed} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != f.old {
		t.Fatal("HEAD changed")
	}
}

func TestHumanSyncRequiresConfirmationOutsideTTY(t *testing.T) {
	f := newCLISyncFixture(t)
	previous := syncInteractive
	syncInteractive = func() bool { return false }
	t.Cleanup(func() { syncInteractive = previous })
	out, err := executeCmd("sync")
	if err == nil {
		t.Fatalf("expected refusal:\n%s", out)
	}
	if !strings.Contains(out, "Re-run with `no-mistakes sync --yes`") {
		t.Fatalf("output:\n%s", out)
	}
	if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != f.old {
		t.Fatal("HEAD changed")
	}

	out, err = executeCmd("sync", "--yes")
	if err != nil {
		t.Fatalf("--yes: %v\n%s", err, out)
	}
	if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != f.pushed {
		t.Fatal("HEAD not synchronized")
	}
}

func TestHumanSyncTTYConfirmationAppliesOnlyAfterYes(t *testing.T) {
	f := newCLISyncFixture(t)
	previous := syncInteractive
	syncInteractive = func() bool { return true }
	t.Cleanup(func() { syncInteractive = previous })
	cmd := newRootCmd()
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetIn(strings.NewReader("yes\n"))
	cmd.SetArgs([]string{"sync"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("interactive sync: %v\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "Apply this exact strict fast-forward?") {
		t.Fatalf("confirmation plan was not shown:\n%s", buf.String())
	}
	if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != f.pushed {
		t.Fatal("confirmed sync did not advance HEAD")
	}
}

func TestSyncTelemetryIsOneBoundedPrivacySafeEvent(t *testing.T) {
	f := newCLISyncFixture(t)
	recorder := &telemetryRecorder{}
	restore := telemetry.SetDefaultForTesting(recorder)
	defer restore()
	if out, err := executeCmd("axi", "sync", "--check"); err != nil {
		t.Fatalf("sync check: %v\n%s", err, out)
	}
	event := recorder.find("command", "command", "axi-sync")
	if event == nil {
		t.Fatal("missing explicit sync command event")
	}
	count := 0
	for _, candidate := range recorder.events {
		if candidate.name == "command" && candidate.fields["command"] == "axi-sync" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("sync command events = %d, want 1", count)
	}
	serialized := fmt.Sprint(event.fields)
	for _, forbidden := range []string{f.old, f.pushed, f.local, f.remote, "feature/sync", "refs/heads/feature/sync"} {
		if strings.Contains(serialized, forbidden) {
			t.Fatalf("telemetry leaked %q: %s", forbidden, serialized)
		}
	}
	for _, want := range []string{"surface:axi", "mode:check", "state_before:behind", "target_kind:upstream", "result:noop"} {
		if !strings.Contains(serialized, want) {
			t.Errorf("telemetry missing %q: %s", want, serialized)
		}
	}
}

func TestAxiStatusCachedBranchSyncDoesNotFetch(t *testing.T) {
	f := newCLISyncFixture(t)
	fetchHead := filepath.Join(f.local, ".git", "FETCH_HEAD")
	before, _ := os.ReadFile(fetchHead)
	out, err := executeCmd("axi", "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	if !strings.Contains(out, "branch_sync:") || !strings.Contains(out, "freshness: pipeline_push") || !strings.Contains(out, "safety: refresh_required") {
		t.Fatalf("cached state missing:\n%s", out)
	}
	after, _ := os.ReadFile(fetchHead)
	if !bytes.Equal(before, after) {
		t.Fatal("passive status mutated FETCH_HEAD")
	}
}

type cliRecoverFixture struct {
	local, gate, submitted, preserved string
}

// newCLIRecoverFixture reproduces the stranded custody state end to end for
// the CLI surface: a cancelled pre-push run whose pipeline fix commit exists
// only in the repo's local gate at <NM_HOME>/repos/<id>.git, while the
// operator worktree sits at the submitted head with no push binding.
func newCLIRecoverFixture(t *testing.T) cliRecoverFixture {
	t.Helper()
	nmHome := filepath.Join(t.TempDir(), "nm-home")
	t.Setenv("NM_HOME", nmHome)
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	cliGit(t, root, "init", "--bare", remote)
	local := filepath.Join(root, "operator")
	cliGit(t, root, "init", "-b", "main", local)
	cliGit(t, local, "config", "user.name", "Test")
	cliGit(t, local, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(local, "file.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, local, "add", "file.txt")
	cliGit(t, local, "commit", "-m", "base")
	base := cliGit(t, local, "rev-parse", "HEAD")
	cliGit(t, local, "checkout", "-b", "feature/recover")
	if err := os.WriteFile(filepath.Join(local, "file.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, local, "commit", "-am", "feature")
	submitted := cliGit(t, local, "rev-parse", "HEAD")

	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	registeredRoot, err := git.FindGitRoot(local)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.InsertRepo(registeredRoot, remote, "main")
	if err != nil {
		t.Fatal(err)
	}

	gate := p.RepoDir(repo.ID)
	cliGit(t, filepath.Dir(gate), "init", "--bare", gate)
	cliGit(t, local, "push", gate, "refs/heads/feature/recover:refs/heads/feature/recover")
	pipeline := filepath.Join(root, "pipeline")
	cliGit(t, root, "clone", gate, pipeline)
	cliGit(t, pipeline, "config", "user.name", "Pipeline")
	cliGit(t, pipeline, "config", "user.email", "pipeline@example.com")
	cliGit(t, pipeline, "checkout", "feature/recover")
	if err := os.WriteFile(filepath.Join(pipeline, "fix.txt"), []byte("fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, pipeline, "add", "fix.txt")
	cliGit(t, pipeline, "commit", "-m", "no-mistakes(review): fix")
	preserved := cliGit(t, pipeline, "rev-parse", "HEAD")
	cliGit(t, pipeline, "push", "origin", "HEAD:refs/heads/feature/recover")

	run, err := database.InsertRun(repo.ID, "feature/recover", submitted, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunHeadSHA(run.ID, preserved); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunCancelled); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	chdir(t, local)
	return cliRecoverFixture{local: local, gate: gate, submitted: submitted, preserved: preserved}
}

func TestAxiSyncCheckSurfacesRecoveryForTerminalPrePushRun(t *testing.T) {
	f := newCLIRecoverFixture(t)
	out, err := executeCmd("axi", "sync", "--check")
	var ee *exitError
	if err == nil || !asExitError(err, &ee) || ee.code != 1 {
		t.Fatalf("stranded check should exit 1, got %#v\n%s", err, out)
	}
	for _, want := range []string{
		"state: pipeline_owned",
		"status: cancelled",
		"safety: blocked_pipeline_owned_recoverable",
		"code: recover_custody",
		"command: no-mistakes axi sync --recover",
		"no-mistakes rerun",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stranded check missing %q:\n%s", want, out)
		}
	}
	if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
		t.Fatal("check moved HEAD")
	}
}

func TestAxiSyncRecoverReturnsCustodyEndToEnd(t *testing.T) {
	f := newCLIRecoverFixture(t)
	out, err := executeCmd("axi", "sync", "--recover")
	if err != nil {
		t.Fatalf("recover: %v\n%s", err, out)
	}
	for _, want := range []string{"recovered: true", "state: custody_returned", "changed: true", "relation: equal", "no-mistakes axi run --intent"} {
		if !strings.Contains(out, want) {
			t.Errorf("recover output missing %q:\n%s", want, out)
		}
	}
	if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != f.preserved {
		t.Fatalf("HEAD = %s, want preserved %s", got, f.preserved)
	}
	// The recovered branch is no longer a blocked dead end.
	out, err = executeCmd("axi", "sync", "--check")
	if err != nil {
		t.Fatalf("post-recover check should exit 0: %v\n%s", err, out)
	}
	if !strings.Contains(out, "state: custody_returned") {
		t.Fatalf("post-recover check:\n%s", out)
	}
}

func TestAxiSyncRecoverDivergedRefusesThenKeepLocalSucceeds(t *testing.T) {
	f := newCLIRecoverFixture(t)
	if err := os.WriteFile(filepath.Join(f.local, "rescope.txt"), []byte("rescope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, f.local, "add", "rescope.txt")
	cliGit(t, f.local, "commit", "-m", "diverging rescope")
	divergedHead := cliGit(t, f.local, "rev-parse", "HEAD")

	out, err := executeCmd("axi", "sync", "--recover")
	var ee *exitError
	if err == nil || !asExitError(err, &ee) || ee.code != 1 {
		t.Fatalf("diverged recover should exit 1, got %#v\n%s", err, out)
	}
	for _, want := range []string{"safety: blocked_recover_diverged", "refs/no-mistakes/recover/", "--keep-local"} {
		if !strings.Contains(out, want) {
			t.Errorf("diverged refusal missing %q:\n%s", want, out)
		}
	}

	out, err = executeCmd("axi", "sync", "--recover", "--keep-local")
	if err != nil {
		t.Fatalf("keep-local recover: %v\n%s", err, out)
	}
	if !strings.Contains(out, "recovered: true") {
		t.Fatalf("keep-local output:\n%s", out)
	}
	if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != divergedHead {
		t.Fatal("keep-local moved the worktree")
	}
	if got := cliGit(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != divergedHead {
		t.Fatalf("gate branch = %s, want kept head %s", got, divergedHead)
	}
}

func TestSyncRecoverFlagValidation(t *testing.T) {
	newCLIRecoverFixture(t)
	for _, args := range [][]string{
		{"sync", "--check", "--recover"},
		{"sync", "--keep-local"},
		{"axi", "sync", "--check", "--recover"},
		{"axi", "sync", "--keep-local"},
	} {
		out, err := executeCmd(args...)
		var ee *exitError
		if err == nil || !asExitError(err, &ee) || ee.code != 2 {
			t.Errorf("%v should exit 2, got %#v\n%s", args, err, out)
		}
	}
}

func TestHumanSyncRecoverRequiresConfirmationOutsideTTY(t *testing.T) {
	f := newCLIRecoverFixture(t)
	previous := syncInteractive
	syncInteractive = func() bool { return false }
	t.Cleanup(func() { syncInteractive = previous })
	out, err := executeCmd("sync", "--recover")
	if err == nil {
		t.Fatalf("expected refusal:\n%s", out)
	}
	if !strings.Contains(out, "Re-run with `no-mistakes sync --recover --yes`") {
		t.Fatalf("output:\n%s", out)
	}
	if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
		t.Fatal("HEAD changed")
	}

	out, err = executeCmd("sync", "--recover", "--yes")
	if err != nil {
		t.Fatalf("--recover --yes: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Custody returned") {
		t.Fatalf("human recover output:\n%s", out)
	}
	if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != f.preserved {
		t.Fatal("HEAD not recovered to preserved head")
	}
}

func cliGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git.Run(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(out)
}

func asExitError(err error, target **exitError) bool {
	for err != nil {
		if typed, ok := err.(*exitError); ok {
			*target = typed
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
