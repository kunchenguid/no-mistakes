package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	toon "github.com/toon-format/toon-go"

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
	cliGit(t, root, "-c", "core.autocrlf=false", "clone", local, pipeline)
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
	cliGit(t, root, "-c", "core.autocrlf=false", "clone", f.local, pipeline)
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
	local, gate, submitted, preserved, runID string
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
	cliGit(t, root, "-c", "core.autocrlf=false", "clone", gate, pipeline)
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

// newCLIUnmovedAbortFixture reproduces the pre-push abort taken when delivery
// switches away from the pipeline: the gate holds the submitted branch, the
// run is terminal with head_sha still equal to submitted_head_sha, and no push
// provenance or custody stamp exists.
func newCLIUnmovedAbortFixture(t *testing.T) cliRecoverFixture {
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

	run, err := database.InsertRun(repo.ID, "feature/recover", submitted, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatusWithVerifiedHead(run.ID, types.RunCancelled, submitted); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	chdir(t, local)
	return cliRecoverFixture{local: local, gate: gate, submitted: submitted, preserved: submitted, runID: run.ID}
}

// TestAxiSurfacesReportUserOwnedReleaseAfterUnmovedPrePushAbort walks the
// public CLI surfaces through the pre-push-abort-with-unmoved-head shape:
// cancellation releases ownership, so status must identify the terminal run
// and report the exact branch and head as user-owned and immediately usable
// with no sync action, the check must be a non-blocking no-op instead of
// wrong-branch ambiguity, and a recovery request must be an idempotent no-op
// that mutates nothing.
func TestAxiSurfacesReportUserOwnedReleaseAfterUnmovedPrePushAbort(t *testing.T) {
	f := newCLIUnmovedAbortFixture(t)

	status, err := executeCmd("axi", "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, status)
	}
	var document struct {
		Run struct {
			Status string `toon:"status"`
		} `toon:"run"`
		BranchSync struct {
			State string `toon:"state"`
			Local struct {
				Branch string `toon:"branch"`
				Head   string `toon:"head"`
			} `toon:"local"`
			Pipeline struct {
				SubmittedHead string `toon:"submitted_head"`
				CurrentHead   string `toon:"current_head"`
			} `toon:"pipeline"`
			Relation string `toon:"relation"`
			Safety   string `toon:"safety"`
		} `toon:"branch_sync"`
	}
	if err := toon.UnmarshalString(status, &document); err != nil {
		t.Fatalf("decode status: %v\n%s", err, status)
	}
	if got, want := document.Run.Status, string(types.RunCancelled); got != want {
		t.Errorf("run status = %q, want %q", got, want)
	}
	if got, want := document.BranchSync.State, "user_owned"; got != want {
		t.Errorf("branch sync state = %q, want %q", got, want)
	}
	if got, want := document.BranchSync.Local.Branch, "feature/recover"; got != want {
		t.Errorf("local branch = %q, want %q", got, want)
	}
	if got, want := document.BranchSync.Local.Head, f.submitted; got != want {
		t.Errorf("local head = %q, want %q", got, want)
	}
	if got, want := document.BranchSync.Pipeline.SubmittedHead, f.submitted; got != want {
		t.Errorf("submitted head = %q, want %q", got, want)
	}
	if got, want := document.BranchSync.Pipeline.CurrentHead, f.submitted; got != want {
		t.Errorf("current head = %q, want %q", got, want)
	}
	if got, want := document.BranchSync.Relation, "equal"; got != want {
		t.Errorf("relation = %q, want %q", got, want)
	}
	if got, want := document.BranchSync.Safety, "user_owned"; got != want {
		t.Errorf("safety = %q, want %q", got, want)
	}
	for _, forbidden := range []string{"recover_custody", "next_action", "blocked_wrong_branch", "pipeline_owned"} {
		if strings.Contains(status, forbidden) {
			t.Errorf("released status must not contain %q:\n%s", forbidden, status)
		}
	}

	check, err := executeCmd("axi", "sync", "--check")
	if err != nil {
		t.Fatalf("released check must be a non-blocking no-op: %v\n%s", err, check)
	}
	if !strings.Contains(check, "state: user_owned") {
		t.Errorf("check missing user_owned state:\n%s", check)
	}
	if strings.Contains(check, "blocked_wrong_branch") || strings.Contains(check, "recover_custody") {
		t.Errorf("released check reports stale custody semantics:\n%s", check)
	}

	apply, err := executeCmd("axi", "sync")
	if err != nil {
		t.Fatalf("released sync must be a non-blocking no-op: %v\n%s", err, apply)
	}
	if !strings.Contains(apply, "state: user_owned") || !strings.Contains(apply, "changed: false") {
		t.Errorf("released sync output unexpected:\n%s", apply)
	}

	for round := 0; round < 2; round++ {
		recover, err := executeCmd("axi", "sync", "--recover")
		if err != nil {
			t.Fatalf("released recover round %d: %v\n%s", round, err, recover)
		}
		for _, want := range []string{"recovered: true", "state: user_owned", "changed: false"} {
			if !strings.Contains(recover, want) {
				t.Errorf("released recover round %d missing %q:\n%s", round, want, recover)
			}
		}
	}
	if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
		t.Fatalf("HEAD after released recover = %s, want %s", got, f.submitted)
	}
	if got := cliGit(t, f.local, "branch", "--show-current"); got != "feature/recover" {
		t.Fatalf("branch after released recover = %q", got)
	}

	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	run, err := database.GetRun(f.runID)
	if err != nil || run == nil {
		t.Fatalf("reload run: %#v, %v", run, err)
	}
	if run.CustodyReturnedAt != nil {
		t.Fatal("released recover stamped custody on the run row")
	}
}

type cliStaleUnpublishedFixture struct {
	local, gate, base, localHead, unpublished, pushed string
}

type olderTargetProvenance int

const (
	olderTargetMatching olderTargetProvenance = iota
	olderTargetConflicting
	olderTargetMissing
)

// newCLIStaleUnpublishedFixture builds the exact same-branch provenance race:
// an older terminal run owns U, while a newer run has an exact pushed
// descendant P. The gate and remote are both at P, and the clean worktree is
// still at L, the ancestor before U.
func newCLIStaleUnpublishedFixture(t *testing.T) cliStaleUnpublishedFixture {
	t.Helper()
	return newCLIStaleUnpublishedFixtureWithOptions(t, true, olderTargetMatching)
}

func newCLIStaleUnpublishedFixtureWithRelation(t *testing.T, pushedDescendant bool) cliStaleUnpublishedFixture {
	t.Helper()
	return newCLIStaleUnpublishedFixtureWithOptions(t, pushedDescendant, olderTargetMatching)
}

func newCLIStaleUnpublishedFixtureWithOptions(t *testing.T, pushedDescendant bool, provenance olderTargetProvenance) cliStaleUnpublishedFixture {
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
	localHead := cliGit(t, local, "rev-parse", "HEAD")

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

	pipeline := filepath.Join(root, "pipeline-old")
	cliGit(t, root, "-c", "core.autocrlf=false", "clone", local, pipeline)
	cliGit(t, pipeline, "config", "user.name", "Pipeline")
	cliGit(t, pipeline, "config", "user.email", "pipeline@example.com")
	cliGit(t, pipeline, "checkout", "feature/sync")
	if err := os.WriteFile(filepath.Join(pipeline, "older-fix.txt"), []byte("older\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, pipeline, "add", "older-fix.txt")
	cliGit(t, pipeline, "commit", "-m", "older pipeline fix")
	unpublished := cliGit(t, pipeline, "rev-parse", "HEAD")
	cliGit(t, pipeline, "push", gate, "HEAD:refs/heads/feature/sync")

	older, err := database.InsertRun(repo.ID, "feature/sync", localHead, base)
	if err != nil {
		t.Fatal(err)
	}
	if provenance != olderTargetMissing {
		fingerprint := branchsync.TargetFingerprint(remote)
		if provenance == olderTargetConflicting {
			fingerprint = branchsync.TargetFingerprint(remote + "-previous")
		}
		if err := database.UpdateRunPushBinding(older.ID, db.PushBinding{HeadSHA: localHead, TargetKind: "upstream", TargetFingerprint: fingerprint, Ref: "refs/heads/feature/sync"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.UpdateRunHeadSHA(older.ID, unpublished); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(older.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}

	// Ensure the database's created_at ordering cannot depend on ULID tie
	// breaking: the pushed rerun is definitively newer than the failed run.
	time.Sleep(1100 * time.Millisecond)
	newer := filepath.Join(root, "pipeline-new")
	pipelineSource := gate
	if !pushedDescendant {
		pipelineSource = local
	}
	cliGit(t, root, "-c", "core.autocrlf=false", "clone", pipelineSource, newer)
	cliGit(t, newer, "config", "user.name", "Pipeline")
	cliGit(t, newer, "config", "user.email", "pipeline@example.com")
	cliGit(t, newer, "checkout", "feature/sync")
	if err := os.WriteFile(filepath.Join(newer, "newer-fix.txt"), []byte("newer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, newer, "add", "newer-fix.txt")
	cliGit(t, newer, "commit", "-m", "newer pipeline fix")
	pushed := cliGit(t, newer, "rev-parse", "HEAD")
	cliGit(t, newer, "push", remote, "HEAD:refs/heads/feature/sync")
	gatePushArgs := []string{"push", gate, "HEAD:refs/heads/feature/sync"}
	if !pushedDescendant {
		gatePushArgs = []string{"push", "--force", gate, "HEAD:refs/heads/feature/sync"}
	}
	cliGit(t, newer, gatePushArgs...)

	latestSubmitted := unpublished
	if !pushedDescendant {
		latestSubmitted = localHead
	}
	latest, err := database.InsertRun(repo.ID, "feature/sync", latestSubmitted, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunHeadSHA(latest.ID, pushed); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPushBinding(latest.ID, db.PushBinding{HeadSHA: pushed, TargetKind: "upstream", TargetFingerprint: branchsync.TargetFingerprint(remote), Ref: "refs/heads/feature/sync"}); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(latest.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	chdir(t, local)
	return cliStaleUnpublishedFixture{local: local, gate: gate, base: base, localHead: localHead, unpublished: unpublished, pushed: pushed}
}

func TestAxiSyncOlderUnpublishedRunSelectsNewerPushedDescendant(t *testing.T) {
	f := newCLIStaleUnpublishedFixture(t)
	out, err := executeCmd("axi", "sync")
	if err != nil {
		t.Fatalf("descendant sync: %v\n%s", err, out)
	}
	for _, want := range []string{"state: synchronized", "status: completed", f.pushed, "changed: true"} {
		if !strings.Contains(out, want) {
			t.Errorf("descendant sync missing %q:\n%s", want, out)
		}
	}
	if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != f.pushed {
		t.Fatalf("sync HEAD = %s, want pushed descendant %s", got, f.pushed)
	}
	if got := cliGit(t, f.gate, "rev-parse", "refs/heads/feature/sync"); got != f.pushed {
		t.Fatalf("sync moved gate to %s, want unchanged pushed head %s", got, f.pushed)
	}
}

func TestAxiSyncOlderUnpublishedNonAncestorStillRefuses(t *testing.T) {
	f := newCLIStaleUnpublishedFixtureWithRelation(t, false)
	out, err := executeCmd("axi", "sync")
	var ee *exitError
	if err == nil || !asExitError(err, &ee) || ee.code != 1 {
		t.Fatalf("non-ancestor sync should refuse, got %#v\n%s", err, out)
	}
	for _, want := range []string{"state: pipeline_owned", "status: failed", f.unpublished} {
		if !strings.Contains(out, want) {
			t.Errorf("non-ancestor sync missing refusal evidence %q:\n%s", want, out)
		}
	}
	if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != f.localHead {
		t.Fatalf("refused non-ancestor sync moved HEAD to %s", got)
	}

	out, err = executeCmd("axi", "sync", "--recover")
	if err == nil || !asExitError(err, &ee) || ee.code != 1 {
		t.Fatalf("non-ancestor recovery should refuse, got %#v\n%s", err, out)
	}
	if !strings.Contains(out, "safety: blocked_recover_unverified_head") {
		t.Fatalf("non-ancestor recovery did not remain fail-closed:\n%s", out)
	}
	if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != f.localHead {
		t.Fatalf("refused non-ancestor recovery moved HEAD to %s", got)
	}
	if got := cliGit(t, f.gate, "rev-parse", "refs/heads/feature/sync"); got != f.pushed {
		t.Fatalf("refused non-ancestor recovery moved gate to %s", got)
	}
}

func TestAxiSyncOlderUnpublishedMissingGateDoesNotSupersede(t *testing.T) {
	f := newCLIStaleUnpublishedFixture(t)
	cliGit(t, f.gate, "update-ref", "-d", "refs/heads/feature/sync")

	out, err := executeCmd("axi", "sync")
	var ee *exitError
	if err == nil || !asExitError(err, &ee) || ee.code != 1 {
		t.Fatalf("missing-gate sync should refuse, got %#v\n%s", err, out)
	}
	for _, want := range []string{"state: pipeline_owned", "status: failed", f.unpublished} {
		if !strings.Contains(out, want) {
			t.Errorf("missing-gate sync missing refusal evidence %q:\n%s", want, out)
		}
	}
	if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != f.localHead {
		t.Fatalf("missing-gate refusal moved HEAD to %s", got)
	}
}

func TestAxiSyncOlderUnpublishedTargetProvenanceRefusesTakeover(t *testing.T) {
	for _, tc := range []struct {
		name       string
		provenance olderTargetProvenance
	}{
		{name: "conflicting", provenance: olderTargetConflicting},
		{name: "missing", provenance: olderTargetMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCLIStaleUnpublishedFixtureWithOptions(t, true, tc.provenance)
			out, err := executeCmd("axi", "sync")
			var ee *exitError
			if err == nil || !asExitError(err, &ee) || ee.code != 1 {
				t.Fatalf("%s target provenance should refuse, got %#v\n%s", tc.name, err, out)
			}
			for _, want := range []string{"state: pipeline_owned", "status: failed", f.unpublished} {
				if !strings.Contains(out, want) {
					t.Errorf("%s target provenance missing refusal evidence %q:\n%s", tc.name, want, out)
				}
			}
			if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != f.localHead {
				t.Fatalf("refused %s target provenance moved HEAD to %s", tc.name, got)
			}
			if got := cliGit(t, f.gate, "rev-parse", "refs/heads/feature/sync"); got != f.pushed {
				t.Fatalf("refused %s target provenance moved gate to %s", tc.name, got)
			}
		})
	}
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

func TestSyncServicesRejectInvalidGlobalRemoteTimeout(t *testing.T) {
	newCLISyncFixture(t)
	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.ConfigFile(), []byte("branch_sync_remote_timeout: \"0s\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	service, closeFn, err := openSyncService()
	if err == nil {
		closeFn()
		t.Fatal("openSyncService accepted an invalid branch sync timeout")
	}
	if service != nil || closeFn != nil {
		t.Fatal("openSyncService returned resources with its configuration error")
	}
	if !strings.Contains(err.Error(), "branch_sync_remote_timeout") || !strings.Contains(err.Error(), "duration must be positive") {
		t.Fatalf("openSyncService error = %v", err)
	}

	service, closeFn, err = branchsync.OpenCurrent()
	if err == nil {
		closeFn()
		t.Fatal("branchsync.OpenCurrent accepted an invalid branch sync timeout")
	}
	if service != nil || closeFn != nil {
		t.Fatal("branchsync.OpenCurrent returned resources with its configuration error")
	}
	if !strings.Contains(err.Error(), "branch_sync_remote_timeout") || !strings.Contains(err.Error(), "duration must be positive") {
		t.Fatalf("branchsync.OpenCurrent error = %v", err)
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
