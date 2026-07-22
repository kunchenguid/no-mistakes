//go:build unix

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// writeStubAcpx writes a stub acpx binary that records its argv (one arg per
// line) to the file named by NM_TEST_ACPX_ARGS_FILE and emits a minimal valid
// acpx JSON event stream.
func writeStubAcpx(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "acpx")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$NM_TEST_ACPX_ARGS_FILE"
printf '{"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","text":"cursor stub reply"}}}\n'
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
			if len(argv) < 2 || argv[len(argv)-2] != "exec" || argv[len(argv)-1] != "review this change" {
				t.Errorf("spawned argv = %q, want trailing exec <prompt>", argv)
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
