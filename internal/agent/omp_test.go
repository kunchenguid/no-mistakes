package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOMPAgent_BuildArgs(t *testing.T) {
	oa := &ompAgent{bin: "omp"}
	args := oa.buildArgs("review")

	expected := []string{"-p", "--mode", "json", "--no-session", "--no-extensions", "--no-skills", "--no-rules", "@review"}

	if len(args) != len(expected) {
		t.Fatalf("expected %d args, got %d: %v", len(expected), len(args), args)
	}
	for i, want := range expected {
		if args[i] != want {
			t.Errorf("arg[%d]: expected %q, got %q", i, want, args[i])
		}
	}
}

func TestOMPAgent_BuildArgs_PrependsExtraArgsAndKeepsPromptLast(t *testing.T) {
	oa := &ompAgent{bin: "omp", extraArgs: []string{"--model", "opus"}}
	args := oa.buildArgs("review")

	expected := []string{"--model", "opus", "-p", "--mode", "json", "--no-session", "--no-extensions", "--no-skills", "--no-rules", "@review"}

	if len(args) != len(expected) {
		t.Fatalf("expected %d args, got %d: %v", len(expected), len(args), args)
	}
	for i, want := range expected {
		if args[i] != want {
			t.Errorf("arg[%d]: expected %q, got %q", i, want, args[i])
		}
	}
}

func writeFakeOMP(t *testing.T, dir, posixScript, windowsScript string) string {
	t.Helper()

	name := "omp"
	script := posixScript
	if runtime.GOOS == "windows" {
		name = "omp.cmd"
		script = windowsScript
	}

	bin := filepath.Join(dir, name)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake omp: %v", err)
	}
	return bin
}

func TestOMPAgent_RunParsesAssistantContentAndUsage(t *testing.T) {
	dir := t.TempDir()
	// Fake omp that emits a streaming text_delta plus a final message_end with
	// content blocks and a usage record. OMP shares Pi's JSONL wire shape but
	// takes its prompt from a temp file via an @<file> argument, so the fake reads
	// no stdin.
	bin := writeFakeOMP(t, dir, `#!/bin/sh
printf '%s\n' '{"type":"message_update","message":{"role":"assistant","responseId":"r1"},"assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"{\"ok"}}'
printf '%s\n' '{"type":"message_update","message":{"role":"assistant","responseId":"r1"},"assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"\":true}"}}'
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","responseId":"r1","content":[{"type":"text","text":"{\"ok\":true}"}],"usage":{"input":11,"output":7,"cacheRead":3,"cacheWrite":1}}}'
printf '%s\n' '{"type":"agent_end","messages":[]}'
`, strings.Join([]string{
		"@echo off",
		"echo {\"type\":\"message_update\",\"message\":{\"role\":\"assistant\",\"responseId\":\"r1\"},\"assistantMessageEvent\":{\"type\":\"text_delta\",\"contentIndex\":0,\"delta\":\"{\\\"ok\"}}",
		"echo {\"type\":\"message_update\",\"message\":{\"role\":\"assistant\",\"responseId\":\"r1\"},\"assistantMessageEvent\":{\"type\":\"text_delta\",\"contentIndex\":0,\"delta\":\"\\\":true}\"}}",
		"echo {\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"responseId\":\"r1\",\"content\":[{\"type\":\"text\",\"text\":\"{\\\"ok\\\":true}\"}],\"usage\":{\"input\":11,\"output\":7,\"cacheRead\":3,\"cacheWrite\":1}}}",
		"echo {\"type\":\"agent_end\",\"messages\":[]}",
	}, "\r\n"))

	schema := json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`)
	oa := &ompAgent{bin: bin}

	var chunks []string
	result, err := oa.Run(context.Background(), RunOpts{
		Prompt:     "review",
		CWD:        t.TempDir(),
		JSONSchema: schema,
		OnChunk:    func(s string) { chunks = append(chunks, s) },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(result.Output) != `{"ok":true}` {
		t.Fatalf("unexpected output: %s", string(result.Output))
	}
	if result.Usage.InputTokens != 11 || result.Usage.OutputTokens != 7 ||
		result.Usage.CacheReadTokens != 3 || result.Usage.CacheCreationTokens != 1 {
		t.Fatalf("unexpected usage: %+v", result.Usage)
	}
	if len(chunks) == 0 {
		t.Fatal("expected onChunk to receive streaming text")
	}
	// OnChunk must receive the incremental deltas, not cumulative state.
	wantChunks := []string{`{"ok`, `":true}`}
	if len(chunks) != len(wantChunks) {
		t.Fatalf("expected %d delta chunks, got %d: %v", len(wantChunks), len(chunks), chunks)
	}
	for i, want := range wantChunks {
		if chunks[i] != want {
			t.Errorf("chunk[%d] = %q, want %q", i, chunks[i], want)
		}
	}
}

func TestOMPAgent_RunFallsBackToAgentEndMessages(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeOMP(t, dir, `#!/bin/sh
printf '%s\n' '{"type":"agent_end","messages":[{"role":"user","content":"prompt"},{"role":"assistant","content":"{\"ok\":true}"}]}'
`, strings.Join([]string{
		"@echo off",
		"echo {\"type\":\"agent_end\",\"messages\":[{\"role\":\"user\",\"content\":\"prompt\"},{\"role\":\"assistant\",\"content\":\"{\\\"ok\\\":true}\"}]}",
	}, "\r\n"))

	schema := json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`)
	oa := &ompAgent{bin: bin}
	result, err := oa.Run(context.Background(), RunOpts{
		Prompt:     "review",
		CWD:        t.TempDir(),
		JSONSchema: schema,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(result.Output) != `{"ok":true}` {
		t.Fatalf("unexpected output: %s", string(result.Output))
	}
}

func TestOMPAgent_RunRejectsAssistantError(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeOMP(t, dir, `#!/bin/sh
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","stopReason":"error","errorMessage":"auth failed","content":[{"type":"text","text":"{\"ok\":true}"}]}}'
`, strings.Join([]string{
		"@echo off",
		"echo {\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"stopReason\":\"error\",\"errorMessage\":\"auth failed\",\"content\":[{\"type\":\"text\",\"text\":\"{\\\"ok\\\":true}\"}]}}",
	}, "\r\n"))

	oa := &ompAgent{bin: bin}
	_, err := oa.Run(context.Background(), RunOpts{
		Prompt:     "review",
		CWD:        t.TempDir(),
		JSONSchema: json.RawMessage(`{"type":"object"}`),
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "auth failed") {
		t.Errorf("expected error to mention 'auth failed', got: %v", err)
	}
}

func TestOMPAgent_RunRejectsEmptyOutput(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeOMP(t, dir, `#!/bin/sh
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"   "}]}}'
`, strings.Join([]string{
		"@echo off",
		"echo {\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"   \"}]}}",
	}, "\r\n"))

	oa := &ompAgent{bin: bin}
	_, err := oa.Run(context.Background(), RunOpts{
		Prompt: "review",
		CWD:    t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "no text output") {
		t.Errorf("expected 'no text output', got: %v", err)
	}
}

func TestOMPAgent_RunSurfacesNonZeroExit(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeOMP(t, dir, `#!/bin/sh
echo "boom" >&2
exit 2
`, strings.Join([]string{
		"@echo off",
		"echo boom 1>&2",
		"exit /b 2",
	}, "\r\n"))

	oa := &ompAgent{bin: bin}
	_, err := oa.Run(context.Background(), RunOpts{
		Prompt: "review",
		CWD:    t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected non-zero exit error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("expected stderr in error message, got: %v", err)
	}
}

func TestOMPAgent_RunCancelledByContext(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeOMP(t, dir, `#!/bin/sh
sleep 30
`, strings.Join([]string{
		"@echo off",
		"timeout /t 30 /nobreak > nul",
	}, "\r\n"))

	oa := &ompAgent{bin: bin}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := oa.Run(ctx, RunOpts{
		Prompt: "review",
		CWD:    t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context canceled") {
		t.Logf("got error: %v", err)
	}
}

func TestOMPAgent_DeliversLargePromptVerbatimViaFile(t *testing.T) {
	// Regression for the Linux MAX_ARG_STRLEN (128 KiB) per-argument cap: a large
	// prompt passed as one positional argv element fails exec with E2BIG. The
	// adapter must instead write it to a temp file and pass @<file>, delivering
	// the full prompt to OMP with no argv-size limit.
	if runtime.GOOS == "windows" {
		t.Skip("prompt-file capture fake is POSIX-only")
	}
	dir := t.TempDir()
	captured := filepath.Join(dir, "captured-prompt.txt")
	// Fake omp copies the file referenced by its trailing @<file> argument, then
	// emits a minimal agent_end so runOnce completes. If the adapter regressed to
	// an inline positional prompt, the trailing arg would not start with @ and
	// nothing is captured, failing the ReadFile below.
	posix := "#!/bin/sh\n" +
		"for a in \"$@\"; do last=\"$a\"; done\n" +
		"case \"$last\" in @*) cat \"${last#@}\" > \"" + captured + "\" ;; esac\n" +
		"printf '%s\\n' '{\"type\":\"agent_end\",\"messages\":[{\"role\":\"user\",\"content\":\"x\"},{\"role\":\"assistant\",\"content\":\"{\\\"ok\\\":true}\"}]}'\n"
	bin := writeFakeOMP(t, dir, posix, "")
	schema := json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`)
	// 200 KiB - larger than the 128 KiB single-argv cap that broke the old path.
	largePrompt := strings.Repeat("a", 200*1024)
	oa := &ompAgent{bin: bin}
	if _, err := oa.Run(context.Background(), RunOpts{
		Prompt:     largePrompt,
		CWD:        t.TempDir(),
		JSONSchema: schema,
	}); err != nil {
		t.Fatalf("large prompt via @file failed (E2BIG regression?): %v", err)
	}
	got, err := os.ReadFile(captured)
	if err != nil {
		t.Fatalf("fake omp did not capture an @<file> prompt (inline-argv regression?): %v", err)
	}
	if want := buildPiPrompt(largePrompt, schema); string(got) != want {
		t.Fatalf("prompt not delivered verbatim via file: got %d bytes, want %d", len(got), len(want))
	}
}
