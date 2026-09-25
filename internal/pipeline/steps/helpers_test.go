package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"github.com/kunchenguid/no-mistakes/internal/scm/bitbucket"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

var testGitExecutable, testGitErr = testgit.RealGit()

type mockAgent struct {
	name  string
	runFn func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error)
	calls []agent.RunOpts
}

func (m *mockAgent) Name() string { return m.name }

func (m *mockAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	m.calls = append(m.calls, opts)
	if m.runFn != nil {
		return m.runFn(ctx, opts)
	}
	return &agent.Result{}, nil
}

func (m *mockAgent) Close() error { return nil }

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
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

func gitStatusPorcelain(t *testing.T, dir string) string {
	t.Helper()
	return gitCmd(t, dir, "status", "--porcelain")
}

func lastCommitMessage(t *testing.T, dir string) string {
	t.Helper()
	return gitCmd(t, dir, "log", "-1", "--pretty=%s")
}

// gitRepoTemplate holds a cached template repo that setupGitRepo copies from
// instead of running git init + config + commits each time.
var gitRepoTemplate struct {
	once    sync.Once
	dir     string
	baseSHA string
	headSHA string
}

func ensureGitRepoTemplate(t *testing.T) {
	t.Helper()
	gitRepoTemplate.once.Do(func() {
		dir, err := os.MkdirTemp("", "git-template-*")
		if err != nil {
			t.Fatal(err)
		}

		run := func(args ...string) string {
			cmd := exec.Command("git", args...)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(),
				"GIT_AUTHOR_NAME=test",
				"GIT_AUTHOR_EMAIL=test@test.com",
				"GIT_COMMITTER_NAME=test",
				"GIT_COMMITTER_EMAIL=test@test.com",
			)
			out, err := cmd.CombinedOutput()
			if err != nil {
				panic(fmt.Sprintf("git %v: %v: %s", args, err, out))
			}
			return strings.TrimSpace(string(out))
		}

		run("init")
		run("config", "user.name", "test")
		run("config", "user.email", "test@test.com")
		run("checkout", "-b", "main")

		os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base content"), 0o644)
		run("add", "-A")
		run("commit", "-m", "base commit")
		gitRepoTemplate.baseSHA = run("rev-parse", "HEAD")

		run("checkout", "-b", "feature")
		os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature code\n"), 0o644)
		run("add", "-A")
		run("commit", "-m", "add feature")
		gitRepoTemplate.headSHA = run("rev-parse", "HEAD")

		gitRepoTemplate.dir = dir
	})
}

// setupGitRepo creates a git repo with a base commit on main and a head commit on feature.
// Returns (repoDir, baseSHA, headSHA).
// Uses a cached template repo and copies it via cp -a for speed.
func setupGitRepo(t *testing.T) (string, string, string) {
	t.Helper()
	ensureGitRepoTemplate(t)

	dir := t.TempDir()
	if err := copyDirContents(gitRepoTemplate.dir, dir); err != nil {
		t.Fatalf("copy template repo: %v", err)
	}

	return dir, gitRepoTemplate.baseSHA, gitRepoTemplate.headSHA
}

// ensureHermeticOrigin gives a test repo that lacks an "origin" remote a
// local one (itself) so incidental upstream fetches stay hermetic instead of
// reaching the network. Without this, tests that never configure a real
// remote fetch the placeholder github.com/test/repo URL - which used to be
// harmless because a failed fetch degraded silently, but resolveBranchBaseSHA
// now refuses on fetch failure (#997/#1147), so every step test needs a
// fetchable base branch ref.
func ensureHermeticOrigin(t *testing.T, workDir string) {
	t.Helper()
	if testGitExecutable == "" {
		return
	}
	gitDir, err := os.Stat(filepath.Join(workDir, ".git"))
	if err != nil || !gitDir.IsDir() {
		return
	}
	if cmd := exec.Command(testGitExecutable, "-C", workDir, "remote", "get-url", "origin"); cmd.Run() != nil {
		cmd = exec.Command(testGitExecutable, "-C", workDir, "remote", "add", "origin", workDir)
		if output, addErr := cmd.CombinedOutput(); addErr != nil {
			t.Fatalf("add hermetic test origin: %v: %s", addErr, output)
		}
	}
}

// ensureLocalBranch creates branch pointing at ref in workDir, unless it
// already exists. Tests that configure a base branch other than "main" (e.g.
// "develop", "epic/feature") need that ref to actually exist locally: with
// ensureHermeticOrigin pointing "origin" at the worktree itself,
// resolveBranchBaseSHA's base-branch fetch now fails closed on a missing ref
// (#997/#1147) instead of silently degrading.
func ensureLocalBranch(t *testing.T, workDir, branch, ref string) {
	t.Helper()
	check := exec.Command(testGitExecutable, "-C", workDir, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	if check.Run() == nil {
		return
	}
	cmd := exec.Command(testGitExecutable, "-C", workDir, "branch", branch, ref)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create local branch %s: %v: %s", branch, err, output)
	}
}

// newTestContext creates a StepContext for testing with optional config overrides.
func newTestContext(t *testing.T, ag agent.Agent, workDir, baseSHA, headSHA string, cmds config.Commands) *pipeline.StepContext {
	t.Helper()
	if testGitErr != nil {
		t.Fatal(testGitErr)
	}
	ensureHermeticOrigin(t, workDir)

	dbPath := filepath.Join(t.TempDir(), "test.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	return &pipeline.StepContext{
		Ctx:  context.Background(),
		Run:  &db.Run{ID: "run-1", RepoID: "repo-1", Branch: "refs/heads/feature", HeadSHA: headSHA, BaseSHA: baseSHA},
		Repo: &db.Repo{ID: "repo-1", WorkingPath: workDir, UpstreamURL: "https://github.com/test/repo", DefaultBranch: "main"},
		// The executor resolves this from the app root in production. Tests get
		// a per-test directory so a step under test can never write evidence
		// into a shared location the next test would then observe.
		EvidenceDir: filepath.Join(t.TempDir(), "evidence", "run-1"),
		WorkDir:     workDir,
		Agent:       ag,
		Config:      &config.Config{Agent: types.AgentClaude, Commands: cmds},
		DB:          database,
		Log:         func(s string) { t.Log(s) },
		LogChunk:    func(s string) {},
		LogFile:     func(s string) {},
	}
}

// fakeCLIEnv builds environment variable entries for a fake CLI binary and PATH override.
// Returns env entries that should be set on StepContext.Env for parallel-safe tests.
func fakeCLIEnv(binDir string, vars map[string]string) []string {
	if testGitErr != nil {
		panic(testGitErr)
	}
	env := []string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"FAKE_CLI_REAL_GIT=" + testGitExecutable,
		"FAKE_CLI_HEAD_FROM_WORKTREE=1",
	}
	for k, v := range vars {
		env = append(env, k+"="+v)
	}
	return env
}

// fakeCLIBinDir creates a temporary directory for fake CLI binaries.
// Unlike t.TempDir(), cleanup tolerates file locks from recently-executed
// binaries on Windows (which prevent immediate deletion).
func fakeCLIBinDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "fakecli")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for i := 0; i < 10; i++ {
			if err := os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	})
	return dir
}

// linkTestBinary places the tiny fake-CLI helper on PATH under name.
func linkTestBinary(t *testing.T, binDir, name string) {
	t.Helper()
	stepstest.LinkFakeCLI(t, binDir, name)
}

// fakeGH creates a mock gh binary in a temp dir and returns env entries for StepContext.Env.
// The binary records all invocations to a log file and responds based on subcommand.
func fakeGH(t *testing.T, prViewURL string) (env []string, logFile string) {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	logFile = filepath.Join(t.TempDir(), "gh.log")
	linkTestBinary(t, binDir, "gh")
	env = fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":   "gh",
		"FAKE_CLI_LOG":    logFile,
		"FAKE_CLI_PR_URL": prViewURL,
	})
	return env, logFile
}

// fakeGHWithBase behaves like fakeGH but additionally records the existing
// PR's actual base branch, so the fake `gh pr list --base X` only returns the
// PR when X matches it - mirroring GitHub's server-side base filtering.
func fakeGHWithBase(t *testing.T, prViewURL, prBase string) (env []string, logFile string) {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	logFile = filepath.Join(t.TempDir(), "gh.log")
	linkTestBinary(t, binDir, "gh")
	env = fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":    "gh",
		"FAKE_CLI_LOG":     logFile,
		"FAKE_CLI_PR_URL":  prViewURL,
		"FAKE_CLI_PR_BASE": prBase,
	})
	return env, logFile
}

// fakeBitbucketAPI is the twg-CLI-backed stand-in for Bitbucket Cloud used by
// pipeline-step tests. State lives in a JSON file (rather than in memory)
// because each fake `twg` invocation is a fresh subprocess; see
// internal/pipeline/fakecli's bbFakeState for the file's shape. Call counts
// are recovered from the fake CLI's invocation log (FAKE_CLI_LOG), which
// internal/pipeline/fakecli writes unconditionally for every mode.
type fakeBitbucketAPI struct {
	t           *testing.T
	binDir      string
	logFile     string
	statePath   string
	env         map[string]string
	existingID  int
	existingURL string
}

// newFakeBitbucketAPI wires up a fake `twg` binary on PATH. existingPRID == 0
// means no PR exists yet (the PR step should create one); a nonzero id seeds
// query/get with an already-open PR at existingPRURL.
func newFakeBitbucketAPI(t *testing.T, existingPRID int, existingPRURL string) *fakeBitbucketAPI {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "twg")

	statePath := filepath.Join(t.TempDir(), "bb-state.json")
	state := bbFakeStateForTest{Exists: existingPRID != 0, ID: existingPRID, URL: existingPRURL}
	if state.Exists {
		// GetPRContent (the author-preserving read-before-write path) rejects
		// an empty title as an incomplete response, so a seeded pre-existing
		// PR needs one - mirroring what the old HTTP fake always returned.
		state.Title = "Existing title"
		state.Description = "Existing unconfigured description"
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	return &fakeBitbucketAPI{
		t:           t,
		binDir:      binDir,
		logFile:     filepath.Join(t.TempDir(), "twg.log"),
		statePath:   statePath,
		env:         map[string]string{},
		existingID:  existingPRID,
		existingURL: existingPRURL,
	}
}

// env builds the StepContext.Env entries for this fake.
func (api *fakeBitbucketAPI) Env() []string {
	vars := map[string]string{
		"FAKE_CLI_MODE":          "twg",
		"FAKE_CLI_LOG":           api.logFile,
		"FAKE_CLI_BB_STATE_FILE": api.statePath,
	}
	for k, v := range api.env {
		vars[k] = v
	}
	return fakeCLIEnv(api.binDir, vars)
}

// withPRState sets the PR lifecycle state ("get" reads it back).
func (api *fakeBitbucketAPI) withPRState(state string) *fakeBitbucketAPI {
	api.env["FAKE_CLI_BB_PR_STATE"] = state
	api.updateState(func(s *bbFakeStateForTest) { s.State = state })
	return api
}

// withStatuses seeds the `_statuses` array `pull-requests get --statuses` returns.
func (api *fakeBitbucketAPI) withStatuses(statusesJSON string) *fakeBitbucketAPI {
	api.env["FAKE_CLI_BB_STATUSES_JSON"] = statusesJSON
	return api
}

// withPipelineLog seeds the failed-step log text `pipeline get --pipeline
// <buildNumber>` returns. buildNumber == "" sets the default log used when no
// build-number-specific log is configured.
func (api *fakeBitbucketAPI) withPipelineLog(buildNumber, log string) *fakeBitbucketAPI {
	key := "FAKE_CLI_BB_PIPELINE_LOG"
	if buildNumber != "" {
		key += "_" + buildNumber
	}
	api.env[key] = log
	return api
}

type bbFakeStateForTest struct {
	Exists      bool   `json:"exists"`
	ID          int    `json:"id"`
	URL         string `json:"url"`
	State       string `json:"state"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Puts        int    `json:"puts"`
}

func (api *fakeBitbucketAPI) updateState(mutate func(*bbFakeStateForTest)) {
	api.t.Helper()
	data, err := os.ReadFile(api.statePath)
	if err != nil {
		api.t.Fatal(err)
	}
	var state bbFakeStateForTest
	if err := json.Unmarshal(data, &state); err != nil {
		api.t.Fatal(err)
	}
	mutate(&state)
	encoded, err := json.Marshal(state)
	if err != nil {
		api.t.Fatal(err)
	}
	if err := os.WriteFile(api.statePath, encoded, 0o644); err != nil {
		api.t.Fatal(err)
	}
}

func (api *fakeBitbucketAPI) readState(t *testing.T) bbFakeStateForTest {
	t.Helper()
	data, err := os.ReadFile(api.statePath)
	if err != nil {
		t.Fatal(err)
	}
	var state bbFakeStateForTest
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func (api *fakeBitbucketAPI) title(t *testing.T) string { return api.readState(t).Title }

func (api *fakeBitbucketAPI) puts(t *testing.T) int { return api.readState(t).Puts }

func (api *fakeBitbucketAPI) setTitle(t *testing.T, title string) {
	api.updateState(func(s *bbFakeStateForTest) { s.Title = title })
}

func (api *fakeBitbucketAPI) setDescription(t *testing.T, description string) {
	api.updateState(func(s *bbFakeStateForTest) { s.Description = description })
}

// lastDescription reads the description twg's most recent create/update
// invocation wrote to the fake's on-disk state.
func (api *fakeBitbucketAPI) lastDescription(t *testing.T) string {
	t.Helper()
	return api.readState(t).Description
}

// logCount returns how many recorded twg invocations contain substr,
// recovering call counts the old HTTP fake tracked with explicit counters.
func (api *fakeBitbucketAPI) logCount(substr string) int {
	api.t.Helper()
	data, err := os.ReadFile(api.logFile)
	if err != nil {
		return 0
	}
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, substr) {
			count++
		}
	}
	return count
}

// fakeBitbucketCmdFactory builds a bitbucket.CmdFactory that runs api's fake
// twg binary directly, for tests that construct a bitbucket.Host outside a
// full StepContext.
func fakeBitbucketCmdFactory(t *testing.T, api *fakeBitbucketAPI) bitbucket.CmdFactory {
	t.Helper()
	env := api.Env()
	binName := "twg"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binPath := filepath.Join(api.binDir, binName)
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		bin := binPath
		if name != "twg" {
			bin = name
		}
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Env = env
		return cmd
	}
}

func fakeGlab(t *testing.T, mrViewJSON string) (env []string, logFile string) {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	logFile = filepath.Join(t.TempDir(), "glab.log")
	linkTestBinary(t, binDir, "glab")
	env = fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":         "glab",
		"FAKE_CLI_LOG":          logFile,
		"FAKE_CLI_MR_VIEW_JSON": mrViewJSON,
	})
	return env, logFile
}

// newTestContextWithDBRecords is like newTestContext but also inserts
// repo and run records into the database so GetRun works after updates.
func recordReviewApproval(t *testing.T, sctx *pipeline.StepContext, headSHA string) {
	t.Helper()
	if err := sctx.DB.UpdateRunReviewApprovedHeadSHA(sctx.Run.ID, headSHA); err != nil {
		t.Fatal(err)
	}
	approved := headSHA
	sctx.Run.ReviewApprovedHeadSHA = &approved
}

func newTestContextWithDBRecords(t *testing.T, ag agent.Agent, workDir, baseSHA, headSHA string, cmds config.Commands) *pipeline.StepContext {
	t.Helper()
	sctx := newTestContext(t, ag, workDir, baseSHA, headSHA, cmds)

	// Insert repo + run records so DB queries work
	repo, err := sctx.DB.InsertRepo(workDir, "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := sctx.DB.InsertRun(repo.ID, "refs/heads/feature", headSHA, baseSHA)
	if err != nil {
		t.Fatal(err)
	}
	sctx.Run = run
	sctx.Repo = repo
	return sctx
}

// fakeCIGH creates a fake gh binary that responds to CI-related
// commands (pr view --json state, pr checks --json, pr view --json comments).
func fakeCIGH(t *testing.T, state, checksJSON string) []string {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "gh")
	return fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":        "ci-gh",
		"FAKE_CLI_STATE":       state,
		"FAKE_CLI_CHECKS":      checksJSON,
		"FAKE_CLI_PR_HEAD_SHA": "deadbeef",
	})
}

func fakeCIGHMergeable(t *testing.T, state, checksJSON, mergeable string) []string {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "gh")
	return fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":        "ci-gh",
		"FAKE_CLI_STATE":       state,
		"FAKE_CLI_CHECKS":      checksJSON,
		"FAKE_CLI_MERGEABLE":   mergeable,
		"FAKE_CLI_PR_HEAD_SHA": "deadbeef",
	})
}

func fakeCIGHMergeableError(t *testing.T, state, checksJSON, mergeableErr string) []string {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "gh")
	return fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":          "ci-gh",
		"FAKE_CLI_STATE":         state,
		"FAKE_CLI_CHECKS":        checksJSON,
		"FAKE_CLI_MERGEABLE_ERR": mergeableErr,
		"FAKE_CLI_PR_HEAD_SHA":   "deadbeef",
	})
}

func fakeCIGHStateError(t *testing.T, stateErr, checksJSON string) []string {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "gh")
	return fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":        "ci-gh",
		"FAKE_CLI_STATE_ERR":   stateErr,
		"FAKE_CLI_CHECKS":      checksJSON,
		"FAKE_CLI_PR_HEAD_SHA": "deadbeef",
	})
}

func fakeCIGHChecksError(t *testing.T, state, mergeable, checksErr string) []string {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "gh")
	return fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":        "ci-gh",
		"FAKE_CLI_STATE":       state,
		"FAKE_CLI_MERGEABLE":   mergeable,
		"FAKE_CLI_CHECKS_ERR":  checksErr,
		"FAKE_CLI_PR_HEAD_SHA": "deadbeef",
	})
}

func fakeCIGHSequenceMergeable(t *testing.T, state string, checks []string, mergeable string) []string {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "gh")

	checksPath := filepath.Join(t.TempDir(), "checks.txt")
	indexPath := filepath.Join(t.TempDir(), "checks-index.txt")

	if err := os.WriteFile(checksPath, []byte(strings.Join(checks, "\n")), 0o644); err != nil {
		t.Fatalf("write checks sequence: %v", err)
	}
	if err := os.WriteFile(indexPath, []byte("0"), 0o644); err != nil {
		t.Fatalf("write checks index: %v", err)
	}

	return fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":              "ci-gh-seq",
		"FAKE_CLI_STATE":             state,
		"FAKE_CLI_CHECKS_PATH":       checksPath,
		"FAKE_CLI_CHECKS_INDEX_PATH": indexPath,
		"FAKE_CLI_MERGEABLE":         mergeable,
		"FAKE_CLI_PR_HEAD_SHA":       "deadbeef",
	})
}

func fakeCIGHSequence(t *testing.T, state string, checks []string) []string {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "gh")

	checksPath := filepath.Join(t.TempDir(), "checks.txt")
	indexPath := filepath.Join(t.TempDir(), "checks-index.txt")

	if err := os.WriteFile(checksPath, []byte(strings.Join(checks, "\n")), 0o644); err != nil {
		t.Fatalf("write checks sequence: %v", err)
	}
	if err := os.WriteFile(indexPath, []byte("0"), 0o644); err != nil {
		t.Fatalf("write checks index: %v", err)
	}

	return fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":              "ci-gh-seq",
		"FAKE_CLI_STATE":             state,
		"FAKE_CLI_CHECKS_PATH":       checksPath,
		"FAKE_CLI_CHECKS_INDEX_PATH": indexPath,
		"FAKE_CLI_PR_HEAD_SHA":       "deadbeef",
	})
}

// fakeCIGHLoggedSequence is fakeCIGHSequence with a recorded argv log, so tests
// can assert which gh commands the CI monitor issued (for example whether it
// asked for a check rerun). mergeable overrides the reported mergeable state
// ("" reports MERGEABLE); rerunErr, when set, makes `gh run rerun` fail.
func fakeCIGHLoggedSequence(t *testing.T, state string, checks []string, mergeable, rerunErr string) (env []string, logFile string) {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "gh")

	tempDir := t.TempDir()
	checksPath := filepath.Join(tempDir, "checks.txt")
	indexPath := filepath.Join(tempDir, "checks-index.txt")
	logFile = filepath.Join(tempDir, "gh.log")

	if err := os.WriteFile(checksPath, []byte(strings.Join(checks, "\n")), 0o644); err != nil {
		t.Fatalf("write checks sequence: %v", err)
	}
	if err := os.WriteFile(indexPath, []byte("0"), 0o644); err != nil {
		t.Fatalf("write checks index: %v", err)
	}

	return fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":              "ci-gh-seq",
		"FAKE_CLI_STATE":             state,
		"FAKE_CLI_CHECKS_PATH":       checksPath,
		"FAKE_CLI_CHECKS_INDEX_PATH": indexPath,
		"FAKE_CLI_MERGEABLE":         mergeable,
		"FAKE_CLI_LOG":               logFile,
		"FAKE_CLI_RERUN_ERR":         rerunErr,
		"FAKE_CLI_PR_HEAD_SHA":       "deadbeef",
	}), logFile
}

func fakeCIGHNoChecks(t *testing.T) []string {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "gh")
	return fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":        "ci-gh-nochecks",
		"FAKE_CLI_PR_HEAD_SHA": "deadbeef",
	})
}

// fakeCIGlab creates a fake glab binary that serves the CI monitoring endpoints.
// state is the MR state ("opened", "merged", "closed"); checksJSON is a JSON
// array of jobs for `glab ci status` / `glab ci get`.
func fakeCIGlab(t *testing.T, state, checksJSON string) []string {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "glab")
	return fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":   "ci-glab",
		"FAKE_CLI_STATE":  state,
		"FAKE_CLI_CHECKS": checksJSON,
	})
}

func fakeCIGlabConflict(t *testing.T, state, checksJSON string, conflict bool) []string {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "glab")
	conflicts := "false"
	if conflict {
		conflicts = "true"
	}
	return fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":         "ci-glab",
		"FAKE_CLI_STATE":        state,
		"FAKE_CLI_CHECKS":       checksJSON,
		"FAKE_CLI_MR_CONFLICTS": conflicts,
	})
}

func fakeCIGlabWithTrace(t *testing.T, state, checksJSON, trace string) []string {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "glab")
	return fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":   "ci-glab",
		"FAKE_CLI_STATE":  state,
		"FAKE_CLI_CHECKS": checksJSON,
		"FAKE_CLI_TRACE":  trace,
	})
}

func fakeCIGlabSequence(t *testing.T, state string, checks []string) []string {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "glab")

	checksPath := filepath.Join(t.TempDir(), "checks.txt")
	indexPath := filepath.Join(t.TempDir(), "checks-index.txt")

	if err := os.WriteFile(checksPath, []byte(strings.Join(checks, "\n")), 0o644); err != nil {
		t.Fatalf("write checks sequence: %v", err)
	}
	if err := os.WriteFile(indexPath, []byte("0"), 0o644); err != nil {
		t.Fatalf("write checks index: %v", err)
	}

	return fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":              "ci-glab-seq",
		"FAKE_CLI_STATE":             state,
		"FAKE_CLI_CHECKS_PATH":       checksPath,
		"FAKE_CLI_CHECKS_INDEX_PATH": indexPath,
	})
}

// runGitDirect runs git without any of the repository's step helpers, so a test
// can observe what a plain, hook-verified git invocation does in dir.
func runGitDirect(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
