//go:build unix

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestClaudeAgent_Run_LargePromptViaStdin is the regression test for the E2BIG
// failure mode: a failing test step embeds its full captured output in the
// auto-fix prompt, which is routinely hundreds of KB to megabytes. When that
// prompt was passed as a `-p <prompt>` argv element the exec overflowed the OS
// ARG_MAX and failed with "argument list too long" (fork/exec ...: argument
// list too long), surfacing as `agent fix tests: claude start: ...` and taking
// the pipeline step down. The prompt now travels on stdin, which has no such
// length ceiling.
//
// The test drives the real claudeAgent.runOnce against a fake `claude` binary
// with a 4 MiB prompt - larger than ARG_MAX on Linux and macOS and far larger
// than Linux's per-argument MAX_ARG_STRLEN (128 KiB) - so a regression that
// reintroduces argv delivery fails here with an exec error.
func TestClaudeAgent_Run_LargePromptViaStdin(t *testing.T) {
	dir := t.TempDir()
	stdinCapture := filepath.Join(dir, "stdin.txt")

	// Fake claude: copy the whole stdin prompt to a file (proving stdin
	// transport and full delivery), then emit a minimal successful stream-json
	// result event that claudeAgent's parser accepts.
	script := "#!/bin/sh\n" +
		"cat > \"$NM_TEST_STDIN_CAPTURE\"\n" +
		"printf '%s\\n' '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}'\n"
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}

	t.Setenv("NM_TEST_STDIN_CAPTURE", stdinCapture)

	// 4 MiB prompt: comfortably over ARG_MAX everywhere, so a return to argv
	// delivery would fail the exec instead of reaching the fake.
	prompt := strings.Repeat("a", 4*1024*1024)

	ca := &claudeAgent{bin: bin}
	res, err := ca.runOnce(context.Background(), RunOpts{Prompt: prompt, CWD: dir})
	if err != nil {
		t.Fatalf("runOnce with large prompt failed (E2BIG regression?): %v", err)
	}
	if res == nil {
		t.Fatalf("expected a result, got nil")
	}

	got, err := os.ReadFile(stdinCapture)
	if err != nil {
		t.Fatalf("read captured stdin: %v", err)
	}
	if len(got) != len(prompt) {
		t.Fatalf("fake claude received %d prompt bytes on stdin, want %d", len(got), len(prompt))
	}
}
