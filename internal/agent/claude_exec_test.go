package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestClaudeAgent_LargePromptDeliveredViaStdin exercises the real exec path
// with a prompt far larger than the Windows CreateProcess command-line limit
// (~32767 characters). Before the fix the prompt rode in argv, so on Windows
// the child failed to launch with error 206 ("The filename or extension is too
// long"); the fix delivers the prompt on stdin instead. The test asserts the
// process launches on every platform and that the full prompt reaches the
// child intact, which is what proves stdin fully replaces the argv prompt.
func TestClaudeAgent_LargePromptDeliveredViaStdin(t *testing.T) {
	bin := buildFakeClaude(t)

	logPath := filepath.Join(t.TempDir(), "invocations.jsonl")
	t.Setenv("FAKEAGENT_LOG", logPath)
	// Force the synthetic clean response: no recorded fixture, no scenario.
	t.Setenv("FAKEAGENT_FIXTURE", "")
	t.Setenv("FAKEAGENT_SCENARIO", "")

	// 64 KiB prompt, well past the 32767-char ceiling, with sentinels so we
	// can confirm it round-tripped whole rather than being truncated.
	const head, tail = "PROMPT_HEAD::", "::PROMPT_TAIL"
	prompt := head + strings.Repeat("y", 64*1024) + tail

	a := &claudeAgent{bin: bin}
	res, err := a.runOnce(context.Background(), RunOpts{Prompt: prompt, CWD: t.TempDir()})
	if err != nil {
		t.Fatalf("runOnce with %d-char prompt failed to exec/parse: %v", len(prompt), err)
	}
	if res == nil {
		t.Fatal("expected a non-nil result")
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read fakeagent log: %v", err)
	}
	var got string
	var seen bool
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var inv struct {
			Agent  string `json:"agent"`
			Prompt string `json:"prompt"`
		}
		if err := json.Unmarshal([]byte(line), &inv); err != nil {
			t.Fatalf("parse invocation %q: %v", line, err)
		}
		if inv.Agent == "claude" {
			got = inv.Prompt
			seen = true
		}
	}
	if !seen {
		t.Fatal("fakeagent recorded no claude invocation")
	}
	if got != prompt {
		t.Fatalf("prompt did not round-trip via stdin: got %d chars, want %d", len(got), len(prompt))
	}
}

// buildFakeClaude compiles cmd/fakeagent and returns a path to it named
// "claude" (claude.exe on Windows) so the binary's argv[0]-basename dispatch
// selects the claude wire protocol.
func buildFakeClaude(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "claude")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", out, "./cmd/fakeagent")
	cmd.Dir = repoRootForTest(t)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fakeagent: %v\n%s", err, b)
	}
	return out
}

// repoRootForTest walks up from this source file to the module's go.mod so
// the build works regardless of the test runner's working directory.
func repoRootForTest(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.mod not found above %s", filepath.Dir(file))
		}
		dir = parent
	}
}
