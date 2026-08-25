//go:build unix

package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// writeStubAcpx writes a stub acpx binary that records its argv (one arg per
// line) and stdin, then emits a minimal valid acpx JSON event stream.
func writeStubAcpx(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "acpx")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$NM_TEST_ACPX_ARGS_FILE"
cat > "$NM_TEST_ACPX_STDIN_FILE"
if [ -n "$NM_TEST_ACPX_EVENT" ]; then
  printf '%s\n' "$NM_TEST_ACPX_EVENT"
else
  printf '{"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","text":"cursor stub reply"}}}\n'
fi
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestAcpxAgent_Run_CursorSpawnsDefaultCommandWithoutOverrides proves both
// spellings of the Cursor agent drive a real acpx spawn with the alias
// default raw command — no acp_registry_overrides entry configured.
func TestAcpxAgent_Run_CursorSpawnsDefaultCommandWithoutOverrides(t *testing.T) {
	for _, tc := range []struct {
		name  string
		agent types.AgentName
	}{
		{name: "cursor alias", agent: types.AgentCursor},
		{name: "explicit acp:cursor target", agent: "acp:cursor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			argsFile := filepath.Join(dir, "argv.txt")
			t.Setenv("NM_TEST_ACPX_ARGS_FILE", argsFile)
			t.Setenv("NM_TEST_ACPX_STDIN_FILE", filepath.Join(dir, "stdin.txt"))
			stub := writeStubAcpx(t, dir)

			a, err := New(tc.agent, stub, nil)
			if err != nil {
				t.Fatalf("New(%q): %v", tc.agent, err)
			}
			res, err := a.Run(context.Background(), RunOpts{Prompt: "review this change", CWD: dir})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Text != "cursor stub reply" {
				t.Errorf("result text = %q, want stub acpx output", res.Text)
			}

			data, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatalf("stub acpx never recorded argv: %v", err)
			}
			argv := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
			if len(argv) < 2 || argv[0] != "--agent" || argv[1] != "cursor-agent acp" {
				t.Errorf("spawned argv = %q, want leading --agent \"cursor-agent acp\"", argv)
			}
			if len(argv) < 3 || strings.Join(argv[len(argv)-3:], "\x00") != "exec\x00--file\x00-" {
				t.Errorf("spawned argv = %q, want trailing exec --file -", argv)
			}
			for _, arg := range argv {
				if arg == "cursor" {
					t.Errorf("spawned argv = %q, must not pass the bare target when the default command is supplied", argv)
				}
			}
			t.Logf("spawned: acpx %s", strings.Join(argv, " "))
		})
	}
}

func TestAcpxAgent_Run_SendsLargePromptOnlyOnStdin(t *testing.T) {
	for _, tc := range []struct {
		name   string
		schema json.RawMessage
	}{
		{name: "plain prompt"},
		{name: "structured prompt", schema: json.RawMessage(`{"type":"object"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			argsFile := filepath.Join(dir, "argv.txt")
			stdinFile := filepath.Join(dir, "stdin.txt")
			t.Setenv("NM_TEST_ACPX_ARGS_FILE", argsFile)
			t.Setenv("NM_TEST_ACPX_STDIN_FILE", stdinFile)

			prompt := strings.Repeat("x", 4096)
			wantPrompt := prompt
			if len(tc.schema) > 0 {
				wantPrompt = buildACPStructuredPrompt(prompt, tc.schema)
				t.Setenv("NM_TEST_ACPX_EVENT", `{"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","text":"{\"ok\":true}"}}}`)
			}
			a := &acpxAgent{bin: writeStubAcpx(t, dir), target: "gemini"}
			if _, err := a.Run(context.Background(), RunOpts{Prompt: prompt, CWD: dir, JSONSchema: tc.schema}); err != nil {
				t.Fatalf("Run: %v", err)
			}

			argsData, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatalf("read argv: %v", err)
			}
			argv := strings.Split(strings.TrimRight(string(argsData), "\n"), "\n")
			if len(argv) < 3 || strings.Join(argv[len(argv)-3:], "\x00") != "exec\x00--file\x00-" {
				t.Fatalf("spawned argv = %q, want trailing exec --file -", argv)
			}
			for _, arg := range argv {
				if arg == wantPrompt {
					t.Fatalf("spawned argv contains the prompt")
				}
			}

			stdinData, err := os.ReadFile(stdinFile)
			if err != nil {
				t.Fatalf("read stdin: %v", err)
			}
			if got := string(stdinData); got != wantPrompt {
				t.Fatalf("stdin prompt mismatch: got %d bytes, want %d", len(got), len(wantPrompt))
			}
		})
	}
}

func TestAcpxAgent_Run_SurfacesStdinWriteFailure(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "acpx")
	script := `#!/bin/sh
printf '{"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","text":"early reply"}}}\n'
printf 'acpx: unknown option --file\n' >&2
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a := &acpxAgent{bin: stub, target: "gemini"}
	_, err := a.Run(ctx, RunOpts{Prompt: strings.Repeat("x", 2*1024*1024), CWD: dir})
	if err == nil || !strings.Contains(err.Error(), "acpx stdin") {
		t.Fatalf("Run error = %v, want acpx stdin write failure", err)
	}
	if !strings.Contains(err.Error(), "unknown option --file") {
		t.Fatalf("Run error = %v, want child stderr in stdin write failure", err)
	}
}
