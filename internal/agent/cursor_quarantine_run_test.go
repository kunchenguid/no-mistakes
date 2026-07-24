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

// writeStubAcpxAssertingQuarantine writes a stub acpx that fails if Cursor
// instruction surfaces are still visible at CWD, and records whether AGENTS.md
// was absent during the run.
func writeStubAcpxAssertingQuarantine(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "acpx")
	script := `#!/bin/sh
# argv includes --cwd <path>; find it.
cwd=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "--cwd" ]; then
    cwd="$arg"
    break
  fi
  prev="$arg"
done
if [ -z "$cwd" ]; then
  echo "missing --cwd" >&2
  exit 1
fi
visible=""
if [ -e "$cwd/AGENTS.md" ]; then visible="${visible}AGENTS.md "; fi
if [ -e "$cwd/CLAUDE.md" ]; then visible="${visible}CLAUDE.md "; fi
if [ -e "$cwd/.cursor/rules" ]; then visible="${visible}.cursor/rules "; fi
if [ -n "$visible" ]; then
  echo "instruction surfaces still visible: $visible" >&2
  exit 1
fi
# Unrelated .cursor content must remain.
if [ ! -f "$cwd/.cursor/mcp.json" ]; then
  echo "unrelated .cursor/mcp.json was removed" >&2
  exit 1
fi
printf '%s\n' "$@" > "$NM_TEST_ACPX_ARGS_FILE"
printf '{"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","text":"quarantine-ok"}}}\n'
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAcpxAgent_Run_CursorQuarantinesInstructionFilesUnderOptOut(t *testing.T) {
	for _, tc := range []struct {
		name  string
		agent types.AgentName
	}{
		{name: "cursor alias", agent: types.AgentCursor},
		{name: "acp:cursor", agent: "acp:cursor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			argsFile := filepath.Join(dir, "argv.txt")
			t.Setenv("NM_TEST_ACPX_ARGS_FILE", argsFile)
			stub := writeStubAcpxAssertingQuarantine(t, dir)

			if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("AYE_CAPTAIN_CANARY\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("claude-canary\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(dir, ".cursor", "rules"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".cursor", "rules", "x.mdc"), []byte("rule\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".cursor", "mcp.json"), []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}

			a, err := NewWithOptions(tc.agent, stub, nil, Options{DisableProjectSettings: true})
			if err != nil {
				t.Fatalf("NewWithOptions: %v", err)
			}
			if !NeutralizesGateInstructions(a) {
				t.Fatal("cursor under opt-out must neutralize")
			}
			res, err := a.Run(context.Background(), RunOpts{Prompt: "ping", CWD: dir})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Text != "quarantine-ok" {
				t.Errorf("text = %q", res.Text)
			}

			// Restored after Run.
			for _, rel := range []string{"AGENTS.md", "CLAUDE.md", filepath.Join(".cursor", "rules")} {
				if _, err := os.Lstat(filepath.Join(dir, rel)); err != nil {
					t.Errorf("%s not restored: %v", rel, err)
				}
			}
			body, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
			if err != nil || string(body) != "AYE_CAPTAIN_CANARY\n" {
				t.Errorf("AGENTS.md body after restore = %q err=%v", body, err)
			}
		})
	}
}

func TestAcpxAgent_Run_CursorRestoresOnAgentError(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "acpx")
	script := `#!/bin/sh
echo boom >&2
exit 1
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("must-restore\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	a, err := NewWithOptions(types.AgentCursor, stub, nil, Options{DisableProjectSettings: true})
	if err != nil {
		t.Fatal(err)
	}
	_, runErr := a.Run(context.Background(), RunOpts{Prompt: "x", CWD: dir})
	if runErr == nil {
		t.Fatal("expected agent error")
	}
	body, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if err != nil || string(body) != "must-restore\n" {
		t.Fatalf("AGENTS.md not restored after error: body=%q err=%v", body, err)
	}
}

func TestAcpxAgent_Run_NoQuarantineWithoutOptOut(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "argv.txt")
	t.Setenv("NM_TEST_ACPX_ARGS_FILE", argsFile)
	// Stub that requires AGENTS.md to still be present (no quarantine).
	stub := filepath.Join(dir, "acpx")
	script := `#!/bin/sh
cwd=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "--cwd" ]; then cwd="$arg"; break; fi
  prev="$arg"
done
if [ ! -f "$cwd/AGENTS.md" ]; then
  echo "AGENTS.md missing without opt-out" >&2
  exit 1
fi
printf '%s\n' "$@" > "$NM_TEST_ACPX_ARGS_FILE"
printf '{"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","text":"visible"}}}\n'
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	a, err := NewWithOptions(types.AgentCursor, stub, nil, Options{}) // opt-out off
	if err != nil {
		t.Fatal(err)
	}
	if NeutralizesGateInstructions(a) {
		t.Fatal("cursor must not claim neutralization without opt-out")
	}
	res, err := a.Run(context.Background(), RunOpts{Prompt: "ping", CWD: dir})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "visible" {
		t.Errorf("text = %q", res.Text)
	}
	if _, err := os.Stat(filepath.Join(dir, "AGENTS.md")); err != nil {
		t.Errorf("AGENTS.md missing: %v", err)
	}
}

func TestAcpxAgent_GenericACP_DoesNotNeutralizeOrQuarantine(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "argv.txt")
	t.Setenv("NM_TEST_ACPX_ARGS_FILE", argsFile)
	stub := writeStubAcpx(t, dir) // records argv; does not require quarantine
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("stay\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	a, err := NewWithOptions(types.AgentName("acp:gemini"), stub, nil, Options{DisableProjectSettings: true})
	if err != nil {
		t.Fatal(err)
	}
	if NeutralizesGateInstructions(a) {
		t.Fatal("generic acp target must not neutralize under opt-out")
	}
	if err := EnsureGateNeutralized(a); err == nil {
		t.Fatal("EnsureGateNeutralized must refuse generic acp under opt-out")
	} else if !strings.Contains(err.Error(), "cursor") {
		t.Errorf("refusal should mention cursor among verified harnesses, got: %v", err)
	}

	// Even if somehow Run is called, AGENTS.md must remain (no quarantine).
	_, _ = a.Run(context.Background(), RunOpts{Prompt: "x", CWD: dir})
	if _, err := os.Stat(filepath.Join(dir, "AGENTS.md")); err != nil {
		t.Errorf("generic ACP must not quarantine AGENTS.md: %v", err)
	}
}
