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
	"time"
)

func TestPiAgent_BuildArgs(t *testing.T) {
	pa := &piAgent{bin: "pi"}
	args := pa.buildArgs(nil)

	expected := []string{"--mode", "json", "--no-session"}

	if len(args) != len(expected) {
		t.Fatalf("expected %d args, got %d: %v", len(expected), len(args), args)
	}
	for i, want := range expected {
		if args[i] != want {
			t.Errorf("arg[%d]: expected %q, got %q", i, want, args[i])
		}
	}
}

func TestPiAgent_BuildArgs_DurableSession(t *testing.T) {
	pa := &piAgent{bin: "pi"}
	started := pa.buildArgs(&SessionRef{})
	if got, want := strings.Join(started, " "), "--mode json"; got != want {
		t.Fatalf("durable-session args = %q, want %q", got, want)
	}

	const sessionID = "019ff2f3-5f31-744b-90b8-679074ff7686"
	resumed := pa.buildArgs(&SessionRef{ID: sessionID})
	if got, want := strings.Join(resumed, " "), "--mode json --session "+sessionID; got != want {
		t.Fatalf("resume args = %q, want %q", got, want)
	}
}

func TestPiAgent_BuildArgs_PrependsExtraArgs(t *testing.T) {
	pa := &piAgent{bin: "pi", extraArgs: []string{"--provider", "google"}}
	args := pa.buildArgs(nil)

	expected := []string{"--provider", "google", "--mode", "json", "--no-session"}

	if len(args) != len(expected) {
		t.Fatalf("expected %d args, got %d: %v", len(expected), len(args), args)
	}
	for i, want := range expected {
		if args[i] != want {
			t.Errorf("arg[%d]: expected %q, got %q", i, want, args[i])
		}
	}
}

func TestPiAgent_BuildArgs_OptOutAddsNoContextFiles(t *testing.T) {
	pa := &piAgent{bin: "pi", extraArgs: []string{"--system-prompt"}, disableProjectSettings: true}
	args := pa.buildArgs(nil)
	expected := []string{"--no-context-files", "--system-prompt", "--mode", "json", "--no-session"}
	if len(args) != len(expected) {
		t.Fatalf("expected %d args, got %d: %v", len(expected), len(args), args)
	}
	for i, want := range expected {
		if args[i] != want {
			t.Errorf("arg[%d]: expected %q, got %q", i, want, args[i])
		}
	}
}

func TestPiAgent_BuildArgs_OptOutDoesNotDuplicateNoContextFiles(t *testing.T) {
	pa := &piAgent{bin: "pi", extraArgs: []string{"--provider", "google", "-nc"}, disableProjectSettings: true}
	args := pa.buildArgs(nil)
	expected := []string{"-nc", "--provider", "google", "--mode", "json", "--no-session"}
	if len(args) != len(expected) {
		t.Fatalf("expected %d args, got %d: %v", len(expected), len(args), args)
	}
	for i, want := range expected {
		if args[i] != want {
			t.Errorf("arg[%d]: expected %q, got %q", i, want, args[i])
		}
	}
}

func TestPiAgent_BuildArgs_OptOutPreservesNoContextFilesOptionValue(t *testing.T) {
	pa := &piAgent{bin: "pi", extraArgs: []string{"--system-prompt", "-nc"}, disableProjectSettings: true}
	args := pa.buildArgs(nil)
	expected := []string{"--no-context-files", "--system-prompt", "-nc", "--mode", "json", "--no-session"}
	if len(args) != len(expected) {
		t.Fatalf("expected %d args, got %d: %v", len(expected), len(args), args)
	}
	for i, want := range expected {
		if args[i] != want {
			t.Errorf("arg[%d]: expected %q, got %q", i, want, args[i])
		}
	}
}

func TestPiAgent_NeutralizesGateInstructions(t *testing.T) {
	if NeutralizesGateInstructions(&piAgent{bin: "pi"}) {
		t.Error("pi must not report neutralized without the opt-out")
	}
	if !NeutralizesGateInstructions(&piAgent{bin: "pi", disableProjectSettings: true}) {
		t.Error("pi must report neutralized under the opt-out")
	}
}

func TestPiAgent_BuildPromptIncludesSchema(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`)
	prompt := buildPiPrompt("do a thing", schema)
	if !strings.Contains(prompt, "do a thing") {
		t.Errorf("prompt missing user prompt: %s", prompt)
	}
	if !strings.Contains(prompt, "no-mistakes final output contract") {
		t.Errorf("prompt missing contract header: %s", prompt)
	}
	if !strings.Contains(prompt, "summary") {
		t.Errorf("prompt missing schema property: %s", prompt)
	}
}

func TestPiAgent_BuildPromptOmitsContractWhenSchemaEmpty(t *testing.T) {
	prompt := buildPiPrompt("do a thing", nil)
	if prompt != "do a thing" {
		t.Errorf("expected raw prompt when no schema, got: %q", prompt)
	}
}

func writeFakePi(t *testing.T, dir, posixScript, windowsScript string) string {
	t.Helper()

	name := "pi"
	script := posixScript
	if runtime.GOOS == "windows" {
		name = "pi.cmd"
		script = windowsScript
	}

	bin := filepath.Join(dir, name)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake pi: %v", err)
	}
	return bin
}

func TestPiAgent_RunOptOutPassesNoContextFilesToCLI(t *testing.T) {
	workDir := t.TempDir()
	bin := writeFakePi(t, t.TempDir(), `#!/bin/sh
printf '%s\n' "$*" > pi-argv.txt
cat > /dev/null
printf '%s\n' '{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"ok"}]}]}'
`, strings.Join([]string{
		"@echo off",
		"echo %* > pi-argv.txt",
		"more > nul",
		"echo {\"type\":\"agent_end\",\"messages\":[{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"ok\"}]}]}",
	}, "\r\n"))

	pa := &piAgent{
		bin:                    bin,
		extraArgs:              []string{"--provider", "google"},
		disableProjectSettings: true,
	}
	if _, err := pa.Run(context.Background(), RunOpts{Prompt: "review", CWD: workDir}); err != nil {
		t.Fatalf("run pi: %v", err)
	}

	argv, err := os.ReadFile(filepath.Join(workDir, "pi-argv.txt"))
	if err != nil {
		t.Fatalf("read captured pi argv: %v", err)
	}
	got := strings.TrimSpace(string(argv))
	want := "--no-context-files --provider google --mode json --no-session"
	if got != want {
		t.Fatalf("pi argv = %q, want %q", got, want)
	}
	t.Logf("pi received argv: %s", got)
}

func TestPiAgent_RunParsesAssistantContentAndUsage(t *testing.T) {
	dir := t.TempDir()
	// Fake pi that emits a streaming text_delta plus a final message_end with
	// content blocks and a usage record. Mirrors the live JSONL shape.
	bin := writeFakePi(t, dir, `#!/bin/sh
cat > /dev/null
printf '%s\n' '{"type":"message_update","usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0},"assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"{\"ok"}}'
printf '%s\n' '{"type":"message_update","usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0},"assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"\":true}"}}'
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","responseId":"r1","provider":"openai-codex","model":"gpt-5.6-luna","content":[{"type":"text","text":"{\"ok\":true}"}],"usage":{"input":11,"output":7,"cacheRead":3,"cacheWrite":1}}}'
printf '%s\n' '{"type":"agent_end","messages":[]}'
`, strings.Join([]string{
		"@echo off",
		"more > nul",
		"echo {\"type\":\"message_update\",\"usage\":{\"input\":0,\"output\":0,\"cacheRead\":0,\"cacheWrite\":0},\"assistantMessageEvent\":{\"type\":\"text_delta\",\"contentIndex\":0,\"delta\":\"{\\\"ok\"}}",
		"echo {\"type\":\"message_update\",\"usage\":{\"input\":0,\"output\":0,\"cacheRead\":0,\"cacheWrite\":0},\"assistantMessageEvent\":{\"type\":\"text_delta\",\"contentIndex\":0,\"delta\":\"\\\":true}\"}}",
		"echo {\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"responseId\":\"r1\",\"provider\":\"openai-codex\",\"model\":\"gpt-5.6-luna\",\"content\":[{\"type\":\"text\",\"text\":\"{\\\"ok\\\":true}\"}],\"usage\":{\"input\":11,\"output\":7,\"cacheRead\":3,\"cacheWrite\":1}}}",
		"echo {\"type\":\"agent_end\",\"messages\":[]}",
	}, "\r\n"))

	schema := json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`)
	pa := &piAgent{bin: bin}

	var chunks []string
	result, err := pa.Run(context.Background(), RunOpts{
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
	if result.Model != "gpt-5.6-luna" || result.ModelProvider != "openai-codex" {
		t.Fatalf("unexpected model telemetry: model=%q provider=%q", result.Model, result.ModelProvider)
	}
	if len(chunks) == 0 {
		t.Fatal("expected onChunk to receive streaming text")
	}
	// OnChunk must receive the incremental deltas, not cumulative state.
	// Otherwise the TUI log buffer (which appends each chunk) duplicates
	// the running prefix.
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

func TestPiAgent_RunFallsBackToAgentEndMessages(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakePi(t, dir, `#!/bin/sh
cat > /dev/null
printf '%s\n' '{"type":"agent_end","messages":[{"role":"user","content":"prompt"},{"role":"assistant","content":[{"type":"text","text":"{\"ok\":true}"}]}]}'
`, strings.Join([]string{
		"@echo off",
		"more > nul",
		"echo {\"type\":\"agent_end\",\"messages\":[{\"role\":\"user\",\"content\":\"prompt\"},{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"{\\\"ok\\\":true}\"}]}]}",
	}, "\r\n"))

	schema := json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`)
	pa := &piAgent{bin: bin}
	result, err := pa.Run(context.Background(), RunOpts{
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

func TestPiAgent_RunResumesPersistedSession(t *testing.T) {
	const sessionID = "019ff2f3-5f31-744b-90b8-679074ff7686"
	workDir := t.TempDir()
	bin := writeFakePi(t, t.TempDir(), `#!/bin/sh
set -eu
cat > /dev/null
printf '%s\n' "$*" >> pi-argv.txt
if [ -f pi-session-id ]; then
	id=$(cat pi-session-id)
	[ "$*" = "--mode json --session $id" ] || { echo "unexpected resume args: $*" >&2; exit 1; }
	input=22
else
	[ "$*" = "--mode json" ] || { echo "unexpected start args: $*" >&2; exit 1; }
	id=019ff2f3-5f31-744b-90b8-679074ff7686
	printf '%s\n' "$id" > pi-session-id
	input=11
fi
printf '%s\n' "{\"type\":\"session\",\"version\":3,\"id\":\"$id\",\"timestamp\":\"2026-08-21T00:00:00.000Z\"}"
printf '%s\n' "{\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"responseId\":\"r$input\",\"stopReason\":\"stop\",\"content\":[{\"type\":\"text\",\"text\":\"{\\\"ok\\\":true}\"}],\"usage\":{\"input\":$input,\"output\":7,\"cacheRead\":3,\"cacheWrite\":1}}}"
printf '%s\n' "{\"type\":\"agent_end\",\"messages\":[{\"role\":\"user\",\"content\":\"fix\"},{\"role\":\"assistant\",\"responseId\":\"r$input\",\"stopReason\":\"stop\",\"content\":[{\"type\":\"text\",\"text\":\"{\\\"ok\\\":true}\"}],\"usage\":{\"input\":$input,\"output\":7,\"cacheRead\":3,\"cacheWrite\":1}}]}"
`, strings.Join([]string{
		"@echo off",
		"setlocal EnableDelayedExpansion",
		"more > nul",
		// The space before >> matters: %* ends in a hex digit, and cmd.exe
		// parses a digit immediately preceding a redirect as a file-descriptor
		// number (6>> would append handle 6, leaving the file empty).
		"echo %* >> pi-argv.txt",
		"if exist pi-session-id (",
		"  set /p id=<pi-session-id",
		"  echo %*| findstr /x /c:\"--mode json --session !id!\" >nul",
		"  if errorlevel 1 (echo unexpected resume args: %* 1>&2 & exit /b 1)",
		"  set input=22",
		") else (",
		"  echo %*| findstr /x /c:\"--mode json\" >nul",
		"  if errorlevel 1 (echo unexpected start args: %* 1>&2 & exit /b 1)",
		"  set id=019ff2f3-5f31-744b-90b8-679074ff7686",
		"  echo !id!>pi-session-id",
		"  set input=11",
		")",
		"echo {\"type\":\"session\",\"version\":3,\"id\":\"!id!\",\"timestamp\":\"2026-08-21T00:00:00.000Z\"}",
		"echo {\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"responseId\":\"r!input!\",\"stopReason\":\"stop\",\"content\":[{\"type\":\"text\",\"text\":\"{\\\"ok\\\":true}\"}],\"usage\":{\"input\":!input!,\"output\":7,\"cacheRead\":3,\"cacheWrite\":1}}}",
		"echo {\"type\":\"agent_end\",\"messages\":[{\"role\":\"user\",\"content\":\"fix\"},{\"role\":\"assistant\",\"responseId\":\"r!input!\",\"stopReason\":\"stop\",\"content\":[{\"type\":\"text\",\"text\":\"{\\\"ok\\\":true}\"}],\"usage\":{\"input\":!input!,\"output\":7,\"cacheRead\":3,\"cacheWrite\":1}}]}",
	}, "\r\n"))

	schema := json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`)
	pa := &piAgent{bin: bin}
	started, err := pa.Run(context.Background(), RunOpts{Prompt: "fix", CWD: workDir, JSONSchema: schema, Session: &SessionRef{}})
	if err != nil {
		t.Fatalf("start durable Pi session: %v", err)
	}
	if started.SessionID != sessionID || started.Resumed {
		t.Fatalf("started session = %+v, want id=%q and Resumed=false", started, sessionID)
	}
	if started.Usage.InputTokens != 11 || started.SessionUsageCumulative {
		t.Fatalf("started usage = %+v, want invocation-only input 11", started.Usage)
	}
	if got, err := os.ReadFile(filepath.Join(workDir, "pi-session-id")); err != nil || strings.TrimSpace(string(got)) != sessionID {
		t.Fatalf("persisted session ID = %q, %v; want %q", got, err, sessionID)
	}

	resumed, err := pa.Run(context.Background(), RunOpts{Prompt: "fix", CWD: workDir, JSONSchema: schema, Session: &SessionRef{ID: started.SessionID}})
	if err != nil {
		t.Fatalf("resume durable Pi session: %v", err)
	}
	if resumed.SessionID != sessionID || !resumed.Resumed {
		t.Fatalf("resumed session = %+v, want id=%q and Resumed=true", resumed, sessionID)
	}
	if resumed.Usage.InputTokens != 22 || resumed.SessionUsageCumulative {
		t.Fatalf("resumed usage = %+v, want invocation-only input 22", resumed.Usage)
	}

	argv, err := os.ReadFile(filepath.Join(workDir, "pi-argv.txt"))
	if err != nil {
		t.Fatalf("read captured pi argv: %v", err)
	}
	if got, want := strings.Fields(string(argv)), []string{"--mode", "json", "--mode", "json", "--session", sessionID}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("pi argv = %q, want %q", got, want)
	}
}

func TestPiAgent_RunFailsWhenStartingDurableSessionWithoutHeader(t *testing.T) {
	bin := writeFakePi(t, t.TempDir(), `#!/bin/sh
cat > /dev/null
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"ok"}]}}'
printf '%s\n' '{"type":"agent_end","messages":[]}'
`, strings.Join([]string{
		"@echo off",
		"more > nul",
		"echo {\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"ok\"}]}}",
		"echo {\"type\":\"agent_end\",\"messages\":[]}",
	}, "\r\n"))

	_, err := (&piAgent{bin: bin}).Run(context.Background(), RunOpts{
		Prompt:  "fix",
		CWD:     t.TempDir(),
		Session: &SessionRef{},
	})
	if err == nil || !strings.Contains(err.Error(), "did not report a session identity") {
		t.Fatalf("missing-session-header error = %v", err)
	}
}

func TestPiAgent_RunRejectsUnconfirmedResume(t *testing.T) {
	bin := writeFakePi(t, t.TempDir(), `#!/bin/sh
cat > /dev/null
printf '%s\n' '{"type":"session","id":"019ff2f3-5f31-744b-90b8-679074ff7686"}'
printf '%s\n' '{"type":"agent_end","messages":[{"role":"assistant","content":"ok"}]}'
`, strings.Join([]string{
		"@echo off",
		"more > nul",
		"echo {\"type\":\"session\",\"id\":\"019ff2f3-5f31-744b-90b8-679074ff7686\"}",
		"echo {\"type\":\"agent_end\",\"messages\":[{\"role\":\"assistant\",\"content\":\"ok\"}]}",
	}, "\r\n"))

	_, err := (&piAgent{bin: bin}).Run(context.Background(), RunOpts{
		Prompt:  "fix",
		CWD:     t.TempDir(),
		Session: &SessionRef{ID: "019ff2f3-5f31-744b-90b8-679074ff7687"},
	})
	if err == nil || !strings.Contains(err.Error(), "did not confirm") {
		t.Fatalf("resume mismatch error = %v", err)
	}
}

func TestPiAgent_RunRejectsInvalidResumeID(t *testing.T) {
	_, err := (&piAgent{bin: "unused"}).Run(context.Background(), RunOpts{
		Prompt:  "fix",
		CWD:     t.TempDir(),
		Session: &SessionRef{ID: "/tmp/not-a-pi-session"},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid pi session identity") {
		t.Fatalf("invalid session error = %v", err)
	}
}

func TestPiParser_CapturesFirstValidSessionHeader(t *testing.T) {
	const sessionID = "019ff2f3-5f31-744b-90b8-679074ff7686"
	stream := strings.Join([]string{
		`{"type":"session","id":"not-a-uuid"}`,
		`{"type":"session","id":"019ff2f3-5f31-744b-90b8-679074ff7686"}`,
		`{"type":"session","id":"019ff2f3-5f31-744b-90b8-679074ff7687"}`,
	}, "\n")
	pp := &piParser{}
	if err := pp.parse(context.Background(), strings.NewReader(stream)); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if pp.sessionID != sessionID {
		t.Fatalf("session ID = %q, want first valid %q", pp.sessionID, sessionID)
	}
}

func TestPiParser_ClearsPriorAssistantErrorAfterSuccessfulRetry(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"message_end","message":{"role":"assistant","responseId":"r1","stopReason":"error","errorMessage":"transient failure"}}`,
		`{"type":"message_end","message":{"role":"assistant","responseId":"r2","stopReason":"stop","content":[{"type":"text","text":"success"}]}}`,
		`{"type":"agent_end","messages":[{"role":"assistant","responseId":"r1","stopReason":"error","errorMessage":"transient failure"},{"role":"assistant","responseId":"r2","stopReason":"stop","content":[{"type":"text","text":"success"}]}]}`,
	}, "\n")

	pp := &piParser{}
	if err := pp.parse(context.Background(), strings.NewReader(stream)); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if pp.assistantError != "" {
		t.Fatalf("expected successful retry to clear assistant error, got %q", pp.assistantError)
	}
	if got := pp.finalText(); got != "success" {
		t.Fatalf("expected final retry text, got %q", got)
	}
}

func TestPiParser_SumsUniqueAssistantUsageAcrossTurns(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"message_end","message":{"role":"assistant","responseId":"r1","stopReason":"toolUse","content":[{"type":"toolCall","name":"bash"}],"usage":{"input":10,"output":2,"cacheRead":3,"cacheWrite":4}}}`,
		`{"type":"turn_end","message":{"role":"assistant","responseId":"r1","stopReason":"toolUse","content":[{"type":"toolCall","name":"bash"}],"usage":{"input":10,"output":2,"cacheRead":3,"cacheWrite":4}}}`,
		`{"type":"message_end","message":{"role":"assistant","responseId":"r2","stopReason":"stop","content":[{"type":"text","text":"done"}],"usage":{"input":1,"output":5,"cacheRead":6,"cacheWrite":7}}}`,
		`{"type":"turn_end","message":{"role":"assistant","responseId":"r2","stopReason":"stop","content":[{"type":"text","text":"done"}],"usage":{"input":1,"output":5,"cacheRead":6,"cacheWrite":7}}}`,
		`{"type":"agent_end","messages":[{"role":"assistant","responseId":"r1","stopReason":"toolUse","content":[{"type":"toolCall","name":"bash"}],"usage":{"input":10,"output":2,"cacheRead":3,"cacheWrite":4}},{"role":"toolResult","content":[{"type":"text","text":"ok"}]},{"role":"assistant","responseId":"r2","stopReason":"stop","content":[{"type":"text","text":"done"}],"usage":{"input":1,"output":5,"cacheRead":6,"cacheWrite":7}}]}`,
	}, "\n")

	pp := &piParser{}
	if err := pp.parse(context.Background(), strings.NewReader(stream)); err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := TokenUsage{InputTokens: 11, OutputTokens: 7, CacheReadTokens: 9, CacheCreationTokens: 11, Reported: true, CacheCreationReported: true}
	if pp.usage != want {
		t.Fatalf("usage = %+v, want %+v", pp.usage, want)
	}
}

func TestPiAgent_RunRejectsAssistantError(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakePi(t, dir, `#!/bin/sh
cat > /dev/null
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","stopReason":"error","errorMessage":"auth failed","content":[{"type":"text","text":"{\"ok\":true}"}]}}'
`, strings.Join([]string{
		"@echo off",
		"more > nul",
		"echo {\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"stopReason\":\"error\",\"errorMessage\":\"auth failed\",\"content\":[{\"type\":\"text\",\"text\":\"{\\\"ok\\\":true}\"}]}}",
	}, "\r\n"))

	pa := &piAgent{bin: bin}
	_, err := pa.Run(context.Background(), RunOpts{
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

func TestPiAgent_RunRejectsEmptyOutput(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakePi(t, dir, `#!/bin/sh
cat > /dev/null
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"   "}]}}'
`, strings.Join([]string{
		"@echo off",
		"more > nul",
		"echo {\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"   \"}]}}",
	}, "\r\n"))

	pa := &piAgent{bin: bin}
	_, err := pa.Run(context.Background(), RunOpts{
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

func TestPiAgent_RunSurfacesNonZeroExit(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakePi(t, dir, `#!/bin/sh
cat > /dev/null
echo "boom" >&2
exit 2
`, strings.Join([]string{
		"@echo off",
		"more > nul",
		"echo boom 1>&2",
		"exit /b 2",
	}, "\r\n"))

	pa := &piAgent{bin: bin}
	_, err := pa.Run(context.Background(), RunOpts{
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

func TestPiAgent_RunSurfacesStdinWriteFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture relies on a child exiting without reading stdin")
	}
	dir := t.TempDir()
	bin := writeFakePi(t, dir, `#!/bin/sh
printf '%s\n' '{"type":"agent_end","messages":[{"role":"assistant","content":"early reply"}]}'
printf 'pi rejected the prompt\n' >&2
`, "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pa := &piAgent{bin: bin}
	_, err := pa.Run(ctx, RunOpts{Prompt: strings.Repeat("x", 2*1024*1024), CWD: dir})
	if err == nil || !strings.Contains(err.Error(), "pi stdin") {
		t.Fatalf("Run error = %v, want pi stdin write failure", err)
	}
	if !strings.Contains(err.Error(), "pi rejected the prompt") {
		t.Fatalf("Run error = %v, want child stderr in stdin write failure", err)
	}
}

func TestPiAgent_RunCancelledByContext(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakePi(t, dir, `#!/bin/sh
cat > /dev/null
sleep 30
`, strings.Join([]string{
		"@echo off",
		"more > nul",
		"timeout /t 30 /nobreak > nul",
	}, "\r\n"))

	pa := &piAgent{bin: bin}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := pa.Run(ctx, RunOpts{
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
