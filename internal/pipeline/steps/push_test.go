package steps

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

func setupGateMirror(t *testing.T, sctx *pipeline.StepContext) string {
	t.Helper()
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	gateDir := paths.WithRoot(t.TempDir()).RepoDir(sctx.Repo.ID)
	sctx.GateDir = gateDir
	if err := os.MkdirAll(filepath.Dir(gateDir), 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, filepath.Dir(gateDir), "init", "--bare", filepath.Base(gateDir))
	return gateDir
}

// TestPushStep_RefusesPostReviewClobberWithoutLaterPipelineCommit reproduces
// the end-user incident at the real push boundary. Review approved R, then an
// out-of-band reset replaced HEAD with divergent D and no pipeline-owned commit
// ran afterward. Push must refuse before changing the remote.
func TestPushStep_RefusesPostReviewClobberWithoutLaterPipelineCommit(t *testing.T) {
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir, baseSHA, submittedHead := setupGitRepo(t)
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "push", "origin", "feature")

	// R is the exact tree the completed review approved.
	if err := os.WriteFile(filepath.Join(dir, "reviewed.txt"), []byte("reviewed fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "reviewed fix")
	reviewedHead := gitCmd(t, dir, "rev-parse", "HEAD")

	// D is a divergent replacement built from the submitted head. There is no
	// later pipeline commit, so the existing commit-time continuity guard never
	// runs.
	gitCmd(t, dir, "reset", "--hard", submittedHead)
	if err := os.WriteFile(filepath.Join(dir, "unreviewed.txt"), []byte("unreviewed replacement\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "out-of-band replacement")
	clobberedHead := gitCmd(t, dir, "rev-parse", "HEAD")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, submittedHead, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	setupGateMirror(t, sctx)
	// Let the entry guard pass so this regression continues to isolate the
	// independent durable review-approved binding added at the push boundary.
	sctx.Run.HeadSHA = clobberedHead
	recordReviewApproval(t, sctx, reviewedHead)

	_, err := (&PushStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected push to refuse a divergent post-review HEAD replacement")
	}
	if !strings.Contains(err.Error(), "review-approved head") {
		t.Fatalf("expected review continuity error, got %v", err)
	}

	remoteHead := gitCmd(t, upstream, "rev-parse", "refs/heads/feature")
	if remoteHead != submittedHead {
		t.Fatalf("remote changed from %s to %s; clobbered head %s must not ship", submittedHead, remoteHead, clobberedHead)
	}
	if fileAtRef(t, upstream, "refs/heads/feature", "unreviewed.txt") {
		t.Fatal("remote contains the unreviewed replacement")
	}
	t.Logf(
		"review-approved=%s clobbered-HEAD=%s push-refused=%q remote-still=%s unreviewed-file-shipped=false",
		reviewedHead,
		clobberedHead,
		err,
		remoteHead,
	)
}

func TestAssertReviewApprovedPushHead(t *testing.T) {
	tests := []struct {
		name      string
		approval  string
		proposed  func(t *testing.T, dir, baseSHA, headSHA string) string
		wantError string
	}{
		{
			name: "equal",
			proposed: func(t *testing.T, dir, baseSHA, headSHA string) string {
				return headSHA
			},
		},
		{
			name: "legitimate descendant",
			proposed: func(t *testing.T, dir, baseSHA, headSHA string) string {
				if err := os.WriteFile(filepath.Join(dir, "docs.md"), []byte("docs\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, dir, "add", "-A")
				gitCmd(t, dir, "commit", "-m", "document approved change")
				return gitCmd(t, dir, "rev-parse", "HEAD")
			},
		},
		{
			name: "backward replacement",
			proposed: func(t *testing.T, dir, baseSHA, headSHA string) string {
				gitCmd(t, dir, "reset", "--hard", baseSHA)
				return baseSHA
			},
			wantError: "not an equal or descendant",
		},
		{
			name: "divergent replacement",
			proposed: func(t *testing.T, dir, baseSHA, headSHA string) string {
				gitCmd(t, dir, "reset", "--hard", baseSHA)
				if err := os.WriteFile(filepath.Join(dir, "other.txt"), []byte("other\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, dir, "add", "-A")
				gitCmd(t, dir, "commit", "-m", "divergent replacement")
				return gitCmd(t, dir, "rev-parse", "HEAD")
			},
			wantError: "not an equal or descendant",
		},
		{
			name:      "malformed approval",
			approval:  "HEAD",
			proposed:  func(t *testing.T, dir, baseSHA, headSHA string) string { return headSHA },
			wantError: "malformed",
		},
		{
			name:      "unreachable approval",
			approval:  strings.Repeat("a", 40),
			proposed:  func(t *testing.T, dir, baseSHA, headSHA string) string { return headSHA },
			wantError: "unreachable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, baseSHA, headSHA := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
			approval := tt.approval
			if approval == "" {
				approval = headSHA
			}
			recordReviewApproval(t, sctx, approval)
			proposed := tt.proposed(t, dir, baseSHA, headSHA)
			err := assertReviewApprovedPushHead(sctx, proposed)
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("expected continuity approval, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantError)
			}
		})
	}
}

func TestAssertReviewApprovedPushHead_RefusesMissingLegacyState(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	err := assertReviewApprovedPushHead(sctx, headSHA)
	if err == nil || !strings.Contains(err.Error(), "no durably recorded review-approved head") {
		t.Fatalf("expected missing legacy approval refusal, got %v", err)
	}
}

func TestAssertReviewApprovedPushHead_UsesStepScopedGit(t *testing.T) {
	dir, baseSHA, approvedHead := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, approvedHead, config.Commands{})
	recordReviewApproval(t, sctx, approvedHead)
	if err := os.WriteFile(filepath.Join(dir, "descendant.txt"), []byte("descendant\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "descendant")
	proposedHead := gitCmd(t, dir, "rev-parse", "HEAD")

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "git")
	logFile := filepath.Join(t.TempDir(), "git.log")
	sctx.Env = []string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"FAKE_CLI_MODE=git-passthrough",
		"FAKE_CLI_REAL_GIT=" + realGit,
		"FAKE_CLI_LOG=" + logFile,
	}

	if err := assertReviewApprovedPushHead(sctx, proposedHead); err != nil {
		t.Fatalf("expected descendant approval, got %v", err)
	}
	logBytes, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(logBytes)
	if !strings.Contains(logText, "rev-parse --verify") || !strings.Contains(logText, "merge-base --is-ancestor") {
		t.Fatalf("step-scoped git did not run both continuity checks; log:\n%s", logText)
	}
}

func TestPushStep_BindsRemoteAndDatabaseToVerifiedCommitWhenHEADMovesDuringPush(t *testing.T) {
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir, baseSHA, submittedHead := setupGitRepo(t)
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "push", "origin", "feature")

	if err := os.WriteFile(filepath.Join(dir, "approved.txt"), []byte("approved descendant\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "approved descendant")
	approvedHead := gitCmd(t, dir, "rev-parse", "HEAD")

	gitCmd(t, dir, "checkout", "--detach", baseSHA)
	if err := os.WriteFile(filepath.Join(dir, "replacement.txt"), []byte("replacement\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "concurrent replacement")
	replacementHead := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "checkout", "--detach", approvedHead)

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "git")
	t.Setenv("FAKE_CLI_MODE", "git-move-head-passthrough")
	t.Setenv("FAKE_CLI_REAL_GIT", realGit)
	t.Setenv("FAKE_CLI_REPLACEMENT_HEAD", replacementHead)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, submittedHead, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	setupGateMirror(t, sctx)
	recordReviewApproval(t, sctx, approvedHead)

	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if liveHead := gitCmd(t, dir, "rev-parse", "HEAD"); liveHead != replacementHead {
		t.Fatalf("test shim did not move HEAD: got %s, want %s", liveHead, replacementHead)
	}
	if remoteHead := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); remoteHead != approvedHead {
		t.Fatalf("remote received mutable HEAD %s instead of verified commit %s", remoteHead, approvedHead)
	}
	dbRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbRun.HeadSHA != approvedHead || dbRun.LastPushedSHA == nil || *dbRun.LastPushedSHA != approvedHead {
		t.Fatalf("durable push binding did not retain verified commit %s: %#v", approvedHead, dbRun)
	}
	t.Logf(
		"review-approved=%s concurrent-HEAD=%s remote-delivered=%s durable-head=%s durable-last-pushed=%s",
		approvedHead,
		replacementHead,
		gitCmd(t, upstream, "rev-parse", "refs/heads/feature"),
		dbRun.HeadSHA,
		*dbRun.LastPushedSHA,
	)
}

func TestPushStep_ReconcilesStaleDatabaseHeadSHA(t *testing.T) {
	// When push retries after a prior UpdateRunHeadSHA failure, there are no
	// uncommitted changes. The step must still reconcile the DB if HeadSHA is stale.
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	actualHeadSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	baseSHA := gitCmd(t, dir, "rev-parse", "main")
	gitCmd(t, dir, "push", "origin", "feature")

	// Create context with a stale HeadSHA (simulates prior DB write failure)
	staleHeadSHA := baseSHA // intentionally wrong
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, staleHeadSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	setupGateMirror(t, sctx)
	recordReviewApproval(t, sctx, actualHeadSHA)

	step := &PushStep{}
	_, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}

	// In-memory HeadSHA must match actual HEAD
	if sctx.Run.HeadSHA != actualHeadSHA {
		t.Errorf("Run.HeadSHA = %s, want %s", sctx.Run.HeadSHA, actualHeadSHA)
	}

	// DB record must also be updated
	dbRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbRun.HeadSHA != actualHeadSHA {
		t.Errorf("DB HeadSHA = %s, want %s", dbRun.HeadSHA, actualHeadSHA)
	}
	if dbRun.LastPushedSHA == nil || *dbRun.LastPushedSHA != actualHeadSHA || dbRun.PushGeneration == nil || *dbRun.PushGeneration != 1 {
		t.Fatalf("already-up-to-date push binding = %#v", dbRun)
	}
	if dbRun.PushActive {
		t.Fatal("push-active marker remained set after successful step")
	}
}

func TestPushStep_DoesNotPublishTestEvidenceIntoThePushedBranch(t *testing.T) {
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	baseSHA := gitCmd(t, dir, "rev-parse", "main")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "feature"
	setupGateMirror(t, sctx)
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true, Dir: "evidence", Branch: "no-mistakes/evidence"}
	recordReviewApproval(t, sctx, headSHA)

	// Evidence for this run exists, collected outside the worktree.
	evidenceDir := testEvidenceDir(sctx)
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(evidenceDir) })
	if err := os.WriteFile(filepath.Join(evidenceDir, "checkout.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}

	step := &PushStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	clone := t.TempDir()
	gitCmd(t, clone, "clone", "--branch", "feature", upstream, ".")
	tracked := gitCmd(t, clone, "ls-files")
	if strings.Contains(tracked, "evidence") || strings.Contains(tracked, ".png") {
		t.Fatalf("pushed branch carries evidence files:\n%s", tracked)
	}
}

func TestPushStep_TargetsForkWhenConfigured(t *testing.T) {
	parent := t.TempDir()
	fork := t.TempDir()
	gitCmd(t, parent, "init", "--bare")
	gitCmd(t, fork, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", parent)
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "push", fork, "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = parent
	sctx.Repo.ForkURL = fork
	sctx.Run.Branch = "feature"
	setupGateMirror(t, sctx)
	recordReviewApproval(t, sctx, headSHA)

	step := &PushStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	forkSHA := gitCmd(t, fork, "rev-parse", "refs/heads/feature")
	if forkSHA != headSHA {
		t.Fatalf("fork branch SHA = %s, want %s", forkSHA, headSHA)
	}
	if out, err := exec.Command("git", "-C", parent, "rev-parse", "--verify", "refs/heads/feature").CombinedOutput(); err == nil {
		t.Fatalf("parent unexpectedly received feature branch at %s", strings.TrimSpace(string(out)))
	}
	dbRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbRun.LastPushedSHA == nil || *dbRun.LastPushedSHA != headSHA || dbRun.PushTargetKind == nil || *dbRun.PushTargetKind != "fork" || dbRun.PushRef == nil || *dbRun.PushRef != "refs/heads/feature" {
		t.Fatalf("fork push binding = %#v", dbRun)
	}
	if dbRun.PushTargetFingerprint == nil || strings.Contains(*dbRun.PushTargetFingerprint, fork) {
		t.Fatalf("push target fingerprint persisted a URL: %#v", dbRun.PushTargetFingerprint)
	}
}

func TestPushStep_RedactsForkURLInGitErrors(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "git")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CLI_MODE", "git-remote-error")
	t.Setenv("FAKE_CLI_REAL_GIT", realGit)

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "https://github.com/parent/project.git"
	sctx.Repo.ForkURL = "https://user:secret@example.com/fork/project.git"
	sctx.Run.Branch = "refs/heads/feature"
	setupGateMirror(t, sctx)
	recordReviewApproval(t, sctx, headSHA)

	step := &PushStep{}
	_, err = step.Execute(sctx)
	if err == nil {
		t.Fatal("expected push error")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("expected error to redact fork credentials, got %v", err)
	}
	if !strings.Contains(err.Error(), "https://redacted@example.com/fork/project.git") {
		t.Fatalf("expected redacted fork URL in error, got %v", err)
	}
}

// TestPushStep_CommitsLeftoverChangesWhenLegacyHuskyRuntimeIsMissing pins the
// Push step's leftover-worktree commit against the same fresh-worktree hook
// failure the correction-commit helper exists to survive: core.hooksPath=.husky
// with a tracked pre-commit hook sourcing the generated .husky/_/husky.sh that
// this worktree never had. The formatter or the Test step's evidence agent left
// an uncommitted edit behind, and the run must still deliver it.
func TestPushStep_CommitsLeftoverChangesWhenLegacyHuskyRuntimeIsMissing(t *testing.T) {
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir, baseSHA, _ := setupGitRepo(t)
	hooksDir := filepath.Join(dir, ".husky")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooksDir, "pre-commit"), []byte("#!/usr/bin/env sh\n. \"$(dirname -- \"$0\")/_/husky.sh\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", ".husky/pre-commit")
	gitCmd(t, dir, "commit", "-m", "add legacy Husky hook")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "config", "core.hooksPath", ".husky")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "push", "origin", "feature")

	// Positive control: with the hook live, a verified commit cannot succeed in
	// this worktree, so the assertions below prove the bypass rather than an
	// inert hook configuration.
	if err := os.WriteFile(filepath.Join(dir, "hook-probe.txt"), []byte("probe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "hook-probe.txt")
	if out, err := runGitDirect(dir, "commit", "-m", "probe"); err == nil {
		t.Fatalf("expected the legacy Husky hook to block a verified commit, got success:\n%s", out)
	}
	gitCmd(t, dir, "reset", "HEAD", "hook-probe.txt")
	if err := os.Remove(filepath.Join(dir, "hook-probe.txt")); err != nil {
		t.Fatal(err)
	}

	// Leftover worktree change the Push step must commit before pushing.
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("formatted feature code\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	setupGateMirror(t, sctx)
	recordReviewApproval(t, sctx, headSHA)

	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatalf("push failed on a worktree whose legacy Husky runtime is absent: %v", err)
	}

	if got := gitStatusPorcelain(t, dir); got != "" {
		t.Fatalf("expected clean worktree after the leftover commit, got %q", got)
	}
	pushedHead := gitCmd(t, dir, "rev-parse", "HEAD")
	if pushedHead == headSHA {
		t.Fatal("expected a new correction commit carrying the leftover change")
	}
	if got := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); got != pushedHead {
		t.Fatalf("remote head = %s, want pushed correction commit %s", got, pushedHead)
	}
	if got := gitCmd(t, dir, "show", pushedHead+":feature.txt"); got != "formatted feature code" {
		t.Fatalf("delivered feature.txt = %q, want the leftover change", got)
	}
	if _, err := os.Stat(filepath.Join(hooksDir, "_", "husky.sh")); !os.IsNotExist(err) {
		t.Fatalf("legacy Husky runtime unexpectedly exists: %v", err)
	}
}

// TestPushStep_AllowsForcePushAfterMidRunRebaseOverPriorPushedGeneration (Issue #837)
// reproduces the exact pipeline sequence:
// 1. Generation 1 pushed gen1Head to the remote.
// 2. Upstream main advanced mid-run.
// 3. The worktree rebased onto the new main, dropping duplicate fix hunks.
// 4. PushStep must allow force-pushing the new rebased head over the pipeline's own prior push.
func TestPushStep_AllowsForcePushAfterMidRunRebaseOverPriorPushedGeneration(t *testing.T) {
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir, baseSHA, submittedHead := setupGitRepo(t)
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	// Generation 1: feature branch is pushed to remote.
	if err := os.WriteFile(filepath.Join(dir, "gen1_fix.txt"), []byte("gen1 fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "gen1 pipeline fix")
	gen1Head := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	// Upstream main advances mid-run.
	other := t.TempDir()
	gitCmd(t, other, "clone", upstream, ".")
	gitCmd(t, other, "config", "user.name", "o")
	gitCmd(t, other, "config", "user.email", "o@test.com")
	gitCmd(t, other, "checkout", "main")
	if err := os.WriteFile(filepath.Join(other, "main_advance.txt"), []byte("advance\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, other, "add", "-A")
	gitCmd(t, other, "commit", "-m", "main advance")
	gitCmd(t, other, "push", "origin", "main")
	newBaseSHA := gitCmd(t, other, "rev-parse", "HEAD")

	// Mid-run rebase onto new main drops duplicate hunks from gen1Head.
	gitCmd(t, dir, "fetch", "origin", "main")
	gitCmd(t, dir, "reset", "--hard", newBaseSHA)
	if err := os.WriteFile(filepath.Join(dir, "rebased_work.txt"), []byte("rebased work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "rebased work")
	rebasedHead := gitCmd(t, dir, "rev-parse", "HEAD")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, submittedHead, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Run.HeadSHA = rebasedHead
	sctx.Run.BaseSHA = newBaseSHA
	sctx.Run.LastPushedSHA = &gen1Head
	setupGateMirror(t, sctx)
	recordReviewApproval(t, sctx, rebasedHead)

	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatalf("push step failed after mid-run rebase over prior pushed generation: %v", err)
	}

	remoteHead := gitCmd(t, upstream, "rev-parse", "refs/heads/feature")
	if remoteHead != rebasedHead {
		t.Fatalf("expected remote head = %s, got %s", rebasedHead, remoteHead)
	}
}

// TestPushStep_AllowsForcePushOnRerunOverPriorRunPushedGeneration (Issue #837)
// reproduces a `rerun` where Run1 pushed gen1Head, then Run2 (with LastPushedSHA==nil)
// validates a rebased head. PushStep must look up the branch's prior pushed head
// from durable DB records and allow the force-push over the prior run's push.
func TestPushStep_AllowsForcePushOnRerunOverPriorRunPushedGeneration(t *testing.T) {
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir, baseSHA, submittedHead := setupGitRepo(t)
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	// Prior Run 1: pushed gen1Head to the remote.
	if err := os.WriteFile(filepath.Join(dir, "gen1_fix.txt"), []byte("gen1 fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "gen1 pipeline fix")
	gen1Head := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	// Upstream main advances mid-run.
	other := t.TempDir()
	gitCmd(t, other, "clone", upstream, ".")
	gitCmd(t, other, "config", "user.name", "o")
	gitCmd(t, other, "config", "user.email", "o@test.com")
	gitCmd(t, other, "checkout", "main")
	if err := os.WriteFile(filepath.Join(other, "main_advance.txt"), []byte("advance\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, other, "add", "-A")
	gitCmd(t, other, "commit", "-m", "main advance")
	gitCmd(t, other, "push", "origin", "main")
	newBaseSHA := gitCmd(t, other, "rev-parse", "HEAD")

	// Rebase onto new main produces rebasedHead.
	gitCmd(t, dir, "fetch", "origin", "main")
	gitCmd(t, dir, "reset", "--hard", newBaseSHA)
	if err := os.WriteFile(filepath.Join(dir, "rebased_work.txt"), []byte("rebased work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "rebased work")
	rebasedHead := gitCmd(t, dir, "rev-parse", "HEAD")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, submittedHead, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Run.HeadSHA = rebasedHead
	sctx.Run.BaseSHA = newBaseSHA
	// Run2 itself has not pushed yet (LastPushedSHA is nil)
	sctx.Run.LastPushedSHA = nil
	setupGateMirror(t, sctx)
	recordReviewApproval(t, sctx, rebasedHead)

	// Record that a prior run for this repo/branch pushed gen1Head.
	priorRun, err := sctx.DB.InsertRun(sctx.Repo.ID, "refs/heads/feature", gen1Head, baseSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateRunPushBinding(priorRun.ID, db.PushBinding{
		HeadSHA:           gen1Head,
		TargetKind:        "upstream",
		TargetFingerprint: "fingerprint",
		Ref:               "refs/heads/feature",
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatalf("push step failed on rerun over prior run's pushed generation: %v", err)
	}

	remoteHead := gitCmd(t, upstream, "rev-parse", "refs/heads/feature")
	if remoteHead != rebasedHead {
		t.Fatalf("expected remote head = %s, got %s", rebasedHead, remoteHead)
	}
}

func TestLastKnownBranchTip_BranchRefNormalization(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.LastPushedSHA = nil

	sha1 := "1111111111111111111111111111111111111111"
	priorRun1, err := sctx.DB.InsertRun(sctx.Repo.ID, "refs/heads/feature", sha1, baseSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateRunPushBinding(priorRun1.ID, db.PushBinding{
		HeadSHA:           sha1,
		TargetKind:        "upstream",
		TargetFingerprint: "fingerprint",
		Ref:               "refs/heads/feature",
	}); err != nil {
		t.Fatal(err)
	}

	if got := lastKnownBranchTip(sctx.Ctx, sctx, "feature", false); got != sha1 {
		t.Fatalf("lastKnownBranchTip with query 'feature' = %q, want %q", got, sha1)
	}
	if got := lastKnownBranchTip(sctx.Ctx, sctx, "refs/heads/feature", false); got != sha1 {
		t.Fatalf("lastKnownBranchTip with query 'refs/heads/feature' = %q, want %q", got, sha1)
	}

	sha2 := "2222222222222222222222222222222222222222"
	priorRun2, err := sctx.DB.InsertRun(sctx.Repo.ID, "unprefixed-branch", sha2, baseSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateRunPushBinding(priorRun2.ID, db.PushBinding{
		HeadSHA:           sha2,
		TargetKind:        "upstream",
		TargetFingerprint: "fingerprint",
		Ref:               "unprefixed-branch",
	}); err != nil {
		t.Fatal(err)
	}

	if got := lastKnownBranchTip(sctx.Ctx, sctx, "unprefixed-branch", false); got != sha2 {
		t.Fatalf("lastKnownBranchTip with query 'unprefixed-branch' = %q, want %q", got, sha2)
	}
	if got := lastKnownBranchTip(sctx.Ctx, sctx, "refs/heads/unprefixed-branch", false); got != sha2 {
		t.Fatalf("lastKnownBranchTip with query 'refs/heads/unprefixed-branch' = %q, want %q", got, sha2)
	}
}

func TestPushStep_UpdatesGateMirrorRefOnSuccessfulPush(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir, baseSHA, submittedHead := setupGitRepo(t)
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "push", "origin", "feature")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, submittedHead, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"

	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	gateDir := p.RepoDir(sctx.Repo.ID)
	sctx.GateDir = gateDir
	if err := os.MkdirAll(filepath.Dir(gateDir), 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, filepath.Dir(gateDir), "init", "--bare", filepath.Base(gateDir))

	// Initial gate head is submittedHead
	gitCmd(t, gateDir, "fetch", dir, "refs/heads/feature:refs/heads/feature")
	initialGateHead := gitCmd(t, gateDir, "rev-parse", "refs/heads/feature")
	if initialGateHead != submittedHead {
		t.Fatalf("expected initial gate head = %s, got %s", submittedHead, initialGateHead)
	}

	// Worktree produces a new non-fast-forward rebased head
	gitCmd(t, dir, "reset", "--hard", baseSHA)
	if err := os.WriteFile(filepath.Join(dir, "rebased.txt"), []byte("rebased\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "non-fast-forward rebased commit")
	rebasedHead := gitCmd(t, dir, "rev-parse", "HEAD")

	sctx.Run.HeadSHA = rebasedHead
	recordReviewApproval(t, sctx, rebasedHead)

	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatalf("push step failed: %v", err)
	}

	// Remote head should be updated
	remoteHead := gitCmd(t, upstream, "rev-parse", "refs/heads/feature")
	if remoteHead != rebasedHead {
		t.Fatalf("expected remote head = %s, got %s", rebasedHead, remoteHead)
	}

	// Gate mirror ref should also be updated
	gateHead := gitCmd(t, gateDir, "rev-parse", "refs/heads/feature")
	if gateHead != rebasedHead {
		t.Fatalf("expected gate mirror ref = %s, got %s", rebasedHead, gateHead)
	}
}

func TestPushStep_GateMirrorUpdateFailurePropagatesError(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir, baseSHA, submittedHead := setupGitRepo(t)
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "push", "origin", "feature")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, submittedHead, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"

	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	gateDir := p.RepoDir(sctx.Repo.ID)
	sctx.GateDir = gateDir
	// Create gateDir as a normal directory that is NOT a valid git repository
	if err := os.MkdirAll(gateDir, 0o755); err != nil {
		t.Fatal(err)
	}

	recordReviewApproval(t, sctx, submittedHead)

	_, err = (&PushStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected push step to fail when gate mirror update fails, but got nil")
	}
	if !strings.Contains(err.Error(), "update gate mirror ref") {
		t.Fatalf("expected error to mention gate mirror ref update, got: %v", err)
	}
}

func TestPushStep_GateMirrorFetchesExplicitPushedHeadInDetachedWorktree(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir, baseSHA, submittedHead := setupGitRepo(t)
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "push", "origin", "feature")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, submittedHead, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"

	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	gateDir := p.RepoDir(sctx.Repo.ID)
	sctx.GateDir = gateDir
	if err := os.MkdirAll(filepath.Dir(gateDir), 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, filepath.Dir(gateDir), "init", "--bare", filepath.Base(gateDir))

	// Initial gate head is submittedHead
	gitCmd(t, gateDir, "fetch", dir, "refs/heads/feature:refs/heads/feature")
	initialGateHead := gitCmd(t, gateDir, "rev-parse", "refs/heads/feature")
	if initialGateHead != submittedHead {
		t.Fatalf("expected initial gate head = %s, got %s", submittedHead, initialGateHead)
	}

	// Detach worktree and create rebased commit on detached HEAD
	gitCmd(t, dir, "checkout", "--detach", submittedHead)
	if err := os.WriteFile(filepath.Join(dir, "rebased.txt"), []byte("rebased\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "rebased commit in detached HEAD")
	rebasedHead := gitCmd(t, dir, "rev-parse", "HEAD")

	// Ensure local branch ref "feature" is still pointing to submittedHead (stale)
	staleLocalRef := gitCmd(t, dir, "rev-parse", "refs/heads/feature")
	if staleLocalRef != submittedHead {
		t.Fatalf("expected local branch ref to be stale submittedHead %s, got %s", submittedHead, staleLocalRef)
	}

	sctx.Run.HeadSHA = rebasedHead
	recordReviewApproval(t, sctx, rebasedHead)

	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatalf("push step failed: %v", err)
	}

	// Remote head should be updated to rebasedHead
	remoteHead := gitCmd(t, upstream, "rev-parse", "refs/heads/feature")
	if remoteHead != rebasedHead {
		t.Fatalf("expected remote head = %s, got %s", rebasedHead, remoteHead)
	}

	// Gate mirror ref should be updated to rebasedHead (not the stale local branch ref)
	gateHead := gitCmd(t, gateDir, "rev-parse", "refs/heads/feature")
	if gateHead != rebasedHead {
		t.Fatalf("expected gate mirror ref = %s, got %s", rebasedHead, gateHead)
	}
}

func TestPushStep_SkipsWhenGateMirrorDirectoryMissing(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir, baseSHA, submittedHead := setupGitRepo(t)
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "push", "origin", "feature")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, submittedHead, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"

	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	gateDir := p.RepoDir(sctx.Repo.ID)
	sctx.GateDir = gateDir
	// Explicitly remove gateDir to test missing directory failure
	_ = os.RemoveAll(gateDir)

	recordReviewApproval(t, sctx, submittedHead)

	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatalf("push step failed with missing gate mirror: %v", err)
	}
	if remoteHead := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); remoteHead != submittedHead {
		t.Fatalf("remote head = %s, want %s", remoteHead, submittedHead)
	}
}

func TestPushStep_GateMirrorDoesNotRewindNewerInterveningPush(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir, baseSHA, submittedHead := setupGitRepo(t)
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "push", "origin", "feature")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, submittedHead, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Run.SubmittedHeadSHA = &submittedHead

	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	gateDir := p.RepoDir(sctx.Repo.ID)
	sctx.GateDir = gateDir
	if err := os.MkdirAll(filepath.Dir(gateDir), 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, filepath.Dir(gateDir), "init", "--bare", filepath.Base(gateDir))

	// Initial gate head is submittedHead
	gitCmd(t, gateDir, "fetch", dir, "refs/heads/feature:refs/heads/feature")

	// Worktree produces a new rebased head
	if err := os.WriteFile(filepath.Join(dir, "rebased.txt"), []byte("rebased\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "rebased commit")
	rebasedHead := gitCmd(t, dir, "rev-parse", "HEAD")

	// Simulate an intervening push advancing gateDir to a newer commit
	if err := os.WriteFile(filepath.Join(dir, "intervening.txt"), []byte("intervening\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "intervening commit")
	interveningHead := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, gateDir, "fetch", dir, "HEAD:refs/heads/feature")

	// Reset worktree back to rebasedHead
	gitCmd(t, dir, "reset", "--hard", rebasedHead)

	sctx.Run.HeadSHA = rebasedHead
	recordReviewApproval(t, sctx, rebasedHead)

	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatalf("push step failed: %v", err)
	}

	// Remote head should be updated to rebasedHead
	remoteHead := gitCmd(t, upstream, "rev-parse", "refs/heads/feature")
	if remoteHead != rebasedHead {
		t.Fatalf("expected remote head = %s, got %s", rebasedHead, remoteHead)
	}

	// Gate mirror ref must NOT be rewound to rebasedHead; it must remain at interveningHead
	gateHead := gitCmd(t, gateDir, "rev-parse", "refs/heads/feature")
	if gateHead != interveningHead {
		t.Fatalf("expected gate mirror ref to remain at %s, got %s", interveningHead, gateHead)
	}
}
