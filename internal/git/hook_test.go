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
	if strings.Contains(script, "nc -U") {
		t.Fatal("hook should not depend on netcat")
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
