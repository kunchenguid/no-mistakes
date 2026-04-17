package steps

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

func TestCIStep_CommitAndPush(t *testing.T) {
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
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"

	step := &CIStep{}
	pushed, err := step.commitAndPush(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !pushed {
		t.Error("expected commitAndPush to report changes were pushed")
	}

	// Verify the commit and push happened
	upstreamSHA := gitCmd(t, upstream, "rev-parse", "refs/heads/feature")
	if upstreamSHA == headSHA {
		t.Error("upstream should have a new commit with CI fixes")
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
	pushed, err := step.commitAndPush(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if pushed {
		t.Error("expected commitAndPush to report no changes pushed")
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
	pushed, err := step.commitAndPush(sctx)
	if err == nil {
		t.Fatal("expected status error")
	}
	if pushed {
		t.Error("expected commitAndPush to report no push on status error")
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

	step := &CIStep{}
	pushed, err := step.commitAndPush(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !pushed {
		t.Fatal("expected commitAndPush to report changes were pushed")
	}

	upstreamSHA := realGitCmd(upstream, "rev-parse", "refs/heads/feature")
	if upstreamSHA == headSHA {
		t.Fatal("expected upstream to receive CI fix commit")
	}
	if sctx.Run.HeadSHA != upstreamSHA {
		t.Fatalf("Run.HeadSHA = %s, want %s", sctx.Run.HeadSHA, upstreamSHA)
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

	step := &CIStep{}
	pushed, err := step.commitAndPush(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if pushed {
		t.Error("expected commitAndPush to report no changes pushed for stale reconcile")
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

	step := &CIStep{}
	pushed, err := step.commitAndPush(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if pushed {
		t.Error("expected commitAndPush to report no changes pushed for stale reconcile")
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

func TestCIStep_CommitAndPush_NoDirtyChangesButHeadAdvanced_PushesNewHead(t *testing.T) {
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

	step := &CIStep{}
	pushed, err := step.commitAndPush(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !pushed {
		t.Fatal("expected commitAndPush to push advanced clean head")
	}

	upstreamSHA := gitCmd(t, upstream, "rev-parse", "refs/heads/feature")
	if upstreamSHA != advancedHeadSHA {
		t.Fatalf("upstream SHA = %s, want %s", upstreamSHA, advancedHeadSHA)
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

func TestCIStep_CommitAndPush_UpdatesLocalBranchRefAfterDetachedPush(t *testing.T) {
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

	step := &CIStep{}
	pushed, err := step.commitAndPush(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !pushed {
		t.Error("expected commitAndPush to report changes were pushed")
	}
	newHeadSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	branchSHA := gitCmd(t, dir, "rev-parse", "refs/heads/feature")
	if branchSHA != newHeadSHA {
		t.Fatalf("branch ref SHA = %s, want %s", branchSHA, newHeadSHA)
	}
	upstreamSHA := gitCmd(t, upstream, "rev-parse", "refs/heads/feature")
	if upstreamSHA != newHeadSHA {
		t.Fatalf("upstream SHA = %s, want %s", upstreamSHA, newHeadSHA)
	}
}
