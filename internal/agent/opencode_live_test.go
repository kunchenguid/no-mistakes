package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/runenv"
)

// TestOpencodeLive drives the adapter against a REAL `opencode serve`. It is
// skipped unless NM_OPENCODE_LIVE_BIN names the opencode binary, because it
// spawns the server, uses whatever model opencode is configured with, and
// spends real tokens. It exists because the unit fake encodes the adapter's
// understanding of the v2 protocol; this is the check that the understanding
// matches the installed binary: a session created and driven through the
// fixed path, with text, structured and tool-using turns.
//
//	NM_OPENCODE_LIVE_BIN=$(command -v opencode) go test ./internal/agent -run TestOpencodeLive -v
func TestOpencodeLive(t *testing.T) {
	bin := os.Getenv("NM_OPENCODE_LIVE_BIN")
	if bin == "" {
		t.Skip("set NM_OPENCODE_LIVE_BIN to the opencode binary to run the live protocol check")
	}
	if out, err := runVersion(bin); err != nil {
		t.Fatalf("%s --version: %v", bin, err)
	} else {
		t.Logf("opencode version: %s", strings.TrimSpace(out))
	}

	a := &opencodeAgent{bin: bin, subprocessContext: newSubprocessContext(runenv.Overlay{})}
	t.Cleanup(func() { _ = a.Close() })

	cwd := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	t.Run("plain text", func(t *testing.T) {
		var chunks []string
		result, err := a.Run(ctx, RunOpts{
			Prompt:  "Reply with exactly the word pong and nothing else.",
			CWD:     cwd,
			OnChunk: func(s string) { chunks = append(chunks, s) },
		})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !strings.Contains(strings.ToLower(result.Text), "pong") {
			t.Errorf("text = %q, want pong", result.Text)
		}
		if !strings.Contains(strings.ToLower(strings.Join(chunks, "")), "pong") {
			t.Errorf("streamed chunks = %q, want pong streamed", chunks)
		}
		if !result.Usage.Reported || result.Usage.OutputTokens == 0 {
			t.Errorf("usage = %+v, want the turn's usage reported", result.Usage)
		}
	})

	t.Run("structured output", func(t *testing.T) {
		result, err := a.Run(ctx, RunOpts{
			Prompt:     "Report that everything is fine: set ok to true and give a one-word summary.",
			CWD:        cwd,
			JSONSchema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"},"summary":{"type":"string"}},"required":["ok","summary"]}`),
		})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		var out struct {
			OK      bool   `json:"ok"`
			Summary string `json:"summary"`
		}
		if err := json.Unmarshal(result.Output, &out); err != nil {
			t.Fatalf("output %s: %v", result.Output, err)
		}
		if !out.OK || out.Summary == "" {
			t.Errorf("output = %+v", out)
		}
	})

	t.Run("tool use under blanket permissions", func(t *testing.T) {
		marker := filepath.Join(cwd, "live-marker.txt")
		result, err := a.Run(ctx, RunOpts{
			Prompt: "Using your shell tool, create a file named live-marker.txt in the current directory containing the single word created. Then reply with exactly the word done.",
			CWD:    cwd,
		})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		data, err := os.ReadFile(marker)
		if err != nil {
			t.Fatalf("the tool turn left no marker file (text was %q): %v", result.Text, err)
		}
		if !strings.Contains(string(data), "created") {
			t.Errorf("marker = %q", data)
		}
	})
}

func runVersion(bin string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	return string(out), err
}
