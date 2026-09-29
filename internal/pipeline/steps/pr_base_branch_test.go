package steps

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestEffectivePRBaseBranch_PerRunOverrideWinsOverRepoConfig(t *testing.T) {
	t.Parallel()
	runBase := "epic/feature"
	sctx := &pipeline.StepContext{
		Run:  &db.Run{PRBaseBranch: &runBase},
		Repo: &db.Repo{DefaultBranch: "main"},
		Config: &config.Config{
			PR: config.PR{BaseBranch: "develop"},
		},
	}
	if got := effectivePRBaseBranch(sctx); got != runBase {
		t.Fatalf("effectivePRBaseBranch = %q, want %q", got, runBase)
	}
}

func TestEffectivePRBaseBranch_RepoConfigWhenNoRunOverride(t *testing.T) {
	t.Parallel()
	sctx := &pipeline.StepContext{
		Run:  &db.Run{},
		Repo: &db.Repo{DefaultBranch: "main"},
		Config: &config.Config{
			PR: config.PR{BaseBranch: "develop"},
		},
	}
	if got := effectivePRBaseBranch(sctx); got != "develop" {
		t.Fatalf("effectivePRBaseBranch = %q, want develop", got)
	}
}

func TestEffectivePRBaseBranch_DefaultBranchWhenUnset(t *testing.T) {
	t.Parallel()
	sctx := &pipeline.StepContext{
		Run:    &db.Run{},
		Repo:   &db.Repo{DefaultBranch: "main"},
		Config: &config.Config{},
	}
	if got := effectivePRBaseBranch(sctx); got != "main" {
		t.Fatalf("effectivePRBaseBranch = %q, want main", got)
	}
}

func TestCurrentPRTargetBranch_RejectsClosedRecordedPR(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	prURL := "https://github.com/test/repo/pull/42"
	env, _ := fakeGHWithBase(t, prURL, "develop")
	env = append(env, "FAKE_CLI_PR_STATE=CLOSED")
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL

	if _, err := currentPRTargetBranch(sctx); err == nil {
		t.Fatal("closed recorded PR must not supply the review target")
	}
}

func TestValidateRunPRBaseBranchName_RejectsInvalidBranch(t *testing.T) {
	t.Parallel()
	_, err := ValidateRunPRBaseBranchName("bad..branch")
	if err == nil {
		t.Fatal("expected error for invalid branch name")
	}
}

func TestPRStep_UsesPerRunBaseBranch(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ensureLocalBranch(t, dir, "epic/feature", baseSHA)
	env, logFile := fakeGH(t, "")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Config.PR.BaseBranch = "develop"
	runBase := "epic/feature"
	sctx.Run.PRBaseBranch = &runBase

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}

	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logData), "pr create --head feature --base epic/feature") {
		t.Fatalf("expected per-run base branch in PR creation, got:\n%s", logData)
	}
}

func TestPRStep_PerRunBaseBranchOverridesRepoConfig(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ensureLocalBranch(t, dir, "epic/feature", baseSHA)
	env, logFile := fakeGH(t, "")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Config.PR.BaseBranch = "develop"
	runBase := "epic/feature"
	sctx.Run.PRBaseBranch = &runBase

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}

	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logData), "--base develop") {
		t.Fatalf("repo config base should lose to per-run override, got:\n%s", logData)
	}
}

func TestPRStep_KeepsExistingPRBaseWhenPerRunBaseDiffers(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ensureLocalBranch(t, dir, "epic/feature", baseSHA)
	ensureLocalBranch(t, dir, "develop", baseSHA)
	env, logFile := fakeGHWithBase(t, "https://github.com/test/repo/pull/42", "develop")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	runBase := "epic/feature"
	sctx.Run.PRBaseBranch = &runBase
	owned := "https://github.com/test/repo/pull/42"
	sctx.Run.PRURL = &owned
	sctx.PRTarget = &pipeline.PRTargetSelection{PRURL: owned, TargetBranch: "develop"}

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}

	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	ghLog := string(logData)
	if strings.Contains(ghLog, "pr create") {
		t.Fatalf("expected existing PR to be updated, not duplicated, got:\n%s", ghLog)
	}
	if strings.Contains(ghLog, "--base epic/feature") {
		t.Fatalf("an ordinary rerun must keep the PR's live base, got:\n%s", ghLog)
	}
}

func TestPRStep_RepoConfigChangeDoesNotRetargetExistingPR(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, logFile := fakeGHWithBase(t, "https://github.com/test/repo/pull/42", "develop")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Config.PR.BaseBranch = "main"

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}

	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logData), "--base main") {
		t.Fatalf("repo-config pr.base_branch must not retarget an existing PR, got:\n%s", logData)
	}
}

func TestPRStep_PrefersPersistedPRWhenFindPRReturnsSibling(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ensureLocalBranch(t, dir, "epic/feature", baseSHA)
	ensureLocalBranch(t, dir, "develop", baseSHA)
	env, logFile := fakeGHWithBase(t, "https://github.com/test/repo/pull/99", "develop")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	runBase := "epic/feature"
	sctx.Run.PRBaseBranch = &runBase
	owned := "https://github.com/test/repo/pull/42"
	sctx.Run.PRURL = &owned
	sctx.PRTarget = &pipeline.PRTargetSelection{PRURL: owned, TargetBranch: "develop"}

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}

	logData, readErr := os.ReadFile(logFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	ghLog := string(logData)
	if strings.Contains(ghLog, "pr edit 99") {
		t.Fatalf("sibling FindPR hit must not be mutated, got:\n%s", ghLog)
	}
	if !strings.Contains(ghLog, "pr edit 42") {
		t.Fatalf("expected persisted PR #42 to be updated, got:\n%s", ghLog)
	}
	if strings.Contains(ghLog, "pr create") {
		t.Fatalf("owned PR must be updated, not duplicated, got:\n%s", ghLog)
	}
}

func TestBindExistingPR_PrefersPersistedURLOverSibling(t *testing.T) {
	t.Parallel()
	owned := "https://github.com/test/repo/pull/42"
	sctx := &pipeline.StepContext{Run: &db.Run{PRURL: &owned}, Log: func(string) {}}
	sibling := &scm.PR{Number: "99", URL: "https://github.com/test/repo/pull/99", BaseBranch: "develop"}
	host := &recordingRetargetHost{state: scm.PRStateOpen}

	got, err := bindExistingPR(sctx, host, sibling)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || !samePRIdentity(owned, got) {
		t.Fatalf("bindExistingPR = %+v, want persisted %s", got, owned)
	}
	if got == sibling {
		t.Fatal("bindExistingPR returned the sibling pointer")
	}
	if host.stateCalls != 1 {
		t.Fatalf("GetPRState calls = %d, want 1 before prefer-owned", host.stateCalls)
	}
}

func TestPRStep_StaleIdentityRefusesRetargetOfEitherPR(t *testing.T) {
	t.Parallel()
	for _, state := range []scm.PRState{scm.PRStateClosed, scm.PRStateMerged} {
		t.Run(string(state), func(t *testing.T) {
			owned := "https://github.com/test/repo/pull/42"
			sctx := &pipeline.StepContext{
				Run: &db.Run{PRURL: &owned, PRBaseBranch: strptr("epic/feature")},
				Log: func(string) {},
			}
			host := &recordingRetargetHost{state: state}
			discovered := &scm.PR{
				Number:     "99",
				URL:        "https://github.com/test/repo/pull/99",
				BaseBranch: "develop",
			}

			bound, err := bindExistingPR(sctx, host, discovered)
			if err == nil {
				t.Fatal("expected stale identity to refuse retarget")
			}
			if bound != nil {
				t.Fatalf("bindExistingPR = %+v, want nil when retarget is refused", bound)
			}
			if host.stateCalls != 1 {
				t.Fatalf("GetPRState calls = %d, want 1", host.stateCalls)
			}
			if host.calls != 0 {
				t.Fatalf("retargeted %s to %s; stale identity must not move either PR", describePR(host.pr), host.base)
			}
			if !strings.Contains(err.Error(), "stale") && !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(string(state))) {
				t.Fatalf("error = %v, want stale/closed/merged identity", err)
			}
		})
	}
}

// recordingRetargetHost records SetPRBaseBranch calls so identity-mismatch
// tests can prove the discovered PR was not moved.
type recordingRetargetHost struct {
	scm.Host
	calls      int
	stateCalls int
	pr         *scm.PR
	base       string
	state      scm.PRState
}

func (h *recordingRetargetHost) SetPRBaseBranch(_ context.Context, pr *scm.PR, base string) error {
	h.calls++
	h.pr = pr
	h.base = base
	return nil
}

func (h *recordingRetargetHost) GetPRState(_ context.Context, _ *scm.PR) (scm.PRState, error) {
	h.stateCalls++
	if h.state == "" {
		return scm.PRStateOpen, nil
	}
	return h.state, nil
}

func strptr(s string) *string { return &s }

func TestPRStep_SkipsWhenBranchEqualsPerRunBase(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, _ := fakeGH(t, "")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.Branch = "epic/feature"
	runBase := "epic/feature"
	sctx.Run.PRBaseBranch = &runBase

	outcome, err := (&PRStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Skipped {
		t.Fatal("expected PR step to skip when branch equals per-run base")
	}
}

func TestCIStep_AutoFixStillPrefersExistingPRForgeBase(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "-b", "develop")
	if err := os.WriteFile(filepath.Join(dir, "develop.txt"), []byte("develop\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "develop")
	developTip := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "develop")

	sctx := newTestContext(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "https://github.com/test/repo.git"
	sctx.Run.Branch = "refs/heads/feature"
	runBase := "epic/feature"
	sctx.Run.PRBaseBranch = &runBase
	sctx.Config.PR.BaseBranch = "main"
	sctx.Config.AutoFix = config.AutoFix{CI: 1}
	pr := &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42", BaseBranch: "develop"}

	var prompt string
	sctx.Agent = &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		prompt = opts.Prompt
		return &agent.Result{}, nil
	}}
	sctx.Env = fakeCIGHMergeable(t, "OPEN", `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`, "CONFLICTING")
	host, skip := buildHost(sctx, scm.ProviderGitHub)
	if host == nil {
		t.Fatal(skip)
	}
	if _, err := (&CIStep{}).autoFixCI(sctx, host, pr, ciTargetsFor(nil, true)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "base commit: "+developTip) {
		t.Fatalf("expected existing PR forge base %s to win over per-run override, got:\n%s", developTip, prompt)
	}
}

func TestRebaseStep_UsesPerRunPRBaseBranch(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base")
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "epic/feature")
	if err := os.WriteFile(filepath.Join(dir, "epic.txt"), []byte("epic\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "epic integration")
	epicSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "epic/feature")

	gitCmd(t, dir, "checkout", "-b", "task")
	if err := os.WriteFile(filepath.Join(dir, "task.txt"), []byte("task\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "task")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, epicSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/task"
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.PR.BaseBranch = "main"
	runBase := "epic/feature"
	sctx.Run.PRBaseBranch = &runBase

	outcome, err := (&RebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("unexpected approval: %s", outcome.Findings)
	}
	if got := gitCmd(t, dir, "merge-base", "HEAD", "origin/epic/feature"); got != epicSHA {
		t.Fatalf("merge-base with per-run base = %s, want epic %s", got, epicSHA)
	}
}
