//go:build windows

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProcessImageName_CurrentProcess(t *testing.T) {
	got := processImageName(os.Getpid())
	if got == "" || got == "gone" || strings.HasPrefix(got, "unknown(") {
		t.Fatalf("processImageName(self) = %q, want a real image name", got)
	}
	if !strings.HasSuffix(strings.ToLower(got), ".exe") {
		t.Errorf("processImageName(self) = %q, want a .exe image", got)
	}
}

// TestSpawnDiag_EndToEndCmdShim drives runOnce against a fake claude .cmd shim
// (as npm installs on Windows) with NM_SPAWN_DIAG set, and asserts the
// diagnostic surfaces the tracked image, stdout volume, and exit path. This is
// the observability the issue #427 investigation needed: the tracked process is
// the cmd.exe wrapper, exposed here without any live API call.
func TestSpawnDiag_EndToEndCmdShim(t *testing.T) {
	t.Setenv(spawnDiagEnv, "1")
	dir := t.TempDir()

	jsonPath := filepath.Join(dir, "events.jsonl")
	events := `{"type":"assistant","message":{"usage":{"input_tokens":1,"output_tokens":1},"content":[{"type":"text","text":"ok"}]}}` + "\r\n" +
		`{"type":"result","subtype":"success","is_error":false}` + "\r\n"
	if err := os.WriteFile(jsonPath, []byte(events), 0o644); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(dir, "claude.cmd")
	if err := os.WriteFile(shim, []byte("@echo off\r\ntype \""+jsonPath+"\"\r\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	var lines []string
	a := &claudeAgent{bin: shim}
	res, err := a.runOnce(context.Background(), RunOpts{
		Prompt:  "hello",
		CWD:     dir,
		OnChunk: func(text string) { lines = append(lines, text) },
	})
	if err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if res == nil {
		t.Fatal("expected a result")
	}

	joined := strings.Join(lines, "\n")
	for _, want := range []string{"spawn-diag[claude]", "image=", "path=ok", "stdoutBytes="} {
		if !strings.Contains(joined, want) {
			t.Errorf("diagnostics missing %q in:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "hello") {
		t.Errorf("prompt should be redacted from diagnostics:\n%s", joined)
	}
}
