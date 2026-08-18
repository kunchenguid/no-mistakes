package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAntigravityAgent_BuildArgs(t *testing.T) {
	a := &antigravityAgent{bin: "agy"}
	args := a.buildArgs("test prompt", "")

	expected := []string{"--dangerously-skip-permissions", "--print", "test prompt", "--output-format", "stream-json"}
	if len(args) != len(expected) {
		t.Fatalf("expected %d args, got %d: %v", len(expected), len(args), args)
	}
	for i, want := range expected {
		if args[i] != want {
			t.Errorf("arg[%d]: expected %q, got %q", i, want, args[i])
		}
	}
}

func TestAntigravityAgent_BuildArgs_WithSchema(t *testing.T) {
	a := &antigravityAgent{bin: "agy"}
	args := a.buildArgs("test prompt", "/tmp/schema.json")

	expected := []string{"--dangerously-skip-permissions", "--print", "test prompt", "--json-schema", "/tmp/schema.json", "--output-format", "stream-json"}
	if len(args) != len(expected) {
		t.Fatalf("expected %d args, got %d: %v", len(expected), len(args), args)
	}
	for i, want := range expected {
		if args[i] != want {
			t.Errorf("arg[%d]: expected %q, got %q", i, want, args[i])
		}
	}
}

func TestAntigravityAgent_BuildArgs_WithExtraArgs(t *testing.T) {
	a := &antigravityAgent{bin: "agy", extraArgs: []string{"--debug"}}
	args := a.buildArgs("test prompt", "")

	expected := []string{"--debug", "--dangerously-skip-permissions", "--print", "test prompt", "--output-format", "stream-json"}
	if len(args) != len(expected) {
		t.Fatalf("expected %d args, got %d: %v", len(expected), len(args), args)
	}
	for i, want := range expected {
		if args[i] != want {
			t.Errorf("arg[%d]: expected %q, got %q", i, want, args[i])
		}
	}
}

func TestAntigravityParser(t *testing.T) {
	stream := `
{"event": "step_update", "step_update": {"text_delta": "hello"}}
{"event": "step_update", "step_update": {"tool_call_delta": " world"}}
{"event": "step_update", "step_update": {"tool_info": {"parameters": {"tool": "info"}}}}
{"event": "step_update", "step_update": {"subagent_info": "doing subagent things"}}
{"event": "step_update", "step_update": {"usage": {"input_tokens": 10, "output_tokens": 5, "cache_read_tokens": 2}}}
{"event": "result", "result": {"status": "SUCCESS"}}
`
	buf := bytes.NewBufferString(stream)
	p := &antigravityParser{}
	err := p.parse(context.Background(), buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	text := p.finalText()
	expectedChunks := []string{
		"hello world",
		`{"tool":"info"}`,
		"doing subagent things",
	}

	for _, chunk := range expectedChunks {
		if !strings.Contains(text, chunk) {
			t.Errorf("expected text to contain %q, got %q", chunk, text)
		}
	}

	if p.usage.InputTokens != 10 {
		t.Errorf("expected 10 input tokens, got %d", p.usage.InputTokens)
	}
	if p.usage.OutputTokens != 5 {
		t.Errorf("expected 5 output tokens, got %d", p.usage.OutputTokens)
	}
	if p.usage.CacheReadTokens != 2 {
		t.Errorf("expected 2 cache read tokens, got %d", p.usage.CacheReadTokens)
	}
	if p.errorMessage != "" {
		t.Errorf("unexpected error message: %s", p.errorMessage)
	}
}

func TestAntigravityParser_StructuredOutputOverride(t *testing.T) {
	stream := `
{"event": "step_update", "step_update": {"text_delta": "hello"}}
{"event": "result", "result": {"status": "SUCCESS", "structured_output": {"success": true}}}
`
	buf := bytes.NewBufferString(stream)
	p := &antigravityParser{}
	err := p.parse(context.Background(), buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	text := p.finalText()
	expected := `{"success":true}`
	if text != expected {
		t.Errorf("expected %q, got %q", expected, text)
	}
}

func TestAntigravityParser_ErrorStatus(t *testing.T) {
	stream := `
{"event": "step_update", "step_update": {"text_delta": "failing"}}
{"event": "result", "result": {"status": "ERROR", "error": "something went wrong"}}
`
	buf := bytes.NewBufferString(stream)
	p := &antigravityParser{}
	err := p.parse(context.Background(), buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.errorMessage != "something went wrong" {
		t.Errorf("expected error message 'something went wrong', got %q", p.errorMessage)
	}
}

// writeFakeAgy writes a fake agy binary that emits the given JSONL
// lines on stdout (one echo per line) and exits with exitCode. It returns the
// path to the fake binary.
func writeFakeAgy(t *testing.T, dir string, jsonlLines []string, exitCode int) string {
	t.Helper()

	name := "agy"
	if runtime.GOOS == "windows" {
		name = "agy.cmd"
	}
	bin := filepath.Join(dir, name)

	var script string
	if runtime.GOOS == "windows" {
		lines := []string{"@echo off"}
		for _, l := range jsonlLines {
			lines = append(lines, "echo "+winEchoEscape(l))
		}
		lines = append(lines, "exit /b "+itoa(exitCode))
		script = strings.Join(lines, "\r\n")
	} else {
		lines := []string{"#!/bin/sh"}
		for _, l := range jsonlLines {
			lines = append(lines, "printf '%s\\n' "+shellSingleQuote(l))
		}
		lines = append(lines, "exit "+itoa(exitCode))
		script = strings.Join(lines, "\n") + "\n"
	}
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake agy: %v", err)
	}
	return bin
}

func TestAntigravityAgent_RunParsesJSONOutput(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeAgy(t, dir, []string{
		`{"event": "step_update", "step_update": {"text_delta": "{\"ok\":true}"}}`,
		`{"event": "result", "result": {"status": "SUCCESS"}}`,
	}, 0)

	var chunks []string
	ca := &antigravityAgent{bin: bin}
	result, err := ca.Run(context.Background(), RunOpts{
		Prompt:     "do work",
		CWD:        t.TempDir(),
		JSONSchema: json.RawMessage(`{"type":"object"}`),
		OnChunk:    func(text string) { chunks = append(chunks, text) },
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	var output map[string]bool
	if err := json.Unmarshal(result.Output, &output); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if !output["ok"] {
		t.Fatalf("output = %s, want ok true", string(result.Output))
	}
	if len(chunks) != 1 || chunks[0] != `{"ok":true}` {
		t.Errorf("chunks = %q", chunks)
	}
}

func TestAntigravityAgent_RunReportsErrorOnNonZeroExit(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeAgy(t, dir, []string{
		`{"event": "result", "result": {"status": "ERROR", "error": "not authenticated"}}`,
	}, 0) // exit with 0 so waitErr is nil, falling through to errorMessage check

	ca := &antigravityAgent{bin: bin}
	_, err := ca.Run(context.Background(), RunOpts{
		Prompt: "do work",
		CWD:    t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected error on non-zero exit")
	}
	if !strings.Contains(err.Error(), "not authenticated") {
		t.Fatalf("error = %v, want antigravity error detail", err)
	}
}
