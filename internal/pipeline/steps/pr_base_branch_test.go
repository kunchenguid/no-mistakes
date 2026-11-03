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
	"github.com/kunchenguid/no-mistakes/internal/types"
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

	sctx := newPRTestContext(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
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

	sctx := newPRTestContext(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
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

func TestPRStep_RetargetsExistingPRWhenPerRunBaseDiffers(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ensureLocalBranch(t, dir, "epic/feature", baseSHA)
	env, logFile := fakeGHWithBase(t, "https://github.com/test/repo/pull/42", "develop")

	sctx := newPRTestContext(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	runBase := "epic/feature"
	sctx.Run.PRBaseBranch = &runBase
	owned := "https://github.com/test/repo/pull/42"
	sctx.Run.PRURL = &owned

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}

	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	ghLog := string(logData)
	if strings.Contains(ghLog, "pr create") {
		t.Fatalf("expected existing PR to be retargeted, not duplicated, got:\n%s", ghLog)
	}
	if !strings.Contains(ghLog, "pr edit") {
		t.Fatalf("expected gh pr edit, got:\n%s", ghLog)
	}
	if !strings.Contains(ghLog, "pr edit 42 --repo test/repo --base epic/feature") && !strings.Contains(ghLog, "--base epic/feature") {
		t.Fatalf("expected retarget --base epic/feature, got:\n%s", ghLog)
	}
}

func TestPRStep_RepoConfigChangeDoesNotRetargetExistingPR(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, logFile := fakeGHWithBase(t, "https://github.com/test/repo/pull/42", "develop")

	sctx := newPRTestContext(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
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

func TestRetargetExistingPRIfNeeded_MismatchedIdentityFailsClosed(t *testing.T) {
	t.Parallel()
	owned := "https://github.com/test/repo/pull/42"
	sctx := &pipeline.StepContext{Run: &db.Run{PRURL: &owned}, Log: func(string) {}}
	host := &recordingRetargetHost{}
	discovered := &scm.PR{
		Number:     "99",
		URL:        "https://github.com/test/repo/pull/99",
		BaseBranch: "develop",
	}

	err := retargetExistingPRIfNeeded(sctx, host, discovered, "epic/feature")
	if err == nil {
		t.Fatal("expected error when discovered PR is not the run's persisted PR")
	}
	if host.calls != 0 {
		t.Fatalf("retargeted %s to %s; a mismatched pull request must not be moved", describePR(host.pr), host.base)
	}
	if !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("error = %v, want identity mismatch", err)
	}
}

func TestRetargetExistingPRIfNeeded_MissingIdentityFailsClosed(t *testing.T) {
	t.Parallel()
	sctx := &pipeline.StepContext{Run: &db.Run{}, Log: func(string) {}}
	host := &recordingRetargetHost{}
	discovered := &scm.PR{
		Number:     "99",
		URL:        "https://github.com/test/repo/pull/99",
		BaseBranch: "develop",
	}

	err := retargetExistingPRIfNeeded(sctx, host, discovered, "epic/feature")
	if err == nil {
		t.Fatal("expected error when the run has no persisted PR identity")
	}
	if host.calls != 0 {
		t.Fatalf("retargeted %s to %s; an unproven pull request must not be moved", describePR(host.pr), host.base)
	}
	if !strings.Contains(err.Error(), "no persisted PR identity") {
		t.Fatalf("error = %v, want missing identity", err)
	}
}

func TestRetargetExistingPRIfNeeded_MatchingIdentityRetargets(t *testing.T) {
	t.Parallel()
	owned := "https://github.com/test/repo/pull/42"
	sctx := &pipeline.StepContext{Run: &db.Run{PRURL: &owned}, Log: func(string) {}}
	host := &recordingRetargetHost{}
	existing := &scm.PR{
		Number:     "42",
		URL:        owned,
		BaseBranch: "develop",
	}

	if err := retargetExistingPRIfNeeded(sctx, host, existing, "epic/feature"); err != nil {
		t.Fatal(err)
	}
	if host.calls != 1 || host.base != "epic/feature" {
		t.Fatalf("retargets = %d base %q, want 1 call to epic/feature", host.calls, host.base)
	}
}

func TestPRStep_PrefersPersistedPRWhenFindPRReturnsSibling(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ensureLocalBranch(t, dir, "epic/feature", baseSHA)
	env, logFile := fakeGHWithBase(t, "https://github.com/test/repo/pull/99", "develop")

	sctx := newPRTestContext(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	runBase := "epic/feature"
	sctx.Run.PRBaseBranch = &runBase
	owned := "https://github.com/test/repo/pull/42"
	sctx.Run.PRURL = &owned

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}

	logData, readErr := os.ReadFile(logFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	ghLog := string(logData)
	if strings.Contains(ghLog, "pulls/99") {
		t.Fatalf("sibling FindPR hit must not be mutated, got:\n%s", ghLog)
	}
	if !strings.Contains(ghLog, "pulls/42") {
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

func TestRetargetExistingPRIfNeeded_ProviderWithoutRetargetFailsClosed(t *testing.T) {
	t.Parallel()
	sctx := &pipeline.StepContext{Run: &db.Run{}}
	host := nonRetargetHost{}
	cases := []struct {
		name string
		pr   *scm.PR
	}{
		{
			name: "unknown live base",
			pr:   &scm.PR{Number: "7", URL: "https://gitea.example.com/owner/repo/pulls/7"},
		},
		{
			name: "known mismatched base",
			pr:   &scm.PR{Number: "7", URL: "https://gitea.example.com/owner/repo/pulls/7", BaseBranch: "main"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owned := tc.pr.URL
			sctx.Run.PRURL = &owned
			err := retargetExistingPRIfNeeded(sctx, host, tc.pr, "epic/feature")
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), "cannot retarget") {
				t.Fatalf("error = %v, want cannot retarget", err)
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

// nonRetargetHost is a scm.Host that does not implement PRBaseRetargeter, so
// a per-run --base-branch override cannot silently skip when the live forge
// base is missing or disagrees.
type nonRetargetHost struct{ scm.Host }

func TestPRStep_SkipsWhenBranchEqualsPerRunBase(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, _ := fakeGH(t, "")

	sctx := newPRTestContext(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
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

	sctx := newPRTestContext(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
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

	sctx := newPRTestContext(t, &mockAgent{name: "test"}, dir, epicSHA, headSHA, config.Commands{})
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

// The base the CI step observed on the pull request scopes the restarted
// validation; it must never become a retarget request. runs.pr_base_branch is
// read as explicit operator intent (PRStep retargets to it and bindExistingPR
// refuses a closed PR because of it), so an observation written there started a
// retarget war after the next stack merge. With only an observed base and no
// operator override, the PR step leaves the pull request where the forge put it.
func TestPRStep_ObservedBaseNeverRetargets(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	observed := "layer2"
	// The pull request already targets main, the way a partial stack merge
	// retargets it; the run observed layer2 earlier and scopes by it.
	env, logFile := fakeGHWithBase(t, "https://github.com/test/repo/pull/42", "main")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.ObservedBaseBranch = &observed
	owned := "https://github.com/test/repo/pull/42"
	sctx.Run.PRURL = &owned

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}

	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	ghLog := string(logData)
	if strings.Contains(ghLog, "pr create") {
		t.Fatalf("expected the existing PR to be kept, not duplicated, got:\n%s", ghLog)
	}
	if strings.Contains(ghLog, "--base layer2") || strings.Contains(ghLog, "layer2") {
		t.Fatalf("an observed base became a retarget request, got:\n%s", ghLog)
	}
	if got := effectivePRBaseBranch(sctx); got != observed {
		t.Fatalf("scoping base = %q, want the observed %q", got, observed)
	}

	// Creating a pull request is target intent too: a new one lands on the
	// configured base, never on the observation.
	createDir, createBase, createHead := setupGitRepo(t)
	createEnv, createLog := fakeGH(t, "")
	createCtx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, createDir, createBase, createHead, config.Commands{})
	createCtx.Env = createEnv
	createCtx.Run.ObservedBaseBranch = &observed
	if _, err := (&PRStep{}).Execute(createCtx); err != nil {
		t.Fatal(err)
	}
	createData, err := os.ReadFile(createLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(createData), "pr create --head feature --base main") {
		t.Fatalf("new pull request did not target the configured base, got:\n%s", createData)
	}
	if strings.Contains(string(createData), "--base "+observed) {
		t.Fatalf("new pull request targeted the observed base, got:\n%s", createData)
	}
}

func TestPRStep_StackMergeDeletesRecordedBase(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{config.PRUpdateTextRegenerate, config.PRUpdateTextKeep} {
		t.Run(mode, func(t *testing.T) {
			dir, baseSHA, layerSHA := setupGitRepo(t)
			gitCmd(t, dir, "branch", "layer1", layerSHA)
			gitCmd(t, dir, "branch", "-f", "main", layerSHA)
			if err := os.WriteFile(filepath.Join(dir, "layer2.txt"), []byte("layer two\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", "layer2.txt")
			gitCmd(t, dir, "commit", "-m", "add layer two")
			headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
			gitCmd(t, dir, "branch", "-D", "layer1")

			owned := "https://github.com/test/repo/pull/42"
			env, logFile := fakeGHWithBase(t, owned, "main")
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: []byte(`{"title":"feat: add layer two","body":"## What Changed\n- Add layer two."}`)}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.Env = env
			sctx.Config.PR.BaseBranch = "layer1"
			sctx.Config.PR.UpdateText = mode
			sctx.Config.PR.TitleFormat = "Use a conventional commit title"
			sctx.Run.PRURL = &owned
			observed := "main"
			sctx.Run.ObservedBaseBranch = &observed
			review, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
			if err != nil {
				t.Fatal(err)
			}
			if err := sctx.DB.UpdateStepStatus(review.ID, types.StepStatusCompleted); err != nil {
				t.Fatal(err)
			}

			outcome, err := (&PRStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.PRURL != owned {
				t.Fatalf("published PR = %q, want %q", outcome.PRURL, owned)
			}
			if len(ag.calls) == 0 {
				t.Fatal("publication did not draft content")
			}
			prompt := ag.calls[0].Prompt
			if !strings.Contains(prompt, "- base commit: "+layerSHA) || !strings.Contains(prompt, "layer2.txt") || strings.Contains(prompt, "feature.txt") {
				t.Fatalf("publication must describe only the surviving layer:\n%s", prompt)
			}
			data, err := os.ReadFile(logFile)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), "stdin --body ") || strings.Contains(string(data), "pr create") || strings.Contains(string(data), "--base layer1") || strings.Contains(string(data), "--base main") {
				t.Fatalf("expected an in-place update without retargeting:\n%s", data)
			}
			if sctx.Run.PRBaseBranch != nil || sctx.Config.PR.BaseBranch != "layer1" {
				t.Fatal("publication changed operator target intent")
			}
		})
	}
}
