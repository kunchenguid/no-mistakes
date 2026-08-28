package steps

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
)

func TestCIStep_CommitAndPush_CommitsLocallyWithoutPushing(t *testing.T) {
	t.Parallel()
	// Set up upstream bare repo
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	// Create working repo
	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	// Add uncommitted changes
	os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("ci fix"), 0o644)

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	if err := sctx.DB.UpdateRunPushBinding(sctx.Run.ID, db.PushBinding{HeadSHA: headSHA, TargetKind: "upstream", TargetFingerprint: branchsync.TargetFingerprint(upstream), Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}

	// This test pins the ci.revalidate_repairs: true path, where the
	// repair is held locally until Review re-approves it.
	sctx.Config.CI.RevalidateRepairs = true
	step := &CIStep{}
	repair, err := step.commitRepair(sctx, "stabilize Windows path test")
	if err != nil {
		t.Fatal(err)
	}
	if !repair.HeadAdvanced {
		t.Error("expected commitAndPush to report committed changes")
	}

	localSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	upstreamSHA := gitCmd(t, upstream, "rev-parse", "refs/heads/feature")
	if upstreamSHA != headSHA {
		t.Fatalf("upstream head = %s, want unchanged head %s", upstreamSHA, headSHA)
	}
	if localSHA == headSHA {
		t.Fatal("local head should include the CI repair commit")
	}
	if message := gitCmd(t, dir, "log", "-1", "--format=%s"); message != "no-mistakes(ci): stabilize Windows path test" {
		t.Fatalf("repair commit message = %q", message)
	}
	dbRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbRun.HeadSHA != localSHA {
		t.Fatalf("recorded head = %s, want local repair %s", dbRun.HeadSHA, localSHA)
	}
	if dbRun.LastPushedSHA == nil || *dbRun.LastPushedSHA != upstreamSHA || dbRun.PushGeneration == nil || *dbRun.PushGeneration != 1 {
		t.Fatalf("CI repair changed push binding = %#v", dbRun)
	}
}

func TestCIStep_CommitAndPushDoesNotPushForkWhenConfigured(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(dir, "ci-fix.txt"), []byte("fixed"), 0o644); err != nil {
		t.Fatal(err)
	}

	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = parent
	sctx.Repo.ForkURL = fork
	sctx.Run.Branch = "refs/heads/feature"

	// This test pins the ci.revalidate_repairs: true path, where the
	// repair is held locally until Review re-approves it.
	sctx.Config.CI.RevalidateRepairs = true
	step := &CIStep{}
	repair, err := step.commitAndPush(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !repair.HeadAdvanced {
		t.Fatal("expected commitAndPush to report committed changes")
	}

	if out, err := exec.Command("git", "-C", fork, "rev-parse", "--verify", "refs/heads/feature").CombinedOutput(); err == nil {
		t.Fatalf("fork unexpectedly received feature branch at %s", strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("git", "-C", parent, "rev-parse", "--verify", "refs/heads/feature").CombinedOutput(); err == nil {
		t.Fatalf("parent unexpectedly received feature branch at %s", strings.TrimSpace(string(out)))
	}
}

func TestCIStep_CommitAndPush_NoChanges(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "dummy"
	sctx.Run.Branch = "refs/heads/feature"

	step := &CIStep{}
	repair, err := step.commitAndPush(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if repair.HeadAdvanced {
		t.Error("expected commitAndPush to report no local changes")
	}
}

func TestCIStep_InvalidCommitTemplateDoesNotStageChanges(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContext(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Commit = config.Commit{FixMessage: `{{printf "%s" .Summary}}`}

	if err := os.WriteFile(filepath.Join(dir, "ci-fix.txt"), []byte("fixed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (&CIStep{}).commitRepair(sctx, "repair checks"); err == nil {
		t.Fatal("commitRepair() accepted an invalid commit.fix_message")
	}
	if got := gitCmd(t, dir, "diff", "--cached", "--name-only"); got != "" {
		t.Fatalf("staged files after template error = %q, want none", got)
	}
}

func TestCIStep_CommitAndPush_StatusError(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}

	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "git")
	env := fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":     "git-status-error",
		"FAKE_CLI_REAL_GIT": realGit,
	})

	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Repo.UpstreamURL = "dummy"
	sctx.Run.Branch = "refs/heads/feature"

	step := &CIStep{}
	repair, err := step.commitAndPush(sctx)
	if err == nil {
		t.Fatal("expected status error")
	}
	if repair.HeadAdvanced {
		t.Error("expected commitAndPush to report no changes on status error")
	}
	if !strings.Contains(err.Error(), "git status --porcelain") {
		t.Fatalf("expected status command in error, got %v", err)
	}
	if !strings.Contains(err.Error(), "status failed") {
		t.Fatalf("expected status stderr in error, got %v", err)
	}
}

func TestCIStep_CommitAndPush_UsesStepEnvForAllGitCommands(t *testing.T) {
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
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")
	os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("ci fix"), 0o644)

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "git")
	env := fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":     "git-passthrough",
		"FAKE_CLI_REAL_GIT": realGit,
	})
	t.Setenv("PATH", t.TempDir())
	realGitCmd := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command(realGit, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test",
			"GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=test",
			"GIT_COMMITTER_EMAIL=test@test.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"

	// This test pins the ci.revalidate_repairs: true path, where the
	// repair is held locally until Review re-approves it.
	sctx.Config.CI.RevalidateRepairs = true
	step := &CIStep{}
	repair, err := step.commitAndPush(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !repair.HeadAdvanced {
		t.Fatal("expected commitAndPush to report committed changes")
	}

	localSHA := realGitCmd(dir, "rev-parse", "HEAD")
	upstreamSHA := realGitCmd(upstream, "rev-parse", "refs/heads/feature")
	if upstreamSHA != headSHA {
		t.Fatal("expected upstream to remain on the pre-repair head")
	}
	if sctx.Run.HeadSHA != localSHA {
		t.Fatalf("Run.HeadSHA = %s, want %s", sctx.Run.HeadSHA, localSHA)
	}
}

func TestCIStep_CommitAndPush_GitCommandsUseStandardCredentialEnv(t *testing.T) {
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
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")
	if err := os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("ci fix"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GIT_EDITOR", "vim")
	t.Setenv("GIT_SEQUENCE_EDITOR", "vim")
	t.Setenv("GIT_TERMINAL_PROMPT", "1")
	t.Setenv("GIT_OPTIONAL_LOCKS", "1")
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[credential \"https://github.com\"]\n\thelper = !gh auth git-credential\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "git")
	env := fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":     "git-require-noninteractive-env",
		"FAKE_CLI_REAL_GIT": realGit,
	})
	t.Setenv("PATH", t.TempDir())

	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"

	// This test pins the ci.revalidate_repairs: true path, where the
	// repair is held locally until Review re-approves it.
	sctx.Config.CI.RevalidateRepairs = true
	step := &CIStep{}
	repair, err := step.commitAndPush(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !repair.HeadAdvanced {
		t.Fatal("expected commitAndPush to report committed changes")
	}
}

func TestCIStep_CommitAndPush_NoChanges_ReconcilesStaleDatabaseHeadSHA(t *testing.T) {
	t.Parallel()
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
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	actualHeadSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	// Create context with stale HeadSHA (simulates prior DB write failure)
	staleHeadSHA := baseSHA
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, staleHeadSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"

	// This test pins the ci.revalidate_repairs: true path, where the
	// repair is held locally until Review re-approves it.
	sctx.Config.CI.RevalidateRepairs = true
	step := &CIStep{}
	repair, err := step.commitAndPush(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !repair.HeadAdvanced {
		t.Error("expected commitAndPush to report the reconciled local head")
	}

	if sctx.Run.HeadSHA != actualHeadSHA {
		t.Errorf("Run.HeadSHA = %s, want %s", sctx.Run.HeadSHA, actualHeadSHA)
	}
	dbRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbRun.HeadSHA != actualHeadSHA {
		t.Errorf("DB HeadSHA = %s, want %s", dbRun.HeadSHA, actualHeadSHA)
	}
}

func TestCIStep_CommitAndPush_NoChanges_ReconcilesStaleDatabaseHeadSHA_UsesStepEnv(t *testing.T) {
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
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	actualHeadSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "git")
	env := fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":     "git-passthrough",
		"FAKE_CLI_REAL_GIT": realGit,
	})
	t.Setenv("PATH", t.TempDir())

	staleHeadSHA := baseSHA
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, staleHeadSHA, config.Commands{})
	sctx.Env = env
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"

	// This test pins the ci.revalidate_repairs: true path, where the
	// repair is held locally until Review re-approves it.
	sctx.Config.CI.RevalidateRepairs = true
	step := &CIStep{}
	repair, err := step.commitAndPush(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !repair.HeadAdvanced {
		t.Error("expected commitAndPush to report the reconciled local head")
	}

	if sctx.Run.HeadSHA != actualHeadSHA {
		t.Errorf("Run.HeadSHA = %s, want %s", sctx.Run.HeadSHA, actualHeadSHA)
	}
	dbRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbRun.HeadSHA != actualHeadSHA {
		t.Errorf("DB HeadSHA = %s, want %s", dbRun.HeadSHA, actualHeadSHA)
	}
}

func TestCIStep_CommitAndPush_NoDirtyChangesRecordsAdvancedLocalHead(t *testing.T) {
	t.Parallel()
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
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	originalHeadSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	os.WriteFile(filepath.Join(dir, "resolved.txt"), []byte("resolved"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "resolve conflict")
	advancedHeadSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, originalHeadSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"

	// This test pins the ci.revalidate_repairs: true path, where the
	// repair is held locally until Review re-approves it.
	sctx.Config.CI.RevalidateRepairs = true
	step := &CIStep{}
	repair, err := step.commitAndPush(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !repair.HeadAdvanced {
		t.Fatal("expected commitAndPush to record advanced clean head")
	}

	upstreamSHA := gitCmd(t, upstream, "rev-parse", "refs/heads/feature")
	if upstreamSHA != originalHeadSHA {
		t.Fatalf("upstream SHA = %s, want unchanged head %s", upstreamSHA, originalHeadSHA)
	}
	if sctx.Run.HeadSHA != advancedHeadSHA {
		t.Fatalf("Run.HeadSHA = %s, want %s", sctx.Run.HeadSHA, advancedHeadSHA)
	}
	dbRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbRun.HeadSHA != advancedHeadSHA {
		t.Fatalf("DB HeadSHA = %s, want %s", dbRun.HeadSHA, advancedHeadSHA)
	}
}

func TestCIStep_CommitAndPush_UpdatesLocalBranchRefWithoutDetachedPush(t *testing.T) {
	t.Parallel()
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
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	originalHeadSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")
	gitCmd(t, dir, "checkout", "--detach", originalHeadSHA)
	os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("ci fix"), 0o644)

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, originalHeadSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"

	// This test pins the ci.revalidate_repairs: true path, where the
	// repair is held locally until Review re-approves it.
	sctx.Config.CI.RevalidateRepairs = true
	step := &CIStep{}
	repair, err := step.commitAndPush(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !repair.HeadAdvanced {
		t.Error("expected commitAndPush to report committed changes")
	}
	newHeadSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	branchSHA := gitCmd(t, dir, "rev-parse", "refs/heads/feature")
	if branchSHA != newHeadSHA {
		t.Fatalf("branch ref SHA = %s, want %s", branchSHA, newHeadSHA)
	}
	upstreamSHA := gitCmd(t, upstream, "rev-parse", "refs/heads/feature")
	if upstreamSHA != originalHeadSHA {
		t.Fatalf("upstream SHA = %s, want unchanged head %s", upstreamSHA, originalHeadSHA)
	}
}
