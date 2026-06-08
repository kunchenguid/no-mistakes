package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPostReceiveHookScript(t *testing.T) {
	script := postReceiveHookScript("/opt/No Mistakes/no-mistakes")

	// should be a shell script
	if !strings.HasPrefix(script, "#!/bin/sh\n") {
		t.Fatal("hook should start with #!/bin/sh")
	}

	if !strings.Contains(script, "NM_BIN='/opt/No Mistakes/no-mistakes'") {
		t.Fatal("hook should embed the no-mistakes executable path")
	}

	// should read oldrev newrev refname
	if !strings.Contains(script, "read oldrev newrev refname") {
		t.Fatal("hook should read ref update args")
	}

	if !strings.Contains(script, "--gate \"$(pwd)\"") {
		t.Fatal("hook should pass the gate path as a flag")
	}
	if !strings.Contains(script, "daemon notify-push") {
		t.Fatal("hook should invoke the CLI notify subcommand")
	}
	if !strings.Contains(script, "GIT_PUSH_OPTION_COUNT") {
		t.Fatal("hook should forward git push options to notify-push")
	}
	if !strings.Contains(script, "--push-option") {
		t.Fatal("hook should pass each git push option as a notify-push flag")
	}
	if strings.Contains(script, "nc -U") {
		t.Fatal("hook should not depend on netcat")
	}
	if strings.Contains(script, "eval") {
		t.Fatal("hook should not use eval to read push options")
	}
	if !strings.Contains(script, "\"$NM_BIN\" daemon notify-push") {
		t.Fatal("hook should execute the embedded binary path")
	}
	if !strings.Contains(script, "command -v no-mistakes") {
		t.Fatal("hook should fall back to PATH when baked-in path doesn't exist")
	}
	if strings.Contains(script, ">/dev/null 2>&1 || true") {
		t.Fatal("hook should not silently swallow notify-push errors (issue #122)")
	}
	if !strings.Contains(script, "notify-push.log") {
		t.Fatal("hook should log notify-push output to a file under the bare repo")
	}

	// should print plain ASCII banner to stderr
	if !strings.Contains(script, ">&2") {
		t.Fatal("hook should print message to stderr")
	}
	if !strings.Contains(script, "Pipeline started") {
		t.Fatal("hook should print pipeline started message")
	}
	if !strings.Contains(script, "no-mistakes") {
		t.Fatal("hook should mention the command name")
	}
	if !strings.Contains(script, "|__| |_/") {
		t.Fatal("hook should contain ASCII art banner")
	}
	if strings.Contains(script, "\033[") {
		t.Fatal("hook banner should not include ANSI escapes")
	}
	if strings.Contains(script, "✓") {
		t.Fatal("hook banner should stay ASCII-only")
	}

	// should exit 0 (never block push)
	if !strings.Contains(script, "exit 0") {
		t.Fatal("hook should exit 0")
	}
}

func TestShellSingleQuote(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"plain", "/usr/bin/no-mistakes", "'/usr/bin/no-mistakes'"},
		{"spaces", "/opt/No Mistakes/bin", "'/opt/No Mistakes/bin'"},
		{"single_quote", "/opt/it's/bin", "'/opt/it'\"'\"'s/bin'"},
		{"multiple_quotes", "a'b'c", "'a'\"'\"'b'\"'\"'c'"},
		{"empty", "", "''"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shellSingleQuote(tt.input)
			if got != tt.want {
				t.Errorf("shellSingleQuote(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestPostReceiveHookScriptWithQuotedPath(t *testing.T) {
	script := postReceiveHookScript("/opt/it's here/no-mistakes")
	if !strings.Contains(script, "NM_BIN='/opt/it'\"'\"'s here/no-mistakes'") {
		t.Fatal("hook should correctly escape single quotes in the executable path")
	}
}

func TestPostReceiveHookScriptDoesNotEvaluatePushOptions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("post-receive hook is /bin/sh-only")
	}

	base := t.TempDir()
	bare := filepath.Join(base, "test.git")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}

	argsPath := filepath.Join(base, "args.txt")
	fakeBin := filepath.Join(base, "fake-no-mistakes")
	fakeScript := "#!/bin/sh\nprintf '%s\n' \"$@\" > " + shellSingleQuote(argsPath) + "\nexit 0\n"
	if err := os.WriteFile(fakeBin, []byte(fakeScript), 0o755); err != nil {
		t.Fatal(err)
	}

	hookPath := filepath.Join(base, "post-receive")
	if err := os.WriteFile(hookPath, []byte(postReceiveHookScript(fakeBin)), 0o755); err != nil {
		t.Fatal(err)
	}

	markerPath := filepath.Join(base, "pwned")
	cmd := exec.Command("/bin/sh", hookPath)
	cmd.Dir = bare
	cmd.Stdin = strings.NewReader("oldrev newrev refs/heads/main\n")
	cmd.Env = append(os.Environ(),
		"GIT_PUSH_OPTION_COUNT=1",
		"GIT_PUSH_OPTION_0=ok; touch "+markerPath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run hook: %v: %s", err, out)
	}

	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("hook should not evaluate push option shell syntax, stat err: %v", err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "ok; touch "+markerPath) {
		t.Fatalf("hook should forward push option literally, got:\n%s", args)
	}
}

func TestInstallPostReceiveHook(t *testing.T) {
	ctx := context.Background()
	bare := filepath.Join(t.TempDir(), "test.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}

	if err := InstallPostReceiveHook(bare); err != nil {
		t.Fatalf("InstallPostReceiveHook failed: %v", err)
	}

	hookPath := filepath.Join(bare, "hooks", "post-receive")

	// verify file exists
	info, err := os.Stat(hookPath)
	if err != nil {
		t.Fatalf("hook file not found: %v", err)
	}

	// verify executable permission
	if runtime.GOOS != "windows" && info.Mode()&0o111 == 0 {
		t.Fatalf("hook should be executable, got mode %v", info.Mode())
	}

	// verify content matches template
	content, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != postReceiveHookScript(exe) {
		t.Fatal("hook content doesn't match template")
	}
}

func TestRefreshManagedPostReceiveHookPreservesCustomHook(t *testing.T) {
	ctx := context.Background()
	bare := filepath.Join(t.TempDir(), "test.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	hookPath := filepath.Join(bare, "hooks", "post-receive")
	custom := "#!/bin/sh\necho custom hook\n"
	if err := os.WriteFile(hookPath, []byte(custom), 0o755); err != nil {
		t.Fatal(err)
	}

	changed, err := RefreshManagedPostReceiveHook(bare)
	if err != nil {
		t.Fatalf("RefreshManagedPostReceiveHook: %v", err)
	}
	if changed {
		t.Fatal("custom hook should not be overwritten")
	}
	data, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != custom {
		t.Fatalf("custom hook changed to:\n%s", data)
	}
}

func TestRefreshManagedPostReceiveHookInstallsMissingHook(t *testing.T) {
	ctx := context.Background()
	bare := filepath.Join(t.TempDir(), "test.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}

	changed, err := RefreshManagedPostReceiveHook(bare)
	if err != nil {
		t.Fatalf("RefreshManagedPostReceiveHook: %v", err)
	}
	if !changed {
		t.Fatal("missing managed hook should be installed")
	}
	info, err := os.Stat(filepath.Join(bare, "hooks", "post-receive"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode()&0o111 == 0 {
		t.Fatalf("hook should be executable, got mode %v", info.Mode())
	}
}

// TestPostReceiveHook_SurfacesNotifyFailures covers issue #122 defect 2:
// when notify-push fails (daemon down, missing-hook state, etc.), the user
// must see the failure on stderr instead of getting a clean-looking push.
// We also persist failures to <bareDir>/notify-push.log so they survive past
// the terminal scrollback.
//
// Note: post-receive's exit code is ignored by git, so we can't make
// `git push` exit non-zero. The wizard's push-confirmation step (defect 3)
// is responsible for surfacing the failure to the user as an error.
func TestPostReceiveHook_SurfacesNotifyFailures(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("post-receive hook is /bin/sh-only")
	}
	ctx := context.Background()

	base := t.TempDir()
	bare := filepath.Join(base, "test.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(base, "work")
	if out, err := exec.Command("git", "init", work).CombinedOutput(); err != nil {
		t.Fatalf("init work: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", work, "config", "user.email", "t@t.com").CombinedOutput(); err != nil {
		t.Fatalf("config email: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", work, "config", "user.name", "T").CombinedOutput(); err != nil {
		t.Fatalf("config name: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", work, "remote", "add", "gate", bare).CombinedOutput(); err != nil {
		t.Fatalf("add remote: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", work, "commit", "--allow-empty", "-m", "init").CombinedOutput(); err != nil {
		t.Fatalf("commit: %v: %s", err, out)
	}

	// Fake no-mistakes binary that always fails notify-push with a
	// distinctive marker on stderr.
	fakeBin := filepath.Join(base, "fake-no-mistakes")
	fakeScript := "#!/bin/sh\necho 'TESTMARKER notify failed' >&2\nexit 7\n"
	if err := os.WriteFile(fakeBin, []byte(fakeScript), 0o755); err != nil {
		t.Fatal(err)
	}

	// Install the real hook generated against the fake binary.
	hooksDir := filepath.Join(bare, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hookPath := filepath.Join(hooksDir, "post-receive")
	if err := os.WriteFile(hookPath, []byte(postReceiveHookScript(fakeBin)), 0o755); err != nil {
		t.Fatal(err)
	}

	// Push. We don't care whether `git push` exits zero (post-receive
	// exit code is ignored by git); we care that the failure surfaced.
	pushOut, _ := exec.Command("git", "-C", work, "push", "gate", "HEAD:refs/heads/main").CombinedOutput()

	if !strings.Contains(string(pushOut), "TESTMARKER notify failed") {
		t.Errorf("push output should surface notify-push stderr to the client, got:\n%s", pushOut)
	}

	logPath := filepath.Join(bare, "notify-push.log")
	logContent, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("notify-push.log should exist at %s: %v", logPath, err)
	}
	if !strings.Contains(string(logContent), "TESTMARKER notify failed") {
		t.Errorf("notify-push.log should contain notify-push stderr, got:\n%s", logContent)
	}
}

func TestIsolateHooksPath_OverridesPoisonedSharedConfig(t *testing.T) {
	ctx := context.Background()
	bare := filepath.Join(t.TempDir(), "test.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}

	// Simulate husky writing core.hookspath into the bare's shared local
	// config (this is what `git config core.hookspath .husky/_` does when
	// invoked from a linked worktree).
	if out, err := exec.Command("git", "-C", bare, "config", "core.hookspath", ".husky/_").CombinedOutput(); err != nil {
		t.Fatalf("seed shared core.hookspath: %v: %s", err, out)
	}

	if err := IsolateHooksPath(ctx, bare); err != nil {
		t.Fatalf("IsolateHooksPath: %v", err)
	}

	out, err := exec.Command("git", "-C", bare, "config", "--get", "core.hookspath").Output()
	if err != nil {
		t.Fatalf("get core.hookspath: %v", err)
	}
	want, err := filepath.Abs(filepath.Join(bare, "hooks"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != want {
		t.Errorf("effective core.hookspath = %q, want %q (per-worktree should win over poisoned shared)", got, want)
	}

	// Verify the shared poisoning is still observable in the local scope -
	// we don't try to clean it up because husky will just re-add it on the
	// next pipeline run. Per-worktree is what protects us.
	out, err = exec.Command("git", "-C", bare, "config", "--local", "--get", "core.hookspath").Output()
	if err != nil {
		t.Fatalf("get local core.hookspath: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != ".husky/_" {
		t.Errorf("local core.hookspath = %q, want %q", got, ".husky/_")
	}
}

// TestIsolateHooksPath_LinkedWorktreeCanRebase covers the regression where
// enabling extensions.worktreeConfig on a bare repo while leaving
// core.bare=true in shared config caused `git rebase` to fail inside linked
// worktrees with "fatal: this operation must be run in a work tree". Git
// requires core.bare to live in per-worktree scope once worktreeConfig is on.
func TestIsolateHooksPath_LinkedWorktreeCanRebase(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("worktree + shell pipeline is /bin/sh-only")
	}
	ctx := context.Background()
	base := t.TempDir()
	bare := filepath.Join(base, "gate.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}

	// Seed the bare with one commit on main so worktree add has a ref to check out.
	seedGate(t, base, bare)

	if err := IsolateHooksPath(ctx, bare); err != nil {
		t.Fatalf("IsolateHooksPath: %v", err)
	}

	// The bare repo itself must still look bare to git, otherwise other
	// operations (ls-remote, receive-pack) regress.
	if got := strings.TrimSpace(runGitOrFatal(t, bare, "rev-parse", "--is-bare-repository")); got != "true" {
		t.Fatalf("bare repo should still report is-bare-repository=true, got %q", got)
	}

	// Hook resolution must still point at the bare's hooks dir (the whole
	// point of IsolateHooksPath).
	wantHooks, err := filepath.Abs(filepath.Join(bare, "hooks"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(runGitOrFatal(t, bare, "config", "--get", "core.hookspath")); got != wantHooks {
		t.Fatalf("core.hookspath = %q, want %q", got, wantHooks)
	}

	// Create a linked worktree and confirm it actually behaves as a work tree.
	wt := filepath.Join(base, "wt")
	runGitOrFatal(t, bare, "worktree", "add", "--detach", wt, "refs/heads/main")

	if got := strings.TrimSpace(runGitOrFatal(t, wt, "rev-parse", "--is-inside-work-tree")); got != "true" {
		t.Fatalf("linked worktree should report is-inside-work-tree=true, got %q", got)
	}

	// Rebase onto the bare's main. Before the fix this errored with
	// "fatal: this operation must be run in a work tree" because core.bare=true
	// leaked from shared config into the linked worktree.
	runGitOrFatal(t, wt, "fetch", bare, "main")
	if out, err := exec.Command("git", "-C", wt, "rebase", "FETCH_HEAD").CombinedOutput(); err != nil {
		t.Fatalf("rebase in linked worktree should succeed, got err=%v output:\n%s", err, out)
	}
}

// TestIsolateHooksPath_PushToGateStillWorks guards the other critical path:
// after IsolateHooksPath moves core.bare to per-worktree scope, a normal
// `git push` from a working repo to the gate must still land, and the
// post-receive hook (resolved via the pinned core.hookspath) must still fire.
func TestIsolateHooksPath_PushToGateStillWorks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("post-receive hook is /bin/sh-only")
	}
	ctx := context.Background()
	base := t.TempDir()
	bare := filepath.Join(base, "gate.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	work := seedGate(t, base, bare)

	// Install a trivial post-receive hook that writes a marker file so we can
	// confirm hookspath resolution still finds the bare's hooks dir.
	hooksDir := filepath.Join(bare, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(base, "hook-ran")
	hookScript := "#!/bin/sh\necho ran > " + marker + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(hooksDir, "post-receive"), []byte(hookScript), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := IsolateHooksPath(ctx, bare); err != nil {
		t.Fatalf("IsolateHooksPath: %v", err)
	}

	// Make a second commit and push it. Push must succeed and the ref on the
	// bare must advance to the new HEAD.
	runGitOrFatal(t, work, "commit", "--allow-empty", "-m", "second")
	newSHA := strings.TrimSpace(runGitOrFatal(t, work, "rev-parse", "HEAD"))
	if out, err := exec.Command("git", "-C", work, "push", "gate", "HEAD:refs/heads/main").CombinedOutput(); err != nil {
		t.Fatalf("push to gate should succeed, got err=%v output:\n%s", err, out)
	}
	if got := strings.TrimSpace(runGitOrFatal(t, bare, "rev-parse", "refs/heads/main")); got != newSHA {
		t.Fatalf("bare refs/heads/main = %q, want %q (push did not advance ref)", got, newSHA)
	}

	// Hook fired via the per-worktree hookspath.
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("post-receive hook should have run, marker missing: %v", err)
	}
}

// seedGate creates a working repo under base/work, wires it to the bare as a
// remote, makes an initial commit, and pushes it as refs/heads/main. Returns
// the working repo path.
func seedGate(t *testing.T, base, bare string) string {
	t.Helper()
	work := filepath.Join(base, "work")
	if out, err := exec.Command("git", "init", work).CombinedOutput(); err != nil {
		t.Fatalf("init work: %v: %s", err, out)
	}
	runGitOrFatal(t, work, "config", "user.email", "t@t.com")
	runGitOrFatal(t, work, "config", "user.name", "T")
	runGitOrFatal(t, work, "remote", "add", "gate", bare)
	runGitOrFatal(t, work, "commit", "--allow-empty", "-m", "init")
	runGitOrFatal(t, work, "push", "gate", "HEAD:refs/heads/main")
	return work
}

func runGitOrFatal(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (in %s): %v: %s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

// TestIsolateHooksPath_MigratesFromPreFixState covers the upgrade path for
// bare repos created by the previous version of IsolateHooksPath, which left
// core.bare=true in shared config. Running the new IsolateHooksPath on that
// state must relocate core.bare to per-worktree scope so linked worktrees
// stop inheriting core.bare=true.
func TestIsolateHooksPath_MigratesFromPreFixState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("worktree + shell pipeline is /bin/sh-only")
	}
	ctx := context.Background()
	base := t.TempDir()
	bare := filepath.Join(base, "gate.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}

	// Simulate the pre-fix state: extensions.worktreeConfig on, hookspath
	// pinned per-worktree, but core.bare still in shared config.
	hooksDir, err := filepath.Abs(filepath.Join(bare, "hooks"))
	if err != nil {
		t.Fatal(err)
	}
	runGitOrFatal(t, bare, "config", "extensions.worktreeConfig", "true")
	runGitOrFatal(t, bare, "config", "--worktree", "core.hookspath", hooksDir)

	if err := IsolateHooksPath(ctx, bare); err != nil {
		t.Fatalf("IsolateHooksPath: %v", err)
	}

	// core.bare must no longer be in shared config.
	if out, err := exec.Command("git", "-C", bare, "config", "--local", "--get", "core.bare").CombinedOutput(); err == nil {
		t.Fatalf("core.bare should have been removed from shared config, still set to %q", strings.TrimSpace(string(out)))
	}
	// core.bare must now be in per-worktree config with value true.
	if got := strings.TrimSpace(runGitOrFatal(t, bare, "config", "--worktree", "--get", "core.bare")); got != "true" {
		t.Fatalf("core.bare in config.worktree = %q, want %q", got, "true")
	}
	// Bare still reports as bare.
	if got := strings.TrimSpace(runGitOrFatal(t, bare, "rev-parse", "--is-bare-repository")); got != "true" {
		t.Fatalf("bare repo should still report is-bare-repository=true, got %q", got)
	}
}

func TestIsolateHooksPath_Idempotent(t *testing.T) {
	ctx := context.Background()
	bare := filepath.Join(t.TempDir(), "test.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	if err := IsolateHooksPath(ctx, bare); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := IsolateHooksPath(ctx, bare); err != nil {
		t.Fatalf("second call should be a no-op: %v", err)
	}
}

func TestIsolateHooksPath_SkipsIsolationWhenWorktreeConfigUnsupported(t *testing.T) {
	ctx := context.Background()
	bare := filepath.Join(t.TempDir(), "test.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}

	originalRunGit := runGit
	runGit = func(_ context.Context, dir string, args ...string) (string, error) {
		t.Helper()
		if dir != bare {
			t.Fatalf("run dir = %q, want %q", dir, bare)
		}
		if len(args) >= 3 && args[0] == "config" && args[1] == "--worktree" {
			return "", exec.ErrNotFound
		}
		return originalRunGit(ctx, dir, args...)
	}
	defer func() { runGit = originalRunGit }()

	if err := IsolateHooksPath(ctx, bare); err != nil {
		t.Fatalf("IsolateHooksPath should tolerate missing --worktree support: %v", err)
	}

	out, err := exec.Command("git", "-C", bare, "config", "--local", "--get", "core.hookspath").CombinedOutput()
	if err == nil && strings.TrimSpace(string(out)) != "" {
		t.Fatalf("local core.hookspath should remain unset without worktree support, got %q", strings.TrimSpace(string(out)))
	}

	out, err = exec.Command("git", "-C", bare, "config", "--local", "--get", "extensions.worktreeConfig").CombinedOutput()
	if err == nil && strings.TrimSpace(string(out)) != "" {
		t.Fatalf("extensions.worktreeConfig should remain unset without worktree support, got %q", strings.TrimSpace(string(out)))
	}
}

// TestIsolateHooksPath_LinkedWorktreeResolvesRepoForCLI reproduces the CI
// step's working directory: a detached linked worktree of the gate bare repo,
// which records origin = the GitHub upstream. The CI watch shells out to `gh`
// from this directory, and gh resolves the repository by running git there.
// This guards the invariant that such a worktree is a real work tree with no
// leaked core.bare and a resolvable origin - i.e. gh can determine the repo
// from cwd alone. See issue #255: when this breaks, `gh pr checks` fails every
// poll and the CI step hangs until ci_timeout. No real gh or network is
// needed; the failure is purely in git's repo resolution, which gh depends on.
func TestIsolateHooksPath_LinkedWorktreeResolvesRepoForCLI(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	bare := filepath.Join(base, "gate.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	seedGate(t, base, bare)

	// The gate records origin = upstream so worktrees can resolve the repo
	// (gate.provisionGate does this for exactly this reason).
	const upstream = "https://github.com/test/repo.git"
	if err := EnsureRemote(ctx, bare, "origin", upstream); err != nil {
		t.Fatalf("EnsureRemote: %v", err)
	}
	if err := IsolateHooksPath(ctx, bare); err != nil {
		t.Fatalf("IsolateHooksPath: %v", err)
	}

	// IsolateHooksPath is best-effort: on a git too old for `config --worktree`
	// it cannot relocate core.bare, and the invariant below genuinely cannot
	// hold. Skip there - that limitation is the subject of
	// TestLinkedWorktreeLeaksCoreBareWithoutRelocation.
	if !worktreeConfigIsolatesCoreBare(t, bare) {
		t.Skip("git lacks `config --worktree`; core.bare cannot be isolated on this host")
	}

	wt := filepath.Join(base, "wt")
	if err := WorktreeAdd(ctx, bare, wt, "refs/heads/main"); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}

	if got := strings.TrimSpace(runGitOrFatal(t, wt, "rev-parse", "--is-inside-work-tree")); got != "true" {
		t.Fatalf("CI-step worktree is-inside-work-tree = %q, want true", got)
	}
	// core.bare must not leak into the worktree. If it resolves true here,
	// stricter git refuses work-tree operations and gh fails to resolve the
	// repo from cwd - the #255 hang.
	if out, err := exec.Command("git", "-C", wt, "config", "--get", "core.bare").CombinedOutput(); err == nil {
		if got := strings.TrimSpace(string(out)); got == "true" {
			t.Fatalf("core.bare leaked into linked worktree (=%q); gh repo resolution breaks on stricter git", got)
		}
	}
	// gh resolves the repo from the remotes of cwd; origin must be reachable.
	if got := strings.TrimSpace(runGitOrFatal(t, wt, "remote", "get-url", "origin")); got != upstream {
		t.Fatalf("origin url in worktree = %q, want %q", got, upstream)
	}
	// gh also runs `git rev-parse --absolute-git-dir`; it must succeed.
	if _, err := exec.Command("git", "-C", wt, "rev-parse", "--absolute-git-dir").Output(); err != nil {
		t.Fatalf("rev-parse --absolute-git-dir in worktree failed: %v", err)
	}
}

// TestLinkedWorktreeLeaksCoreBareWithoutRelocation pins the failure mechanism
// behind issue #255 for a git too old for per-worktree config. When
// IsolateHooksPath cannot relocate core.bare (no `config --worktree`), the bare
// repo's core.bare=true stays in shared config and leaks into every linked
// worktree. A worktree that reports core.bare=true is treated as bare by
// stricter git, so git - and therefore gh - fail to operate from it, and that
// worktree is the CI step's cwd. The old-git path is simulated deterministically
// via the runGit stub so this reproduces the reporter's config state on any git.
func TestLinkedWorktreeLeaksCoreBareWithoutRelocation(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	bare := filepath.Join(base, "gate.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	seedGate(t, base, bare)

	// Make `config --worktree` look unsupported so IsolateHooksPath takes its
	// best-effort no-op path, leaving core.bare=true in shared config - exactly
	// the state an old git leaves on the reporter's host.
	originalRunGit := runGit
	runGit = func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "config" && args[1] == "--worktree" {
			return "", exec.ErrNotFound
		}
		return originalRunGit(ctx, dir, args...)
	}
	defer func() { runGit = originalRunGit }()

	if err := IsolateHooksPath(ctx, bare); err != nil {
		t.Fatalf("IsolateHooksPath should no-op without worktree support: %v", err)
	}

	wt := filepath.Join(base, "wt")
	if err := WorktreeAdd(ctx, bare, wt, "refs/heads/main"); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}

	// The leak: core.bare=true resolves inside the linked worktree, which is
	// what makes gh (and git) treat the CI step's cwd as bare on stricter git.
	got := strings.TrimSpace(runGitOrFatal(t, wt, "config", "--get", "core.bare"))
	if got != "true" {
		t.Fatalf("expected core.bare to leak into linked worktree (=true) without relocation, got %q", got)
	}
}

// worktreeConfigIsolatesCoreBare reports whether the host git supports
// per-worktree config, i.e. whether IsolateHooksPath was able to relocate
// core.bare into per-worktree scope. When false, the host cannot isolate
// core.bare at all (best-effort path).
func worktreeConfigIsolatesCoreBare(t *testing.T, bare string) bool {
	t.Helper()
	out, err := exec.Command("git", "-C", bare, "config", "--worktree", "--get", "core.bare").CombinedOutput()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

func TestInstallPostReceiveHookCreatesDir(t *testing.T) {
	// hooks dir might not exist in some bare repos; installer should create it
	dir := t.TempDir()
	bareDir := filepath.Join(dir, "test.git")
	if err := os.MkdirAll(bareDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := InstallPostReceiveHook(bareDir); err != nil {
		t.Fatalf("InstallPostReceiveHook should create hooks dir: %v", err)
	}

	hookPath := filepath.Join(bareDir, "hooks", "post-receive")
	if _, err := os.Stat(hookPath); err != nil {
		t.Fatalf("hook file not found: %v", err)
	}
}
