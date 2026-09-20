package daemon

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// writeStdinCapturingPiAgent writes a fake pi binary that copies its stdin -
// the prompt the adapter pipes to the process - into capturePath, then returns
// a minimal agent_end payload so the invocation succeeds. It mirrors
// writeCapturingPiAgent, which captures argv instead.
func writeStdinCapturingPiAgent(t *testing.T, dir, capturePath string) string {
	t.Helper()
	const response = `{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"ok"}]}]}`
	bin := filepath.Join(dir, "pi")
	script := "#!/bin/sh\ncat >" + shellQuoteForTest(capturePath) + "\nprintf '%s\\n' '" + response + "'\n"
	if runtime.GOOS == "windows" {
		bin += ".cmd"
		script = "@echo off\r\nmore > \"" + capturePath + "\"\r\necho " + response + "\r\n"
	}
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// readCapturedPrompt returns the fake-pi stdin capture with newlines folded to
// LF. writeStdinCapturingPiAgent's Windows branch uses `more > file`, which
// writes CRLF; the production pipe still carries WorktreeSteering's LF text.
func readCapturedPrompt(t *testing.T, capturePath string) string {
	t.Helper()
	captured, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read captured prompt: %v", err)
	}
	return strings.ReplaceAll(strings.ReplaceAll(string(captured), "\r\n", "\n"), "\r", "\n")
}

// TestNewPipelineAgent_SteersHostSearchBoundary is the production-wiring half of
// the host-search contract. The step-level tests prove which step prompts carry
// the boundary; this one builds the agent the daemon actually runs through
// newPipelineAgent and proves the prompt delivered to the process carries the
// workspace-boundary preamble's host-search rule. It fails if the production
// wiring ever stops wrapping the agent in agent.WithSteering, which the
// step-level tests alone cannot catch.
func TestNewPipelineAgent_SteersHostSearchBoundary(t *testing.T) {
	dir := t.TempDir()
	evidenceRoot := filepath.Join(t.TempDir(), "evidence")
	capturePath := filepath.Join(t.TempDir(), "prompt.txt")
	piBin := writeStdinCapturingPiAgent(t, dir, capturePath)

	cfg := &config.Config{Agent: types.AgentPi, AgentPathOverride: map[string]string{"pi": piBin}}
	ag, err := newPipelineAgent(context.Background(), cfg, evidenceRoot, fakeLookPath, runenv.Overlay{})
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close()

	if _, err := ag.Run(context.Background(), agent.RunOpts{Prompt: "hello", CWD: dir, Purpose: "test-evidence"}); err != nil {
		t.Fatal(err)
	}
	prompt := readCapturedPrompt(t, capturePath)
	// Production interpolates evidenceRoot as given (no short/long-path rewrite).
	// The Windows fake copies stdin with `more > file`, which translates LF to
	// CRLF, so compare the preamble after newline normalization rather than as
	// raw capture bytes. The host-search wording and the evidence path still
	// have to match exactly.
	if !strings.Contains(prompt, agent.WorktreeSteering(evidenceRoot)) {
		t.Fatalf("production pipeline agent did not deliver the workspace-boundary preamble from agent.WorktreeSteering:\n%s", prompt)
	}
	for _, want := range []string{
		"Never run a filesystem-wide search such as `find /` or `mdfind /`",
		"never hunt the machine for an installed tool",
		"does not make a whole-root search bounded",
		"report the missing tool and the work it blocked in your normal result",
		"external evidence path a prompt explicitly names",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("production pipeline prompt missing host-search boundary %q:\n%s", want, prompt)
		}
	}
}

// TestReadCapturedPrompt_NormalizesWindowsNewlines pins the windows-git failure
// shape: `more > file` stores WorktreeSteering as CRLF, so a raw Contains of
// the LF preamble misses even though the host-search text and evidence path
// were delivered. Newline folding is the whole allowance; the path still has
// to match the caller-supplied root.
func TestReadCapturedPrompt_NormalizesWindowsNewlines(t *testing.T) {
	evidenceRoot := filepath.Join(t.TempDir(), "evidence")
	preamble := agent.WorktreeSteering(evidenceRoot)
	path := filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(preamble, "\n", "\r\n")+"hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := readCapturedPrompt(t, path)
	if !strings.Contains(got, preamble) {
		t.Fatalf("CRLF capture did not match WorktreeSteering after newline normalization:\n%s", got)
	}
	if !strings.Contains(got, evidenceRoot) {
		t.Fatalf("normalized capture dropped the caller-supplied evidence root %q:\n%s", evidenceRoot, got)
	}
	raw := strings.ReplaceAll(preamble, "\n", "\r\n") + "hello"
	if strings.Contains(raw, preamble) {
		t.Fatal("raw CRLF capture already contained the LF preamble; the windows-git failure mode is gone from this fixture")
	}
}
