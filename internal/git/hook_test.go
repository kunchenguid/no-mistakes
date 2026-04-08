package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPostReceiveHookScript(t *testing.T) {
	script := PostReceiveHookScript()

	// should be a shell script
	if !strings.HasPrefix(script, "#!/bin/sh\n") {
		t.Fatal("hook should start with #!/bin/sh")
	}

	// should reference the socket path with NM_HOME support
	if !strings.Contains(script, "NM_HOME") {
		t.Fatal("hook should respect NM_HOME env var")
	}
	if !strings.Contains(script, "$HOME/.no-mistakes") {
		t.Fatal("hook should reference default .no-mistakes path")
	}
	if !strings.Contains(script, "/socket") {
		t.Fatal("hook should reference socket")
	}

	// should read oldrev newrev refname
	if !strings.Contains(script, "read oldrev newrev refname") {
		t.Fatal("hook should read ref update args")
	}

	// should send JSON-RPC push_received
	if !strings.Contains(script, "push_received") {
		t.Fatal("hook should send push_received method")
	}
	if !strings.Contains(script, "nc -U \"$SOCKET\" >/dev/null 2>&1 || true") {
		t.Fatal("hook should suppress nc output so pushes stay clean")
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
	if info.Mode()&0o111 == 0 {
		t.Fatalf("hook should be executable, got mode %v", info.Mode())
	}

	// verify content matches template
	content, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != PostReceiveHookScript() {
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
