package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestGrokAgent_Name(t *testing.T) {
	ga := &grokAgent{bin: "grok"}
	if ga.Name() != "grok" {
		t.Fatalf("Name() = %q, want grok", ga.Name())
	}
}

func TestGrokAgent_DoesNotNeutralizeGateInstructions(t *testing.T) {
	// grok --help has no flag that disables AGENTS.md/CLAUDE.md discovery.
	// Isolation is --cwd + a unique --leader-socket, not a verified
	// neutralization knob. Do not report neutralized.
	if NeutralizesGateInstructions(&grokAgent{bin: "grok"}) {
		t.Fatal("grok must not report neutralized; no verified suppression flag exists")
	}
	a, err := NewWithOptions(types.AgentGrok, "grok", nil, Options{DisableProjectSettings: true})
	if err != nil {
		t.Fatalf("NewWithOptions(grok): %v", err)
	}
	if NeutralizesGateInstructions(a) {
		t.Fatal("grok must not report neutralized under disable_project_settings either")
	}
	if err := EnsureGateNeutralized(a); err == nil {
		t.Fatal("EnsureGateNeutralized must refuse grok; neutralization is unverified")
	}
}

func TestGrokAgent_BuildArgs_HeadlessIsolationFlags(t *testing.T) {
	ga := &grokAgent{bin: "grok"}
	cwd := "/work/checkout"
	sock := filepath.Join(t.TempDir(), "leader.sock")
	args := ga.buildArgs(RunOpts{Prompt: "review the diff", CWD: cwd}, sock)

	assertArgPair(t, args, "--cwd", cwd)
	assertArgPair(t, args, "--leader-socket", sock)
	if !containsArg(args, "--always-approve") {
		t.Fatalf("args missing --always-approve: %v", args)
	}
	if !containsArgPair(args, "--output-format", "json") {
		t.Fatalf("args missing --output-format json: %v", args)
	}
	if !containsArgPair(args, "-p", "review the diff") {
		t.Fatalf("args missing -p prompt: %v", args)
	}
	if containsArg(args, "agent") {
		t.Fatalf("headless invoke must not start grok agent TUI/subcommand: %v", args)
	}
}

func TestGrokAgent_BuildArgs_ExtraArgsFirst(t *testing.T) {
	ga := &grokAgent{bin: "grok", extraArgs: []string{"--model", "grok-4"}}
	sock := filepath.Join(t.TempDir(), "leader.sock")
	args := ga.buildArgs(RunOpts{Prompt: "fix it", CWD: "/repo"}, sock)
	if len(args) < 2 || args[0] != "--model" || args[1] != "grok-4" {
		t.Fatalf("extraArgs should lead argv, got %v", args)
	}
}

func TestGrokAgent_BuildArgs_PassesJSONSchema(t *testing.T) {
	ga := &grokAgent{bin: "grok"}
	schema := json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`)
	sock := filepath.Join(t.TempDir(), "leader.sock")
	args := ga.buildArgs(RunOpts{Prompt: "review", CWD: "/repo", JSONSchema: schema}, sock)
	if !containsArgPair(args, "--json-schema", string(schema)) {
		t.Fatalf("args missing --json-schema: %v", args)
	}
}

func TestGrokAgent_BuildArgs_UserYoloSuppressesAlwaysApprove(t *testing.T) {
	ga := &grokAgent{bin: "grok", extraArgs: []string{"--yolo"}}
	args := ga.buildArgs(RunOpts{Prompt: "p", CWD: "/repo"}, filepath.Join(t.TempDir(), "leader.sock"))
	count := 0
	for _, a := range args {
		if a == "--always-approve" {
			count++
		}
	}
	if count != 0 {
		t.Fatalf("expected no default --always-approve when --yolo is set, got %v", args)
	}
}

func TestParseGrokJSONOutput_TextAndUsage(t *testing.T) {
	raw := []byte(`{
		"text": "{\"ok\":true}",
		"stopReason": "end_turn",
		"usage": {
			"input_tokens": 11,
			"output_tokens": 7,
			"cache_read_input_tokens": 3,
			"cache_creation_input_tokens": 1,
			"reasoning_tokens": 2
		}
	}`)
	text, usage, err := parseGrokJSONOutput(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if text != `{"ok":true}` {
		t.Fatalf("text = %q, want {\"ok\":true}", text)
	}
	want := TokenUsage{
		InputTokens:           11,
		OutputTokens:          7,
		CacheReadTokens:       3,
		CacheCreationTokens:   1,
		ReasoningTokens:       2,
		Reported:              true,
		CacheCreationReported: true,
	}
	if usage != want {
		t.Fatalf("usage = %+v, want %+v", usage, want)
	}
}

func TestParseGrokJSONOutput_ErrorObject(t *testing.T) {
	raw := []byte(`{"type":"error","message":"Couldn't start session: auth failed"}`)
	_, _, err := parseGrokJSONOutput(raw)
	if err == nil {
		t.Fatal("expected error object to fail")
	}
	if !strings.Contains(err.Error(), "auth failed") {
		t.Fatalf("error = %v, want auth failed", err)
	}
}

func TestParseGrokJSONOutput_RejectsEmptyText(t *testing.T) {
	_, _, err := parseGrokJSONOutput([]byte(`{"text":"   ","stopReason":"end_turn"}`))
	if err == nil {
		t.Fatal("expected empty text to fail")
	}
}

func TestGrokAgent_Run_PinsCwdAndUniqueLeaderSocket(t *testing.T) {
	workDir := t.TempDir()
	bin := writeFakeGrok(t, t.TempDir(), `#!/bin/sh
printf '%s\n' "$@" > grok-argv.txt
printf '%s\n' '{"text":"ok","stopReason":"end_turn"}'
`, strings.Join([]string{
		"@echo off",
		"echo %* > grok-argv.txt",
		"echo {\"text\":\"ok\",\"stopReason\":\"end_turn\"}",
	}, "\r\n"))

	ga := &grokAgent{bin: bin}
	if _, err := ga.Run(context.Background(), RunOpts{Prompt: "review", CWD: workDir}); err != nil {
		t.Fatalf("run grok: %v", err)
	}
	first := readArgv(t, workDir)
	cwd := argValue(first, "--cwd")
	if cwd != workDir {
		t.Fatalf("--cwd = %q, want %q in %v", cwd, workDir, first)
	}
	sock1 := argValue(first, "--leader-socket")
	if sock1 == "" {
		t.Fatalf("missing --leader-socket in %v", first)
	}
	home, _ := os.UserHomeDir()
	defaultSock := filepath.Join(home, ".grok", "leader.sock")
	if sock1 == defaultSock {
		t.Fatal("leader socket must not be the default ~/.grok/leader.sock")
	}

	workDir2 := t.TempDir()
	if _, err := ga.Run(context.Background(), RunOpts{Prompt: "review", CWD: workDir2}); err != nil {
		t.Fatalf("second run grok: %v", err)
	}
	sock2 := argValue(readArgv(t, workDir2), "--leader-socket")
	if sock1 == sock2 {
		t.Fatalf("leader sockets must be unique per invocation, both %q", sock1)
	}
}

func TestGrokAgent_Run_UnsetsFirstmateEnv(t *testing.T) {
	t.Setenv("GROK_AGENT", "firstmate")
	t.Setenv("GROK_SESSION_ID", "sess-stolen")
	t.Setenv("GROK_WORKSPACE_ROOT", "/home/rick/projects/firstmate")

	workDir := t.TempDir()
	bin := writeFakeGrok(t, t.TempDir(), `#!/bin/sh
env | grep -E '^(GROK_AGENT|GROK_SESSION_ID|GROK_WORKSPACE_ROOT)=' > grok-env.txt || true
printf '%s\n' '{"text":"ok","stopReason":"end_turn"}'
`, strings.Join([]string{
		"@echo off",
		"set GROK_AGENT>grok-env.txt 2>nul",
		"set GROK_SESSION_ID>>grok-env.txt 2>nul",
		"set GROK_WORKSPACE_ROOT>>grok-env.txt 2>nul",
		"echo {\"text\":\"ok\",\"stopReason\":\"end_turn\"}",
	}, "\r\n"))

	ga := &grokAgent{bin: bin}
	if _, err := ga.Run(context.Background(), RunOpts{Prompt: "review", CWD: workDir}); err != nil {
		t.Fatalf("run grok: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(workDir, "grok-env.txt"))
	if err != nil {
		t.Fatalf("read grok-env.txt: %v", err)
	}
	text := string(got)
	for _, key := range []string{"GROK_AGENT", "GROK_SESSION_ID", "GROK_WORKSPACE_ROOT"} {
		if strings.Contains(text, key+"=") {
			t.Errorf("child env still has %s: %s", key, text)
		}
	}
}

func TestGrokAgent_Run_ParsesJSONText(t *testing.T) {
	bin := writeFakeGrok(t, t.TempDir(), `#!/bin/sh
printf '%s\n' '{"text":"{\"ok\":true}","stopReason":"end_turn","usage":{"input_tokens":4,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}'
`, strings.Join([]string{
		"@echo off",
		"echo {\"text\":\"{\\\"ok\\\":true}\",\"stopReason\":\"end_turn\",\"usage\":{\"input_tokens\":4,\"output_tokens\":2,\"cache_read_input_tokens\":0,\"cache_creation_input_tokens\":0}}",
	}, "\r\n"))

	schema := json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`)
	ga := &grokAgent{bin: bin}
	var chunks []string
	result, err := ga.Run(context.Background(), RunOpts{
		Prompt:     "review",
		CWD:        t.TempDir(),
		JSONSchema: schema,
		OnChunk:    func(s string) { chunks = append(chunks, s) },
	})
	if err != nil {
		t.Fatalf("run grok: %v", err)
	}
	if string(result.Output) != `{"ok":true}` {
		t.Fatalf("output = %s, want {\"ok\":true}", result.Output)
	}
	if result.Usage.InputTokens != 4 || result.Usage.OutputTokens != 2 {
		t.Fatalf("usage = %+v", result.Usage)
	}
	if len(chunks) == 0 {
		t.Fatal("expected OnChunk to receive final text")
	}
}

func TestGrokAgent_Run_SurfacesNonZeroExit(t *testing.T) {
	bin := writeFakeGrok(t, t.TempDir(), `#!/bin/sh
echo boom >&2
exit 1
`, strings.Join([]string{
		"@echo off",
		"echo boom 1>&2",
		"exit /b 1",
	}, "\r\n"))

	ga := &grokAgent{bin: bin}
	_, err := ga.Run(context.Background(), RunOpts{Prompt: "review", CWD: t.TempDir()})
	if err == nil {
		t.Fatal("expected non-zero exit error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("expected stderr in error, got: %v", err)
	}
}

func writeFakeGrok(t *testing.T, dir, posixScript, windowsScript string) string {
	t.Helper()
	name := "grok"
	script := posixScript
	if runtime.GOOS == "windows" {
		name = "grok.cmd"
		script = windowsScript
	}
	bin := filepath.Join(dir, name)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake grok: %v", err)
	}
	return bin
}

func readArgv(t *testing.T, workDir string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(workDir, "grok-argv.txt"))
	if err != nil {
		t.Fatalf("read grok-argv.txt: %v", err)
	}
	return strings.Fields(strings.TrimSpace(string(raw)))
}

func argValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func containsArg(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func containsArgPair(args []string, flag, value string) bool {
	for i, a := range args {
		if a == flag && i+1 < len(args) && args[i+1] == value {
			return true
		}
	}
	return false
}

func assertArgPair(t *testing.T, args []string, flag, value string) {
	t.Helper()
	if !containsArgPair(args, flag, value) {
		t.Fatalf("args missing %s %s: %v", flag, value, args)
	}
}
