package steps

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
	"github.com/kunchenguid/no-mistakes/internal/types"
)

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

// newTestContext creates a StepContext for testing with optional config overrides.
func newTestContext(t *testing.T, ag agent.Agent, workDir, baseSHA, headSHA string, cmds config.Commands) *pipeline.StepContext {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	return &pipeline.StepContext{
		Ctx:      context.Background(),
		Run:      &db.Run{ID: "run-1", RepoID: "repo-1", Branch: "refs/heads/feature", HeadSHA: headSHA, BaseSHA: baseSHA},
		Repo:     &db.Repo{ID: "repo-1", WorkingPath: workDir, UpstreamURL: "https://github.com/test/repo", DefaultBranch: "main"},
		WorkDir:  workDir,
		Agent:    ag,
		Config:   &config.Config{Agent: types.AgentClaude, Commands: cmds},
		DB:       database,
		Log:      func(s string) {},
		LogChunk: func(s string) {},
		LogFile:  func(s string) {},
	}
}

// fakeCLIEnv builds environment variable entries for a fake CLI binary and PATH override.
// Returns env entries that should be set on StepContext.Env for parallel-safe tests.
func fakeCLIEnv(binDir string, vars map[string]string) []string {
	env := []string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
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

// linkTestBinary creates a hard link (or copy) of the current test binary
// with the given name in binDir. On Windows, .exe is appended.
func linkTestBinary(t *testing.T, binDir, name string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dst := filepath.Join(binDir, name)
	if err := os.Link(exe, dst); err != nil {
		// Fallback to copy if hard link fails (cross-device, etc.)
		data, readErr := os.ReadFile(exe)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if err := os.WriteFile(dst, data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
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

type fakeBitbucketPRAPI struct {
	server         *httptest.Server
	listCalls      int
	createCalls    int
	updateCalls    int
	lastAuthHeader string
	lastCreateBody string
	lastUpdateBody string
	existingPRID   int
	existingPRURL  string
	createdPRURL   string
}

func newFakeBitbucketPRAPI(t *testing.T, existingPRID int, existingPRURL string) *fakeBitbucketPRAPI {
	t.Helper()

	api := &fakeBitbucketPRAPI{
		existingPRID:  existingPRID,
		existingPRURL: existingPRURL,
		createdPRURL:  "https://bitbucket.org/test/repo/pull-requests/99",
	}

	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.lastAuthHeader = r.Header.Get("Authorization")

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/2.0/repositories/test/repo/pullrequests":
			api.listCalls++
			w.Header().Set("Content-Type", "application/json")
			if api.existingPRID == 0 {
				fmt.Fprint(w, `{"values":[]}`)
				return
			}
			fmt.Fprintf(w, `{"values":[{"id":%d,"links":{"html":{"href":%q}}}]}`,
				api.existingPRID,
				api.existingPRURL,
			)
		case r.Method == http.MethodPost && r.URL.Path == "/2.0/repositories/test/repo/pullrequests":
			api.createCalls++
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read create body: %v", err)
			}
			api.lastCreateBody = string(body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":99,"links":{"html":{"href":%q}}}`,
				api.createdPRURL,
			)
		case r.Method == http.MethodPut && r.URL.Path == fmt.Sprintf("/2.0/repositories/test/repo/pullrequests/%d", api.existingPRID):
			api.updateCalls++
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read update body: %v", err)
			}
			api.lastUpdateBody = string(body)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":%d,"links":{"html":{"href":%q}}}`,
				api.existingPRID,
				api.existingPRURL,
			)
		default:
			t.Fatalf("unexpected Bitbucket PR API request: %s %s", r.Method, r.URL.String())
		}
	}))
	t.Cleanup(api.server.Close)

	return api
}

func fakeBitbucketEnv(apiBaseURL string) []string {
	return []string{
		"NO_MISTAKES_BITBUCKET_EMAIL=test@example.com",
		"NO_MISTAKES_BITBUCKET_API_TOKEN=test-token",
		"NO_MISTAKES_BITBUCKET_API_BASE_URL=" + apiBaseURL,
	}
}

type fakeBitbucketCIAPI struct {
	server         *httptest.Server
	prState        string
	statusesJSON   string
	pipelinesJSON  string
	stepsJSON      string
	stepLog        string
	stepsByPath    map[string]string
	stepLogsByPath map[string]string
	prSourceSHA    string
	prStateCalls   int
	statusesCalls  int
	pipelinesCalls int
	stepsCalls     int
	stepLogCalls   int
	lastAuthHeader string
	lastStatusesQ  string
	lastPipelineQ  string
}

func newFakeBitbucketCIAPI(t *testing.T, prState, statusesJSON string) *fakeBitbucketCIAPI {
	t.Helper()

	api := &fakeBitbucketCIAPI{
		prState:      prState,
		statusesJSON: statusesJSON,
	}

	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.lastAuthHeader = r.Header.Get("Authorization")

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/2.0/repositories/test/repo/pullrequests/42":
			api.prStateCalls++
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":42,"state":%q,"source":{"commit":{"hash":%q}}}`, api.prState, api.prSourceSHA)
		case r.Method == http.MethodGet && r.URL.Path == "/2.0/repositories/test/repo/pullrequests/42/statuses":
			api.statusesCalls++
			api.lastStatusesQ = r.URL.Query().Get("q")
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, api.statusesJSON)
		case r.Method == http.MethodGet && r.URL.Path == "/2.0/repositories/test/repo/pipelines" && api.pipelinesJSON != "":
			api.pipelinesCalls++
			api.lastPipelineQ = r.URL.Query().Get("target.commit.hash")
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, api.pipelinesJSON)
		case r.Method == http.MethodGet && api.stepsByPath[r.URL.Path] != "":
			api.stepsCalls++
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, api.stepsByPath[r.URL.Path])
		case r.Method == http.MethodGet && api.stepLogsByPath[r.URL.Path] != "":
			api.stepLogCalls++
			fmt.Fprint(w, api.stepLogsByPath[r.URL.Path])
		case r.Method == http.MethodGet && r.URL.Path == "/2.0/repositories/test/repo/pipelines/{pipeline-1}/steps" && api.stepsJSON != "":
			api.stepsCalls++
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, api.stepsJSON)
		case r.Method == http.MethodGet && r.URL.Path == "/2.0/repositories/test/repo/pipelines/{pipeline-1}/steps/{step-1}/log" && api.stepLog != "":
			api.stepLogCalls++
			fmt.Fprint(w, api.stepLog)
		default:
			t.Fatalf("unexpected Bitbucket CI API request: %s %s", r.Method, r.URL.String())
		}
	}))
	t.Cleanup(api.server.Close)

	return api
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
		"FAKE_CLI_MODE":   "ci-gh",
		"FAKE_CLI_STATE":  state,
		"FAKE_CLI_CHECKS": checksJSON,
	})
}

func fakeCIGHMergeable(t *testing.T, state, checksJSON, mergeable string) []string {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "gh")
	return fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":      "ci-gh",
		"FAKE_CLI_STATE":     state,
		"FAKE_CLI_CHECKS":    checksJSON,
		"FAKE_CLI_MERGEABLE": mergeable,
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
	})
}

func fakeCIGHChecksError(t *testing.T, state, mergeable, checksErr string) []string {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "gh")
	return fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":       "ci-gh",
		"FAKE_CLI_STATE":      state,
		"FAKE_CLI_MERGEABLE":  mergeable,
		"FAKE_CLI_CHECKS_ERR": checksErr,
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
	})
}

func fakeCIGHNoChecks(t *testing.T) []string {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "gh")
	return fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE": "ci-gh-nochecks",
	})
}
