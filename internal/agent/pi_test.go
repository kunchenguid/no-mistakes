package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
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

// writePiProbeStub records its working directory and argv so tests can assert
// the probe environment and flags from the fake Pi's own observations.
func writePiProbeStub(t *testing.T) string {
	t.Helper()
	return writeFakePi(t, t.TempDir(), `#!/bin/sh
{
	printf '%s\n' "$(pwd)"
	printf '%s\n' "$*"
} > pi-probe.txt
exit 0
`, strings.Join([]string{
		"@echo off",
		"echo %cd% > pi-probe.txt",
		"echo %* >> pi-probe.txt",
		"exit /b 0",
	}, "\r\n"))
}

func readPiProbe(t *testing.T, workDir string) (string, string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(workDir, "pi-probe.txt"))
	if err != nil {
		t.Fatalf("read pi probe record: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 2 {
		t.Fatalf("pi probe record = %q, want cwd and argv lines", data)
	}
	return strings.TrimSpace(lines[0]), strings.TrimSpace(strings.Join(lines[1:], "\n"))
}

func TestPiAgent_ValidateConfigurationProbesInWorktree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pwd assertion relies on a POSIX stub")
	}
	workDir := t.TempDir()
	pa := &piAgent{bin: writePiProbeStub(t), extraArgs: []string{"--model", "sonnet"}, modelSource: "agent_config.pi.model"}
	if err := pa.ValidateConfiguration(context.Background(), workDir); err != nil {
		t.Fatalf("ValidateConfiguration: %v", err)
	}
	cwd, argv := readPiProbe(t, workDir)
	gotDir, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatalf("resolve probed cwd: %v", err)
	}
	wantDir, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		t.Fatalf("resolve workDir: %v", err)
	}
	if gotDir != wantDir {
		t.Fatalf("probe cwd = %q, want the run worktree %q", gotDir, wantDir)
	}
	for _, want := range []string{"--model sonnet", "--offline", "--mode rpc", "--no-session"} {
		if !strings.Contains(argv, want) {
			t.Fatalf("probe argv = %q, want %q", argv, want)
		}
	}
}

func TestPiAgent_ValidateConfigurationResolvesProjectSettingsDefault(t *testing.T) {
	workDir := t.TempDir()
	projectDir := filepath.Join(workDir, ".pi")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "settings.json"), []byte(`{"defaultProvider":"google","defaultModel":"project-model"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pa := &piAgent{bin: writePiProbeStub(t)}
	if err := pa.ValidateConfiguration(context.Background(), workDir); err != nil {
		t.Fatalf("ValidateConfiguration: %v", err)
	}
	_, argv := readPiProbe(t, workDir)
	if !strings.Contains(argv, "--model project-model") {
		t.Fatalf("probe argv = %q, want the project default model resolved by Pi", argv)
	}
	if strings.Contains(argv, "--provider") {
		t.Fatalf("probe argv = %q, want the bare settings id so Pi can still fail on an unknown model instead of fabricating a fallback", argv)
	}
}

func TestPiAgent_ValidateConfigurationProjectSettingsOverrideGlobal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("HOME-based fixture relies on a POSIX environment")
	}
	workDir := t.TempDir()
	globalDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(globalDir, ".pi", "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, ".pi", "agent", "settings.json"), []byte(`{"defaultModel":"global-model"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	projectDir := filepath.Join(workDir, ".pi")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "settings.json"), []byte(`{"defaultModel":"project-model"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pa := &piAgent{bin: writePiProbeStub(t), subprocessContext: newSubprocessContext(runenv.Overlay{Set: map[string]string{"HOME": globalDir}})}
	if err := pa.ValidateConfiguration(context.Background(), workDir); err != nil {
		t.Fatalf("ValidateConfiguration: %v", err)
	}
	_, argv := readPiProbe(t, workDir)
	if !strings.Contains(argv, "--model project-model") {
		t.Fatalf("probe argv = %q, want the project default to override the global default", argv)
	}
}

func TestPiAgent_ValidateConfigurationSurfacesPiResolutionFailure(t *testing.T) {
	bin := writeFakePi(t, t.TempDir(), `#!/bin/sh
cat > /dev/null
echo 'Error: Model "ghost-model" not found. Use --list-models to see available models.' >&2
exit 1
`, strings.Join([]string{
		"@echo off",
		"more > nul",
		"echo Error: Model not found. Use --list-models to see available models. 1>&2",
		"exit /b 1",
	}, "\r\n"))
	pa := &piAgent{bin: bin, extraArgs: []string{"--model", "ghost-model"}, modelSource: "agent_config.pi.model"}
	err := pa.ValidateConfiguration(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("expected Pi's own resolution failure to fail setup")
	}
	for _, want := range []string{"ghost-model", "agent_config.pi.model", `Model "ghost-model" not found`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("validation error = %q, want %q", err, want)
		}
	}
}

func TestPiAgent_ValidateConfigurationSkipsProbeWithoutModelSelection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("HOME-based fixture relies on a POSIX environment")
	}
	workDir := t.TempDir()
	// Isolate from any real ~/.pi/agent/settings.json default model.
	pa := &piAgent{bin: writePiProbeStub(t), subprocessContext: newSubprocessContext(runenv.Overlay{Set: map[string]string{"HOME": t.TempDir()}})}
	if err := pa.ValidateConfiguration(context.Background(), workDir); err != nil {
		t.Fatalf("ValidateConfiguration: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "pi-probe.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("probe ran without any configured model: %v", err)
	}
}

// A project default Pi would not honor must never fail setup: when Pi does not
// trust the project it ignores the project copy entirely, so the global default
// is what the probe validates.
func TestPiAgent_ValidateConfigurationIgnoresProjectDefaultForUntrustedProject(t *testing.T) {
	workDir := t.TempDir()
	projectDir := filepath.Join(workDir, ".pi")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "settings.json"), []byte(`{"defaultModel":"ghost-model"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	globalDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(globalDir, ".pi", "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, ".pi", "agent", "settings.json"), []byte(`{"defaultModel":"global-model"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pa := &piAgent{
		bin:               writePiProbeStub(t),
		extraArgs:         []string{"--no-approve"},
		subprocessContext: newSubprocessContext(runenv.Overlay{Set: map[string]string{"HOME": globalDir}}),
	}
	if err := pa.ValidateConfiguration(context.Background(), workDir); err != nil {
		t.Fatalf("ValidateConfiguration: %v", err)
	}
	_, argv := readPiProbe(t, workDir)
	if !strings.Contains(argv, "--model global-model") {
		t.Fatalf("probe argv = %q, want the global default the untrusted project copy cannot override", argv)
	}
	if strings.Contains(argv, "ghost-model") {
		t.Fatalf("probe argv = %q, want the inert project default ignored", argv)
	}
}

// With no recorded trust decision the project copy may be inert, so a failing
// project default is a warning, never a setup failure.
func TestPiAgent_ValidateConfigurationWarnsInsteadOfAbortingWhenProjectTrustUndetermined(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(previous)

	workDir := t.TempDir()
	projectSettings := filepath.Join(workDir, ".pi", "settings.json")
	if err := os.MkdirAll(filepath.Dir(projectSettings), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectSettings, []byte(`{"defaultModel":"ghost-model"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := writeFakePi(t, t.TempDir(), `#!/bin/sh
cat > /dev/null
echo 'Error: Model "ghost-model" not found. Use --list-models to see available models.' >&2
exit 1
`, strings.Join([]string{
		"@echo off",
		"more > nul",
		"echo Error: Model not found. Use --list-models to see available models. 1>&2",
		"exit /b 1",
	}, "\r\n"))
	pa := &piAgent{
		bin:               bin,
		subprocessContext: newSubprocessContext(runenv.Overlay{Set: map[string]string{"HOME": t.TempDir()}}),
	}
	if err := pa.ValidateConfiguration(context.Background(), workDir); err != nil {
		t.Fatalf("an undetermined project default must not abort setup: %v", err)
	}
	for _, want := range []string{"ghost-model", projectSettings, "inert"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("warning log = %q, want %q", logs.String(), want)
		}
	}
}

// A trust decision Pi would honor keeps the project default a hard failure.
func TestPiAgent_ValidateConfigurationRejectsProjectDefaultForTrustedProject(t *testing.T) {
	workDir := t.TempDir()
	projectDir := filepath.Join(workDir, ".pi")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "settings.json"), []byte(`{"defaultModel":"ghost-model"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := writeFakePi(t, t.TempDir(), `#!/bin/sh
cat > /dev/null
echo 'Error: Model "ghost-model" not found. Use --list-models to see available models.' >&2
exit 1
`, strings.Join([]string{
		"@echo off",
		"more > nul",
		"echo Error: Model not found. Use --list-models to see available models. 1>&2",
		"exit /b 1",
	}, "\r\n"))
	pa := &piAgent{
		bin:               bin,
		extraArgs:         []string{"--approve"},
		subprocessContext: newSubprocessContext(runenv.Overlay{Set: map[string]string{"HOME": t.TempDir()}}),
	}
	err := pa.ValidateConfiguration(context.Background(), workDir)
	if err == nil || !strings.Contains(err.Error(), "ghost-model") {
		t.Fatalf("trusted project default failure = %v, want the model resolution error", err)
	}
}

// A project file Pi cannot load is inert regardless of trust, so its garbage
// must never abort setup while the global default it shadows stays valid.
func TestPiAgent_ValidateConfigurationIgnoresUnloadableProjectSettings(t *testing.T) {
	workDir := t.TempDir()
	projectDir := filepath.Join(workDir, ".pi")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "settings.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	globalDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(globalDir, ".pi", "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, ".pi", "agent", "settings.json"), []byte(`{"defaultModel":"global-model"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pa := &piAgent{
		bin:               writePiProbeStub(t),
		extraArgs:         []string{"--approve"},
		subprocessContext: newSubprocessContext(runenv.Overlay{Set: map[string]string{"HOME": globalDir}}),
	}
	if err := pa.ValidateConfiguration(context.Background(), workDir); err != nil {
		t.Fatalf("ValidateConfiguration: %v", err)
	}
	_, argv := readPiProbe(t, workDir)
	if !strings.Contains(argv, "--model global-model") {
		t.Fatalf("probe argv = %q, want the global default when the project file cannot load", argv)
	}
}

// The failure source must name where the model value came from: a project file
// that only overrides the provider leaves the model owned by the global
// settings.
func TestPiAgent_ValidateConfigurationNamesGlobalSourceForGlobalModel(t *testing.T) {
	workDir := t.TempDir()
	projectSettings := filepath.Join(workDir, ".pi", "settings.json")
	if err := os.MkdirAll(filepath.Dir(projectSettings), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectSettings, []byte(`{"defaultProvider":"openrouter"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	globalDir := t.TempDir()
	globalSettings := filepath.Join(globalDir, ".pi", "agent", "settings.json")
	if err := os.MkdirAll(filepath.Dir(globalSettings), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(globalSettings, []byte(`{"defaultModel":"ghost-model"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := writeFakePi(t, t.TempDir(), `#!/bin/sh
cat > /dev/null
echo 'Error: Model "ghost-model" not found. Use --list-models to see available models.' >&2
exit 1
`, strings.Join([]string{
		"@echo off",
		"more > nul",
		"echo Error: Model not found. Use --list-models to see available models. 1>&2",
		"exit /b 1",
	}, "\r\n"))
	pa := &piAgent{
		bin:               bin,
		subprocessContext: newSubprocessContext(runenv.Overlay{Set: map[string]string{"HOME": globalDir}}),
	}
	err := pa.ValidateConfiguration(context.Background(), workDir)
	if err == nil {
		t.Fatal("expected the global default's resolution failure to fail setup")
	}
	if !strings.Contains(err.Error(), globalSettings) {
		t.Fatalf("validation error = %q, want the global settings path", err)
	}
	if strings.Contains(err.Error(), projectSettings) {
		t.Fatalf("validation error = %q, must not name the project file that supplied no model", err)
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

func TestNewWithOptions_PiCombinesNeutralizationAndRunEnvironment(t *testing.T) {
	t.Setenv("GH_TOKEN", "ambient-token")

	created, err := NewWithOptions(types.AgentPi, "pi", nil, Options{
		DisableProjectSettings: true,
		Environment: runenv.Overlay{
			Set:   map[string]string{"GH_CONFIG_DIR": "/profiles/personal"},
			Unset: []string{"GH_TOKEN"},
		},
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	pa, ok := created.(*piAgent)
	if !ok {
		t.Fatalf("agent type = %T, want *piAgent", created)
	}
	if !pa.NeutralizesGateInstructions() {
		t.Fatal("Pi lost project-instruction neutralization")
	}

	resolved := resolveAgentEnv(pa.gitSafeEnv("/work/dir"))
	if got := resolved["GH_CONFIG_DIR"]; got != "/profiles/personal" {
		t.Fatalf("GH_CONFIG_DIR = %q, want /profiles/personal", got)
	}
	if _, ok := resolved["GH_TOKEN"]; ok {
		t.Fatal("GH_TOKEN remained in Pi environment")
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

// A clean Pi process can still return text that does not satisfy the structured
// output contract. The payload may itself mention a transient-looking status,
// but that text is evidence to diagnose, not a transport failure to replay.
func TestPiAgent_MalformedStructuredOutputIsDiagnosedWithoutRetry(t *testing.T) {
	defer withFastBackoff(t)()

	dir := t.TempDir()
	callsPath := filepath.Join(dir, "calls.txt")
	bin := writeFakePi(t, dir, `#!/bin/sh
printf 'call\n' >> calls.txt
cat > /dev/null
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"upstream returned 503 before the JSON verdict"}]}}'
`, strings.Join([]string{
		"@echo off",
		"echo call>>calls.txt",
		"more > nul",
		"echo {\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"upstream returned 503 before the JSON verdict\"}]}}",
	}, "\r\n"))

	pa := &piAgent{bin: bin}
	_, err := pa.Run(context.Background(), RunOpts{
		Prompt:     "review",
		CWD:        dir,
		JSONSchema: json.RawMessage(`{"type":"object","required":["ok"],"properties":{"ok":{"type":"boolean"}}}`),
	})
	if err == nil {
		t.Fatal("expected malformed-output error")
	}
	if !strings.Contains(err.Error(), "pi output parse") || !strings.Contains(err.Error(), "output snippet") {
		t.Fatalf("error = %q, want the malformed payload boundary", err)
	}
	calls, readErr := os.ReadFile(callsPath)
	if readErr != nil {
		t.Fatalf("read invocation count: %v", readErr)
	}
	if got := strings.Count(string(calls), "call"); got != 1 {
		t.Fatalf("Pi invocations = %d, want 1; malformed output must not consume a retry", got)
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

// TestPiAgent_ToolOnlyStreamStillReportsSubprocessLiveness pins the proof of
// life that separates a working native agent from a wedged one.
//
// Verified against pi 0.84.3: a turn that uses tools emits tool_execution_start
// / tool_execution_update / tool_execution_end and toolcall_* assistant events,
// and no text_delta at all until the very end. Every adapter forwards only
// assistant prose to OnChunk, so for the whole tool-using stretch - which is
// most of a fix round - the pipeline saw nothing and could not tell the agent
// was alive. Subprocess byte liveness is that missing signal.
func TestPiAgent_ToolOnlyStreamStillReportsSubprocessLiveness(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakePi(t, dir, `#!/bin/sh
cat > /dev/null
printf '%s\n' '{"type":"tool_execution_start","tool":"bash"}'
printf '%s\n' '{"type":"tool_execution_update","tool":"bash"}'
printf '%s\n' '{"type":"tool_execution_end","tool":"bash"}'
printf '%s\n' '{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"done"}]}]}'
`, strings.Join([]string{
		"@echo off",
		"more > nul",
		"echo {\"type\":\"tool_execution_start\",\"tool\":\"bash\"}",
		"echo {\"type\":\"tool_execution_update\",\"tool\":\"bash\"}",
		"echo {\"type\":\"tool_execution_end\",\"tool\":\"bash\"}",
		"echo {\"type\":\"agent_end\",\"messages\":[{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"done\"}]}]}",
	}, "\r\n"))

	var chunks []string
	var phases []string
	pa := &piAgent{bin: bin}
	if _, err := pa.Run(context.Background(), RunOpts{
		Prompt:      "fix ci",
		CWD:         t.TempDir(),
		OnChunk:     func(s string) { chunks = append(chunks, s) },
		OnLifecycle: func(e LifecycleEvent) { phases = append(phases, e.Phase) },
	}); err != nil {
		t.Fatalf("run pi: %v", err)
	}

	if len(chunks) != 0 {
		t.Fatalf("OnChunk = %q, want a tool-only turn to stream no assistant prose", chunks)
	}
	activityAt := -1
	exitAt := -1
	for i, phase := range phases {
		if phase == LifecyclePhaseActivity && activityAt < 0 {
			activityAt = i
		}
		if phase == LifecyclePhaseExit {
			exitAt = i
		}
	}
	if activityAt < 0 {
		t.Fatalf("lifecycle phases = %v, want subprocess liveness reported for a tool-only turn", phases)
	}
	if exitAt < 0 || activityAt > exitAt {
		t.Fatalf("lifecycle phases = %v, want liveness reported while the agent was still running", phases)
	}
}

// TestPiAgent_SilentSubprocessReportsNoLiveness is the counter-test: an agent
// that produces no bytes must produce no liveness, or the signal would say
// every agent is fine and the wedge would stay invisible.
func TestPiAgent_SilentSubprocessReportsNoLiveness(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakePi(t, dir, `#!/bin/sh
cat > /dev/null
exit 0
`, strings.Join([]string{
		"@echo off",
		"more > nul",
		"exit 0",
	}, "\r\n"))

	var phases []string
	pa := &piAgent{bin: bin}
	// A pi run with no events yields no text; the error is expected and is not
	// what this test is about.
	_, _ = pa.Run(context.Background(), RunOpts{
		Prompt:      "fix ci",
		CWD:         t.TempDir(),
		OnLifecycle: func(e LifecycleEvent) { phases = append(phases, e.Phase) },
	})

	for _, phase := range phases {
		if phase == LifecyclePhaseActivity {
			t.Fatalf("lifecycle phases = %v, want no liveness from a subprocess that emitted nothing", phases)
		}
	}
	if len(phases) == 0 {
		t.Fatal("expected start and exit lifecycle events even for a silent subprocess")
	}
}
