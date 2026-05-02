//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Harness wires together the temp filesystem state needed to run the real
// no-mistakes binary against a fake agent. Each test gets its own Harness;
// the binaries themselves are built once per `go test` process.
type Harness struct {
	t *testing.T

	NMBin       string // absolute path to the no-mistakes binary under test
	FakeAgent   string // absolute path to the fake agent binary
	BinDir      string // temp dir holding agent symlinks; prepended to PATH
	NMHome      string // value used as $NM_HOME (daemon DB, socket, config)
	HomeDir     string // value used as $HOME so git operations don't read user state
	UpstreamDir string // bare repo serving as origin for the working clone
	WorkDir     string // working clone where the user runs `no-mistakes init`
	AgentLog    string // every fake-agent invocation appended here, one JSON per line
	Scenario    string // optional path to a scenario yaml; empty = built-in default

	agentName string // claude / codex / opencode
}

// SetupOpts controls per-test setup.
type SetupOpts struct {
	// Agent picks which fake the harness wires up: "claude", "codex", or
	// "opencode". The other two binaries are still on PATH (so `auto`
	// detection finds the requested one first via config), but only the
	// chosen one is exercised.
	Agent string

	// Scenario is an optional path to a YAML scenario file. If empty the
	// fake agent uses its built-in clean-response default.
	Scenario string
}

// NewHarness builds the no-mistakes + fakeagent binaries (once per test
// process), creates a temp git repo with origin, writes the no-mistakes
// global config to point at the chosen fake agent, and registers cleanup
// to stop the daemon and remove temp state at test end.
func NewHarness(t *testing.T, opts SetupOpts) *Harness {
	t.Helper()
	if opts.Agent == "" {
		opts.Agent = "claude"
	}

	nmBin, fakeBin := buildBinaries(t)

	root := t.TempDir()
	h := &Harness{
		t:           t,
		NMBin:       nmBin,
		FakeAgent:   fakeBin,
		BinDir:      filepath.Join(root, "bin"),
		NMHome:      filepath.Join(root, "nmhome"),
		HomeDir:     filepath.Join(root, "home"),
		UpstreamDir: filepath.Join(root, "upstream.git"),
		WorkDir:     filepath.Join(root, "work"),
		AgentLog:    filepath.Join(root, "fakeagent.log"),
		Scenario:    opts.Scenario,
		agentName:   opts.Agent,
	}

	for _, dir := range []string{h.BinDir, h.NMHome, h.HomeDir, h.WorkDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	// Symlink each agent name to the same fake binary. Codex and Claude
	// dispatch by argv[0] basename; opencode the same. Symlinks (not
	// copies) keep the build cheap on subsequent tests.
	for _, name := range []string{"claude", "codex", "opencode", "gh"} {
		linkPath := filepath.Join(h.BinDir, name)
		if err := os.Symlink(fakeBin, linkPath); err != nil {
			t.Fatalf("symlink %s: %v", linkPath, err)
		}
	}

	// Process-wide env. t.Setenv mutates os.Environ() for the rest of the
	// test, so subprocesses spawned by no-mistakes inherit these. The
	// daemon re-execs itself, also inheriting them.
	t.Setenv("PATH", h.BinDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", h.HomeDir)
	t.Setenv("NM_HOME", h.NMHome)
	t.Setenv("FAKEAGENT_LOG", h.AgentLog)
	if h.Scenario != "" {
		t.Setenv("FAKEAGENT_SCENARIO", h.Scenario)
	}
	// Point the fake at recorded real-agent fixtures by default. When
	// the directory contains <agent>/structured.{jsonl,*}, the fake
	// replays those bytes verbatim instead of generating synthetic
	// output. This is what makes the e2e a real wire-format check.
	root, err := defaultFixtureRoot()
	if err != nil {
		t.Fatalf("fixture root: %v", err)
	}
	t.Setenv("FAKEAGENT_FIXTURE", root)
	// Skip launchd/systemd/schtasks installation in the daemon. Without
	// this the daemon would touch the developer's real launch agents.
	t.Setenv("NM_TEST_START_DAEMON", "1")
	// Give the daemon room to come up; the default 5s is plenty for local
	// runs but CI runners with a cold Go cache occasionally exceed it.
	t.Setenv("NM_TEST_DAEMON_START_TIMEOUT", "30s")

	// Disable telemetry attempts (the package would no-op anyway, but
	// avoid a network DNS lookup on each command).
	t.Setenv("NO_MISTAKES_TELEMETRY", "off")

	h.writeGlobalConfig()
	h.initGitRepos()

	t.Cleanup(h.shutdown)
	return h
}

// writeGlobalConfig writes a no-mistakes global config that pins the
// agent name and binary path. The path override forces no-mistakes'
// agent.New to use our absolute fake-agent path instead of looking up the
// agent name in PATH (which would also work, but the override removes a
// confounding variable when something goes wrong).
func (h *Harness) writeGlobalConfig() {
	configPath := filepath.Join(h.NMHome, "config.yaml")
	if err := os.MkdirAll(h.NMHome, 0o755); err != nil {
		h.t.Fatalf("mkdir nm home: %v", err)
	}
	binLink := filepath.Join(h.BinDir, h.agentName)
	cfg := fmt.Sprintf(`agent: %s
log_level: debug
agent_path_override:
  %s: %s
auto_fix:
  rebase: 0
  lint: 0
  test: 0
  review: 0
  document: 0
  ci: 0
`, h.agentName, h.agentName, binLink)
	if err := os.WriteFile(configPath, []byte(cfg), 0o644); err != nil {
		h.t.Fatalf("write config: %v", err)
	}
}

// initGitRepos creates a bare upstream repo and a working clone with one
// initial commit on the default branch. Both repos use a local git
// identity so commits succeed without reading the user's gitconfig.
func (h *Harness) initGitRepos() {
	h.t.Helper()
	ctx := context.Background()
	mustGit := func(dir string, args ...string) {
		out, err := h.runGit(ctx, dir, args...)
		if err != nil {
			h.t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
		}
	}

	// Bare upstream. file:// URL is intentionally unsupported by
	// scm.DetectProvider, which is what causes the PR and CI steps to
	// gracefully skip in the e2e happy path.
	if err := os.MkdirAll(h.UpstreamDir, 0o755); err != nil {
		h.t.Fatalf("mkdir upstream: %v", err)
	}
	mustGit(h.UpstreamDir, "init", "--bare", "--initial-branch=main")

	mustGit(h.WorkDir, "init", "--initial-branch=main")
	mustGit(h.WorkDir, "config", "user.email", "e2e@example.com")
	mustGit(h.WorkDir, "config", "user.name", "E2E Test")
	mustGit(h.WorkDir, "config", "commit.gpgsign", "false")

	// One initial commit so the branch exists and the gate can rebase
	// against a real base.
	readme := filepath.Join(h.WorkDir, "README.md")
	if err := os.WriteFile(readme, []byte("# e2e\n"), 0o644); err != nil {
		h.t.Fatalf("write readme: %v", err)
	}
	repoConfig := filepath.Join(h.WorkDir, ".no-mistakes.yaml")
	if err := os.WriteFile(repoConfig, []byte("ignore_patterns:\n  - '*.generated.go'\n  - 'vendor/**'\n"), 0o644); err != nil {
		h.t.Fatalf("write repo config: %v", err)
	}
	mustGit(h.WorkDir, "add", "README.md", ".no-mistakes.yaml")
	mustGit(h.WorkDir, "commit", "-m", "initial commit")
	mustGit(h.WorkDir, "remote", "add", "origin", h.UpstreamDir)
	mustGit(h.WorkDir, "push", "-u", "origin", "main")
}

// Run invokes the no-mistakes binary in the working repo and returns
// (stdout+stderr, error). It propagates the harness env via os.Environ().
func (h *Harness) Run(args ...string) (string, error) {
	h.t.Helper()
	return h.RunInDir(h.WorkDir, args...)
}

// RunInDir invokes the no-mistakes binary in dir and returns stdout+stderr.
func (h *Harness) RunInDir(dir string, args ...string) (string, error) {
	h.t.Helper()
	return h.RunInDirWithEnv(dir, nil, args...)
}

// RunInDirWithEnv invokes the no-mistakes binary in dir with env overrides.
func (h *Harness) RunInDirWithEnv(dir string, env map[string]string, args ...string) (string, error) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.NMBin, args...)
	cmd.Dir = dir
	cmd.Env = mergedEnv(os.Environ(), env)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func mergedEnv(base []string, overrides map[string]string) []string {
	if len(overrides) == 0 {
		return base
	}
	out := make([]string, 0, len(base)+len(overrides))
	seen := make(map[string]bool, len(overrides))
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			out = append(out, entry)
			continue
		}
		if value, exists := overrides[key]; exists {
			out = append(out, key+"="+value)
			seen[key] = true
			continue
		}
		out = append(out, entry)
	}
	for key, value := range overrides {
		if !seen[key] {
			out = append(out, key+"="+value)
		}
	}
	return out
}

// CommitChange writes content to path (relative to WorkDir), commits it
// on the named branch, and returns the new HEAD SHA. Branches off main
// the first time the branch is referenced.
func (h *Harness) CommitChange(branch, path, content, message string) string {
	h.t.Helper()
	ctx := context.Background()

	current, err := h.runGit(ctx, h.WorkDir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		h.t.Fatalf("rev-parse HEAD: %v", err)
	}
	if bytes.TrimSpace(current) == nil || string(bytes.TrimSpace(current)) != branch {
		// Try checkout, fall back to creating the branch.
		if _, err := h.runGit(ctx, h.WorkDir, "checkout", branch); err != nil {
			if _, err := h.runGit(ctx, h.WorkDir, "checkout", "-b", branch, "main"); err != nil {
				h.t.Fatalf("checkout %s: %v", branch, err)
			}
		}
	}
	full := filepath.Join(h.WorkDir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		h.t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		h.t.Fatalf("write %s: %v", path, err)
	}
	if _, err := h.runGit(ctx, h.WorkDir, "add", path); err != nil {
		h.t.Fatalf("git add %s: %v", path, err)
	}
	if _, err := h.runGit(ctx, h.WorkDir, "commit", "-m", message); err != nil {
		h.t.Fatalf("git commit: %v", err)
	}
	sha, err := h.runGit(ctx, h.WorkDir, "rev-parse", "HEAD")
	if err != nil {
		h.t.Fatalf("rev-parse HEAD post-commit: %v", err)
	}
	return string(bytes.TrimSpace(sha))
}

// PushToGate pushes the current branch through the no-mistakes remote,
// which fires the post-receive hook and triggers a daemon-side pipeline
// run. Returns the IPC client connected to the daemon's socket.
func (h *Harness) PushToGate(branch string) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := h.runGit(ctx, h.WorkDir, "push", "no-mistakes", branch)
	if err != nil {
		h.t.Fatalf("git push no-mistakes %s: %v\n%s", branch, err, out)
	}
}

func (h *Harness) UpstreamBranchSHA(branch string) string {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sha, err := h.runGit(ctx, h.UpstreamDir, "rev-parse", "refs/heads/"+branch)
	if err != nil {
		h.t.Fatalf("rev-parse upstream %s: %v\n%s", branch, err, sha)
	}
	return string(bytes.TrimSpace(sha))
}

func (h *Harness) AddWorktree(branch string) string {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if out, err := h.runGit(ctx, h.WorkDir, "checkout", "main"); err != nil {
		h.t.Fatalf("checkout main before adding worktree: %v\n%s", err, out)
	}
	dir := filepath.Join(h.t.TempDir(), "worktree")
	if out, err := h.runGit(ctx, h.WorkDir, "worktree", "add", dir, branch); err != nil {
		h.t.Fatalf("git worktree add %s %s: %v\n%s", dir, branch, err, out)
	}
	h.t.Cleanup(func() {
		if _, err := os.Stat(dir); err == nil {
			_, _ = h.runGit(context.Background(), h.WorkDir, "worktree", "remove", "--force", dir)
		}
	})
	return dir
}

func (h *Harness) RemoveWorktree(dir string) {
	h.t.Helper()
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if out, err := h.runGit(ctx, h.WorkDir, "worktree", "remove", "--force", dir); err != nil {
		h.t.Fatalf("git worktree remove %s: %v\n%s", dir, err, out)
	}
}

func (h *Harness) Checkout(branch string) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if out, err := h.runGit(ctx, h.WorkDir, "checkout", branch); err != nil {
		h.t.Fatalf("checkout %s: %v\n%s", branch, err, out)
	}
}

func (h *Harness) WorktreeRefSHA(ref string) string {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sha, err := h.runGit(ctx, h.WorkDir, "rev-parse", ref)
	if err != nil {
		h.t.Fatalf("rev-parse worktree %s: %v\n%s", ref, err, sha)
	}
	return string(bytes.TrimSpace(sha))
}

// WaitForRun polls the daemon over IPC until a run for branch completes
// (status != pending|running) or timeout expires. Returns the final
// RunInfo so the test can assert on per-step outcomes.
func (h *Harness) WaitForRun(branch string, timeout time.Duration) *ipc.RunInfo {
	h.t.Helper()
	return h.waitForRunStatus(branch, timeout, func(status types.RunStatus) bool {
		switch status {
		case types.RunCompleted, types.RunFailed, types.RunCancelled:
			return true
		default:
			return false
		}
	}, "finish")
}

// WaitForRunRunning polls until the newest run for branch reaches running.
func (h *Harness) WaitForRunRunning(branch string, timeout time.Duration) *ipc.RunInfo {
	h.t.Helper()
	return h.waitForRunStatus(branch, timeout, func(status types.RunStatus) bool {
		return status == types.RunRunning
	}, "start running")
}

func (h *Harness) Runs() []ipc.RunInfo {
	h.t.Helper()
	p := paths.WithRoot(h.NMHome)
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		h.t.Fatalf("dial daemon: %v", err)
	}
	defer client.Close()
	var result ipc.GetRunsResult
	if err := client.Call(ipc.MethodGetRuns, &ipc.GetRunsParams{RepoID: h.repoID()}, &result); err != nil {
		h.t.Fatalf("get runs: %v", err)
	}
	return result.Runs
}

func (h *Harness) Respond(runID string, step types.StepName, action types.ApprovalAction) {
	h.t.Helper()
	if err := h.RespondError(runID, step, action); err != nil {
		h.t.Fatalf("respond to run %s step %s with %s: %v", runID, step, action, err)
	}
}

func (h *Harness) RespondError(runID string, step types.StepName, action types.ApprovalAction) error {
	h.t.Helper()
	p := paths.WithRoot(h.NMHome)
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		return fmt.Errorf("dial daemon: %w", err)
	}
	defer client.Close()
	var result ipc.RespondResult
	return client.Call(ipc.MethodRespond, &ipc.RespondParams{RunID: runID, Step: step, Action: action}, &result)
}

func (h *Harness) waitForRunStatus(branch string, timeout time.Duration, match func(types.RunStatus) bool, action string) *ipc.RunInfo {
	h.t.Helper()
	deadline := time.Now().Add(timeout)

	var lastRun *ipc.RunInfo
	for time.Now().Before(deadline) {
		var result ipc.GetRunsResult
		func() {
			p := paths.WithRoot(h.NMHome)
			client, err := ipc.Dial(p.Socket())
			if err != nil {
				return
			}
			defer client.Close()
			if err := client.Call(ipc.MethodGetRuns, &ipc.GetRunsParams{RepoID: h.repoID()}, &result); err != nil {
				result.Runs = nil
			}
		}()
		if result.Runs == nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		for i := range result.Runs {
			r := &result.Runs[i]
			if r.Branch != branch {
				continue
			}
			lastRun = r
			if match(r.Status) {
				return r
			}
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	h.dumpDebugState()
	if lastRun != nil {
		h.t.Fatalf("run for branch %s did not %s in %v (last status=%s)", branch, action, timeout, lastRun.Status)
	}
	h.t.Fatalf("no run found for branch %s within %v", branch, timeout)
	return nil
}

// dumpDebugState writes the daemon log, agent log, and per-step run
// logs to test output. Called on WaitForRun timeout so a stuck pipeline
// surfaces the actual failure mode instead of just "stuck running".
func (h *Harness) dumpDebugState() {
	for _, candidate := range []string{
		filepath.Join(h.NMHome, "logs", "daemon.log"),
		filepath.Join(h.NMHome, "daemon.log"),
	} {
		if data, err := os.ReadFile(candidate); err == nil && len(data) > 0 {
			h.t.Logf("--- %s (last 8KB) ---\n%s", candidate, tailBytes(data, 8192))
		}
	}
	if data, err := os.ReadFile(h.AgentLog); err == nil {
		h.t.Logf("--- fakeagent.log (%d invocations recorded) ---", bytes.Count(data, []byte("\n")))
	}
	logsDir := filepath.Join(h.NMHome, "logs")
	_ = filepath.Walk(logsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || filepath.Base(path) == "daemon.log" {
			return nil
		}
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			h.t.Logf("--- %s ---\n%s", path, tailBytes(data, 4096))
		}
		return nil
	})
}

func tailBytes(data []byte, n int) []byte {
	if len(data) <= n {
		return data
	}
	return data[len(data)-n:]
}

// AgentInvocations returns every recorded fake-agent invocation in the
// order they happened. Useful for asserting which steps actually called
// the agent.
func (h *Harness) AgentInvocations() []Invocation {
	h.t.Helper()
	data, err := os.ReadFile(h.AgentLog)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		h.t.Fatalf("read agent log: %v", err)
	}
	var invs []Invocation
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var inv Invocation
		if err := json.Unmarshal(line, &inv); err != nil {
			h.t.Fatalf("parse agent log line: %v\n%s", err, line)
		}
		invs = append(invs, inv)
	}
	return invs
}

// Invocation is a single fake-agent call captured in $FAKEAGENT_LOG.
type Invocation struct {
	Time   string   `json:"time"`
	Agent  string   `json:"agent"`
	Args   []string `json:"args"`
	Prompt string   `json:"prompt"`
	CWD    string   `json:"cwd,omitempty"`
}

func (h *Harness) runGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=E2E Test",
		"GIT_AUTHOR_EMAIL=e2e@example.com",
		"GIT_COMMITTER_NAME=E2E Test",
		"GIT_COMMITTER_EMAIL=e2e@example.com",
	)
	return cmd.CombinedOutput()
}

// repoID mirrors gate.repoID(): sha256 of the absolute work path, first
// 6 bytes hex. Tests need this to query runs by repo. EvalSymlinks
// matches gate.Init's normalisation (macOS /var → /private/var bites here
// without it).
func (h *Harness) repoID() string {
	abs := h.WorkDir
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	sum := sha256.Sum256([]byte(abs))
	return hex.EncodeToString(sum[:6])
}

// shutdown stops the daemon (best-effort) and is registered as a test
// cleanup. Ignoring errors here is intentional: the daemon may already
// be gone, the binary may have failed to build, etc. We just want the
// next test (or the developer's real daemon) not to inherit our state.
func (h *Harness) shutdown() {
	if _, err := os.Stat(h.NMBin); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.NMBin, "daemon", "stop")
	cmd.Dir = h.WorkDir
	cmd.Env = os.Environ()
	_ = cmd.Run()
}

// ---- Binary build cache ----

var (
	buildOnce sync.Once
	builtNM   string
	builtFake string
	buildErr  error
)

// buildBinaries compiles the no-mistakes binary and the fakeagent binary
// once per `go test` invocation. Both are placed in a per-process build
// dir; subsequent harnesses reuse them.
func buildBinaries(t *testing.T) (nmBin, fakeBin string) {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "nm-e2e-bin-*")
		if err != nil {
			buildErr = err
			return
		}
		nm := filepath.Join(dir, executableName("no-mistakes"))
		fake := filepath.Join(dir, executableName("fakeagent"))

		repoRoot, err := findRepoRoot()
		if err != nil {
			buildErr = err
			return
		}
		for _, target := range []struct {
			out, pkg string
		}{
			{nm, "./cmd/no-mistakes"},
			{fake, "./cmd/fakeagent"},
		} {
			cmd := exec.Command("go", "build", "-o", target.out, target.pkg)
			cmd.Dir = repoRoot
			out, err := cmd.CombinedOutput()
			if err != nil {
				buildErr = fmt.Errorf("build %s: %v\n%s", target.pkg, err, out)
				return
			}
		}
		builtNM = nm
		builtFake = fake
	})
	if buildErr != nil {
		t.Fatalf("build binaries: %v", buildErr)
	}
	return builtNM, builtFake
}

func executableName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

// defaultFixtureRoot returns the absolute path to internal/e2e/fixtures.
func defaultFixtureRoot() (string, error) {
	root, err := findRepoRoot()
	if err != nil {
		return "", err
	}
	return fixtureRootFromRepoRoot(root)
}

func fixtureRootFromRepoRoot(root string) (string, error) {
	dir := filepath.Join(root, "internal", "e2e", "fixtures")
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return dir, nil
	}
	return "", fmt.Errorf("fixtures directory not found: %s", dir)
}

// findRepoRoot walks up from this source file's directory looking for
// the go.mod that declares the no-mistakes module. Robust to test
// runners that change the working directory.
func findRepoRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("runtime caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("go.mod not found above " + filepath.Dir(file))
		}
		dir = parent
	}
}
