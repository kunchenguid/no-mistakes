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

func TestNew_GrokAgent(t *testing.T) {
	a, err := New(types.AgentGrok, "grok", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.Name() != "grok" {
		t.Errorf("expected name %q, got %q", "grok", a.Name())
	}
	if _, ok := a.(*grokAgent); !ok {
		t.Fatalf("agent type = %T, want *grokAgent", a)
	}
}

func TestNewWithOptions_GrokAgent(t *testing.T) {
	a, err := NewWithOptions(types.AgentGrok, "/path/to/grok", []string{"--model", "grok-3"}, Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ga, ok := a.(*grokAgent)
	if !ok {
		t.Fatalf("agent type = %T, want *grokAgent", a)
	}
	if ga.bin != "/path/to/grok" {
		t.Errorf("bin = %q, want override path", ga.bin)
	}
	if len(ga.extraArgs) != 2 || ga.extraArgs[0] != "--model" || ga.extraArgs[1] != "grok-3" {
		t.Errorf("extraArgs = %v, want [--model grok-3]", ga.extraArgs)
	}
}

func TestBuildGrokArgs_Ordering(t *testing.T) {
	args := buildGrokArgs("/tmp/prompt.md", []string{"--model", "grok-3"}, "/repo")

	want := []string{
		"--model", "grok-3",
		"--prompt-file", "/tmp/prompt.md",
		"--cwd", "/repo",
		"--permission-mode", "bypassPermissions",
		"--output-format", "plain",
	}
	if len(args) != len(want) {
		t.Fatalf("expected %d args, got %d: %v", len(want), len(args), args)
	}
	for i, expected := range want {
		if args[i] != expected {
			t.Errorf("arg[%d]: expected %q, got %q", i, expected, args[i])
		}
	}
	for _, arg := range args {
		if arg == "--effort" || strings.HasPrefix(arg, "--effort=") {
			t.Fatalf("args must not include --effort, got %v", args)
		}
	}
}

func TestBuildGrokPromptIncludesSchemaContract(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`)
	prompt := buildGrokPrompt("do a thing", schema)
	if !strings.Contains(prompt, "do a thing") {
		t.Errorf("prompt missing user prompt: %s", prompt)
	}
	if !strings.Contains(prompt, "no-mistakes final output contract") {
		t.Errorf("prompt missing contract header: %s", prompt)
	}
	if !strings.Contains(prompt, "```json") {
		t.Errorf("prompt missing fenced-json instruction: %s", prompt)
	}
	if !strings.Contains(prompt, "summary") {
		t.Errorf("prompt missing schema property: %s", prompt)
	}
	if !strings.HasPrefix(prompt, "## no-mistakes final output contract") {
		t.Errorf("expected schema contract prepended, got: %q", prompt)
	}
}

func TestGrokPromptTempFileOwnerOnly(t *testing.T) {
	f, err := os.CreateTemp("", "nm-grok-*.md")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	path := f.Name()
	defer os.Remove(path)
	if err := f.Close(); err != nil {
		t.Fatalf("close temp: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat temp: %v", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("prompt temp file should be owner-only, got mode %o", info.Mode().Perm())
	}
}

func TestBuildGrokPromptOmitsContractWhenSchemaEmpty(t *testing.T) {
	prompt := buildGrokPrompt("do a thing", nil)
	if prompt != "do a thing" {
		t.Errorf("expected raw prompt when no schema, got: %q", prompt)
	}
}

func strconvQuote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
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

func TestGrokAgent_RunParsesFencedJSONAndUsesManagedFlags(t *testing.T) {
	dir := t.TempDir()
	argvLog := filepath.Join(dir, "argv.log")
	fenceOpen := "```json"
	fenceClose := "```"
	posixScript := strings.Join([]string{
		"#!/bin/sh",
		"printf '%s\\n' \"$@\" > " + strconvQuote(argvLog),
		"printf '%s\\n' '" + fenceOpen + "'",
		"printf '%s\\n' '{\"ok\":true}'",
		"printf '%s\\n' '" + fenceClose + "'",
	}, "\n")
	bin := writeFakeGrok(t, dir, posixScript, strings.Join([]string{
		"@echo off",
		"echo %* > \"" + strings.ReplaceAll(argvLog, "/", "\\") + "\"",
		"echo " + fenceOpen,
		"echo {\"ok\":true}",
		"echo " + fenceClose,
	}, "\r\n"))

	schema := json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`)
	ga := &grokAgent{bin: bin, extraArgs: []string{"--model", "grok-3"}}
	cwd := t.TempDir()

	var chunks []string
	result, err := ga.Run(context.Background(), RunOpts{
		Prompt:     "review the diff",
		CWD:        cwd,
		JSONSchema: schema,
		OnChunk:    func(s string) { chunks = append(chunks, s) },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(result.Output) != `{"ok":true}` {
		t.Fatalf("unexpected output: %s", string(result.Output))
	}
	if len(chunks) == 0 {
		t.Fatal("expected onChunk to receive streaming text")
	}

	argvBytes, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	argv := strings.Fields(string(argvBytes))
	want := []string{
		"--model", "grok-3",
		"--prompt-file", "PROMPT_PATH",
		"--cwd", cwd,
		"--permission-mode", "bypassPermissions",
		"--output-format", "plain",
	}
	if len(argv) != len(want) {
		t.Fatalf("argv len = %d, want %d: %v", len(argv), len(want), argv)
	}
	for i, expected := range want {
		got := argv[i]
		if expected == "PROMPT_PATH" {
			if !strings.HasPrefix(filepath.Base(got), "nm-grok-") || !strings.HasSuffix(got, ".md") {
				t.Errorf("argv[%d]: expected managed temp prompt path, got %q", i, got)
			}
			continue
		}
		if got != expected {
			t.Errorf("argv[%d]: got %q, want %q", i, got, expected)
		}
	}
}
