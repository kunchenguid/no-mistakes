package git

import (
	"context"
	"os"
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
	if !strings.Contains(script, ">/dev/null 2>&1 || true") {
		t.Fatal("hook should suppress notifier output so pushes stay clean")
	}

	// should print user-facing message to stderr
	if !strings.Contains(script, "no-mistakes") && !strings.Contains(script, ">&2") {
		t.Fatal("hook should print message to stderr")
	}
	if !strings.Contains(script, "printf '%s\\n' 'no-mistakes: pipeline started. Run `no-mistakes` to review.' >&2") {
		t.Fatal("hook should print a literal backticked command without command substitution")
	}

	// should exit 0 (never block push)
	if !strings.Contains(script, "exit 0") {
		t.Fatal("hook should exit 0")
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
