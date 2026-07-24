package steps

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

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
	// This models the current in-memory run state left by the review-fix commit.
	sctx.Run.HeadSHA = reviewedHead
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
	binDir := t.TempDir()
	shim := filepath.Join(binDir, "git")
	shimBody := `#!/bin/sh
if [ "$1" = "push" ]; then
  "$NM_TEST_REAL_GIT" reset --hard "$NM_TEST_REPLACEMENT_HEAD" >/dev/null
fi
exec "$NM_TEST_REAL_GIT" "$@"
`
	if err := os.WriteFile(shim, []byte(shimBody), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NM_TEST_REAL_GIT", realGit)
	t.Setenv("NM_TEST_REPLACEMENT_HEAD", replacementHead)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, submittedHead, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
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
}

func TestPushStep_ReconcilesStaleDatabaseHeadSHA(t *testing.T) {
	t.Parallel()
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

func TestPushStep_ForceAddsInRepoEvidenceArtifacts(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.png\n"), 0o644); err != nil {
		t.Fatal(err)
	}
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
	evidenceDir := filepath.Join(dir, "evidence", "feature")
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidenceDir, "checkout.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "feature"
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true, Dir: "evidence"}
	recordReviewApproval(t, sctx, headSHA)

	step := &PushStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	clone := t.TempDir()
	gitCmd(t, clone, "clone", "--branch", "feature", upstream, ".")
	if _, err := os.Stat(filepath.Join(clone, "evidence", "feature", "checkout.png")); err != nil {
		t.Fatalf("expected ignored evidence artifact to be pushed: %v", err)
	}
}

func TestPushStep_TargetsForkWhenConfigured(t *testing.T) {
	t.Parallel()
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

func TestPushStep_DoesNotForceAddIgnoredEvidenceDirectory(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("evidence/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", ".gitignore")
	gitCmd(t, dir, "commit", "-m", "ignore evidence")
	headSHA = gitCmd(t, dir, "rev-parse", "HEAD")
	evidenceDir := filepath.Join(dir, "evidence", "feature")
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidenceDir, "stale.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "feature"
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true, Dir: "evidence"}

	step := &PushStep{}
	if err := step.stageInRepoEvidence(sctx); err != nil {
		t.Fatal(err)
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("ignored evidence directory was staged: %q", status)
	}
}
