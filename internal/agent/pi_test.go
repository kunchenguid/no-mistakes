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

func TestPiAgent_BuildArgs(t *testing.T) {
	pa := &piAgent{bin: "pi"}
	args := pa.buildArgs()

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

func TestPiAgent_BuildArgs_PrependsExtraArgs(t *testing.T) {
	pa := &piAgent{bin: "pi", extraArgs: []string{"--provider", "google"}}
	args := pa.buildArgs()

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

func TestPiAgent_BuildArgs_GateIsolationAppendedAfterModelOverride(t *testing.T) {
	pa := &piAgent{
		bin:                    "pi",
		extraArgs:              []string{"--model", "openai-codex/gpt-5.6-sol", "--thinking", "medium"},
		disableProjectSettings: true,
	}
	args := pa.buildArgs()

	expected := []string{
		"--model", "openai-codex/gpt-5.6-sol", "--thinking", "medium",
		"--mode", "json", "--no-session",
		"--no-extensions", "--no-skills", "--no-prompt-templates", "--no-themes",
		"--no-context-files", "--no-approve",
		"--system-prompt", "", "--append-system-prompt", "",
	}
	if len(args) != len(expected) {
		t.Fatalf("expected %d args, got %d: %v", len(expected), len(args), args)
	}
	for i, want := range expected {
		if args[i] != want {
			t.Errorf("arg[%d]: expected %q, got %q", i, want, args[i])
		}
	}
}

func TestPiAgent_GateIsolationRejectsConflictingOverrides(t *testing.T) {
	bin := writeCapableFakePi(t, t.TempDir(), "0.80.10", true, "")
	conflicts := []string{
		"--", "install", "remove", "uninstall", "update", "list", "config",
		"--extension", "-e", "-e./project.ts", "--extension=./project.ts",
		"--skill", "--skill=./project-skill",
		"--prompt-template", "--prompt-template=./project.md",
		"--theme", "--theme=./project.json",
		"--approve", "-a", "--approve=true",
		"--system-prompt", "--system-prompt=project",
		"--append-system-prompt", "--append-system-prompt=project",
		"--continue", "-c", "--resume", "-r", "--session", "--session-id", "--fork",
		"--extensions", "--skills", "--prompt-templates", "--themes", "--context-files",
		"--no-extensions=false", "--no-skills=false",
		"--no-prompt-templates=false", "--no-themes=false",
		"--no-context-files=false", "--no-approve=false",
		"@AGENTS.md", "@CLAUDE.md",
	}
	for _, conflict := range conflicts {
		t.Run(strings.ReplaceAll(conflict, "/", "_"), func(t *testing.T) {
			pa := &piAgent{bin: bin, extraArgs: []string{conflict}, disableProjectSettings: true}
			if pa.NeutralizesGateInstructions() {
				t.Fatalf("conflicting override %q must fail closed", conflict)
			}
		})
	}

	for _, args := range [][]string{
		{"--model", "openai-codex/gpt-5.6-sol", "--thinking", "medium"},
		{"--model", "list"},
		{"--model=list"},
		{"--provider", "config", "--thinking", "update"},
	} {
		pa := &piAgent{bin: bin, extraArgs: args, disableProjectSettings: true}
		if !pa.NeutralizesGateInstructions() {
			t.Fatalf("option values %q must preserve Pi gate isolation", args)
		}
	}

	for _, args := range [][]string{
		{"--model", "list", "install"},
		{"--thinking=medium", "config"},
	} {
		pa := &piAgent{bin: bin, extraArgs: args, disableProjectSettings: true}
		if pa.NeutralizesGateInstructions() {
			t.Fatalf("positional package command in %q must fail closed", args)
		}
	}
}

func TestPiAgent_GateIsolationCapabilityProbeIsIsolated(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("capability probe capture fixture is POSIX-only")
	}
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "help-args")
	cwdPath := filepath.Join(dir, "help-cwd")
	configPath := filepath.Join(dir, "help-config")
	ambientConfig := filepath.Join(dir, "ambient-config")
	t.Setenv("NM_PI_HELP_ARGS", argsPath)
	t.Setenv("NM_PI_HELP_CWD", cwdPath)
	t.Setenv("NM_PI_HELP_CONFIG", configPath)
	t.Setenv("PI_CODING_AGENT_DIR", ambientConfig)
	help := strings.Join(piRequiredGateCapabilityFlags(), " ")
	bin := writeFakePi(t, dir, `#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = "--version" ]; then
    printf '0.80.10\n'
    exit 0
  fi
done
printf '%s\n' "$@" > "$NM_PI_HELP_ARGS"
pwd > "$NM_PI_HELP_CWD"
printf '%s' "$PI_CODING_AGENT_DIR" > "$NM_PI_HELP_CONFIG"
printf '%s\n' '`+help+`'
`, "")

	if !piHasGateIsolationCapabilities(bin) {
		t.Fatal("isolated capable Pi must be admitted")
	}
	gotArgs, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read help argv: %v", err)
	}
	wantArgs := append(piGateIsolationArgs(), "--help")
	if got := strings.Split(strings.TrimSuffix(string(gotArgs), "\n"), "\n"); strings.Join(got, "\x00") != strings.Join(wantArgs, "\x00") {
		t.Fatalf("help argv = %q, want %q", got, wantArgs)
	}
	gotCWD, err := os.ReadFile(cwdPath)
	if err != nil {
		t.Fatalf("read help cwd: %v", err)
	}
	if strings.TrimSpace(string(gotCWD)) == dir {
		t.Fatalf("help probe used ambient project cwd %q", dir)
	}
	gotConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read help config: %v", err)
	}
	if string(gotConfig) == ambientConfig || filepath.Dir(string(gotConfig)) != strings.TrimSpace(string(gotCWD)) {
		t.Fatalf("help config dir = %q, cwd = %q", gotConfig, gotCWD)
	}
}

func TestPiAgent_GateIsolationRejectsDanglingValueOverrides(t *testing.T) {
	for _, arg := range []string{"--provider", "--model", "--api-key", "--session-dir", "--name", "-n", "--models", "--tools", "-t", "--exclude-tools", "-xt", "--thinking", "--export"} {
		t.Run(strings.TrimLeft(arg, "-"), func(t *testing.T) {
			if piExtraArgsPreserveGateIsolation([]string{arg}) {
				t.Fatalf("dangling value option %q must fail closed", arg)
			}
		})
	}
}

func TestPiAgent_GateIsolationRequiresCompatibleCapabilities(t *testing.T) {
	tests := []struct {
		name         string
		version      string
		completeHelp bool
		want         bool
	}{
		{name: "minimum", version: "0.80.10", completeHelp: true, want: true},
		{name: "newer", version: "0.81.0", completeHelp: true, want: true},
		{name: "older", version: "0.80.9", completeHelp: true},
		{name: "minimum prerelease", version: "0.80.10-beta.1", completeHelp: true},
		{name: "malformed", version: "development", completeHelp: true},
		{name: "incomplete help", version: "0.81.0", completeHelp: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin := writeCapableFakePi(t, t.TempDir(), tt.version, tt.completeHelp, "")
			pa := &piAgent{bin: bin, disableProjectSettings: true}
			if got := pa.NeutralizesGateInstructions(); got != tt.want {
				t.Fatalf("NeutralizesGateInstructions() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPiAgent_GateIsolationRevalidatesReplacedBinaryBeforeRun(t *testing.T) {
	dir := t.TempDir()
	bin := writeCapableFakePi(t, dir, "0.80.10", true, "")
	pa := &piAgent{bin: bin, disableProjectSettings: true}
	if !pa.NeutralizesGateInstructions() {
		t.Fatal("initial capable Pi must be admitted")
	}

	marker := filepath.Join(dir, "launched-after-replacement")
	writeCapableFakePi(t, dir, "0.80.9", true, marker)
	if _, err := pa.Run(context.Background(), RunOpts{Prompt: "must not launch", CWD: dir}); err == nil {
		t.Fatal("Run() must revalidate a replaced Pi binary")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("replacement gate invocation launched despite failed capability check: %v", err)
	}
}

func TestPiAgent_GateIsolationCapabilityFailureRefusesRun(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "launched")
	bin := writeCapableFakePi(t, dir, "0.80.9", true, marker)
	pa := &piAgent{bin: bin, disableProjectSettings: true}
	if _, err := pa.Run(context.Background(), RunOpts{Prompt: "must not launch", CWD: dir}); err == nil {
		t.Fatal("Run() must reject an unsupported Pi")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("gate invocation launched despite failed capability check: %v", err)
	}
}

func TestPiAgent_RunGateIsolationUsesExactArgvAndStdinPrompt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("argv NUL-capture fixture is POSIX-only")
	}
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args")
	promptPath := filepath.Join(dir, "prompt")
	t.Setenv("NM_PI_TEST_ARGS", argsPath)
	t.Setenv("NM_PI_TEST_PROMPT", promptPath)
	bin := writeCapableFakePi(t, dir, "0.80.10", true, "")
	pa := &piAgent{
		bin:                    bin,
		extraArgs:              []string{"--model", "openai-codex/gpt-5.6-sol", "--thinking", "medium"},
		disableProjectSettings: true,
	}
	result, err := pa.Run(context.Background(), RunOpts{Prompt: "NO_MISTAKES_GATE_PROMPT_SENTINEL", CWD: dir})
	if err != nil {
		t.Fatalf("Pi run: %v", err)
	}
	if result.Text != "done" {
		t.Fatalf("Pi result = %q, want done", result.Text)
	}

	prompt, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatalf("read captured prompt: %v", err)
	}
	if string(prompt) != "NO_MISTAKES_GATE_PROMPT_SENTINEL" {
		t.Fatalf("Pi stdin prompt = %q", prompt)
	}
	encodedArgs, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read captured argv: %v", err)
	}
	gotArgs := strings.Split(strings.TrimSuffix(string(encodedArgs), "\x00"), "\x00")
	wantArgs := []string{
		"--model", "openai-codex/gpt-5.6-sol", "--thinking", "medium",
		"--mode", "json", "--no-session",
		"--no-extensions", "--no-skills", "--no-prompt-templates", "--no-themes",
		"--no-context-files", "--no-approve",
		"--system-prompt", "", "--append-system-prompt", "",
	}
	if len(gotArgs) != len(wantArgs) {
		t.Fatalf("launched argv has %d args, want %d: %q", len(gotArgs), len(wantArgs), gotArgs)
	}
	for i, want := range wantArgs {
		if gotArgs[i] != want {
			t.Errorf("launched arg[%d] = %q, want %q", i, gotArgs[i], want)
		}
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

func writeCapableFakePi(t *testing.T, dir, version string, completeHelp bool, launchMarker string) string {
	t.Helper()
	help := strings.Join(piRequiredGateCapabilityFlags(), " ")
	if !completeHelp {
		help = strings.Replace(help, "--no-context-files", "", 1)
	}
	posixScript := `#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = "--version" ]; then
    printf '%s\n' '` + version + `'
    exit 0
  fi
done
for arg in "$@"; do
  if [ "$arg" = "--help" ]; then
    printf '%s\n' '` + help + `'
    exit 0
  fi
done
if [ -n '` + launchMarker + `' ]; then
  : > '` + launchMarker + `'
fi
if [ -n "$NM_PI_TEST_ARGS" ]; then
  : > "$NM_PI_TEST_ARGS"
  for arg in "$@"; do
    printf '%s\0' "$arg" >> "$NM_PI_TEST_ARGS"
  done
fi
if [ -n "$NM_PI_TEST_PROMPT" ]; then
  cat > "$NM_PI_TEST_PROMPT"
else
  cat > /dev/null
fi
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}'
`
	windowsScript := strings.Join([]string{
		"@echo off",
		"echo %* | findstr /C:\"--version\" >nul && (echo " + version + "& exit /b 0)",
		"echo %* | findstr /C:\"--help\" >nul && (echo " + help + "& exit /b 0)",
		"more > nul",
		"echo {\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"done\"}]}}",
	}, "\r\n")
	return writeFakePi(t, dir, posixScript, windowsScript)
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

func TestPiAgent_RunParsesAssistantContentAndUsage(t *testing.T) {
	dir := t.TempDir()
	// Fake pi that emits a streaming text_delta plus a final message_end with
	// content blocks and a usage record. Mirrors the live JSONL shape.
	bin := writeFakePi(t, dir, `#!/bin/sh
cat > /dev/null
printf '%s\n' '{"type":"message_update","message":{"role":"assistant","responseId":"r1"},"assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"{\"ok"}}'
printf '%s\n' '{"type":"message_update","message":{"role":"assistant","responseId":"r1"},"assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"\":true}"}}'
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","responseId":"r1","content":[{"type":"text","text":"{\"ok\":true}"}],"usage":{"input":11,"output":7,"cacheRead":3,"cacheWrite":1}}}'
printf '%s\n' '{"type":"agent_end","messages":[]}'
`, strings.Join([]string{
		"@echo off",
		"more > nul",
		"echo {\"type\":\"message_update\",\"message\":{\"role\":\"assistant\",\"responseId\":\"r1\"},\"assistantMessageEvent\":{\"type\":\"text_delta\",\"contentIndex\":0,\"delta\":\"{\\\"ok\"}}",
		"echo {\"type\":\"message_update\",\"message\":{\"role\":\"assistant\",\"responseId\":\"r1\"},\"assistantMessageEvent\":{\"type\":\"text_delta\",\"contentIndex\":0,\"delta\":\"\\\":true}\"}}",
		"echo {\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"responseId\":\"r1\",\"content\":[{\"type\":\"text\",\"text\":\"{\\\"ok\\\":true}\"}],\"usage\":{\"input\":11,\"output\":7,\"cacheRead\":3,\"cacheWrite\":1}}}",
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
printf '%s\n' '{"type":"agent_end","messages":[{"role":"user","content":"prompt"},{"role":"assistant","content":"{\"ok\":true}"}]}'
`, strings.Join([]string{
		"@echo off",
		"more > nul",
		"echo {\"type\":\"agent_end\",\"messages\":[{\"role\":\"user\",\"content\":\"prompt\"},{\"role\":\"assistant\",\"content\":\"{\\\"ok\\\":true}\"}]}",
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
