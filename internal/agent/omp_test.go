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

func TestOMPAgent_BuildArgs(t *testing.T) {
	oa := &ompAgent{bin: "omp"}
	args := oa.buildArgs()

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

func TestOMPAgent_BuildArgs_PrependsExtraArgs(t *testing.T) {
	oa := &ompAgent{bin: "omp", extraArgs: []string{"--model", "sonnet"}}
	args := oa.buildArgs()

	expected := []string{"--model", "sonnet", "--mode", "json", "--no-session"}

	if len(args) != len(expected) {
		t.Fatalf("expected %d args, got %d: %v", len(expected), len(args), args)
	}
	for i, want := range expected {
		if args[i] != want {
			t.Errorf("arg[%d]: expected %q, got %q", i, want, args[i])
		}
	}
}

func TestOMPAgent_BuildPromptIncludesSchema(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`)
	prompt := buildOMPPrompt("do a thing", schema)
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

func TestOMPAgent_BuildPromptOmitsContractWhenSchemaEmpty(t *testing.T) {
	prompt := buildOMPPrompt("do a thing", nil)
	if prompt != "do a thing" {
		t.Errorf("expected raw prompt when no schema, got: %q", prompt)
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
	bin := writeFakeOMP(t, dir, `#!/bin/sh
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
cat > /dev/null
printf '%s\n' '{"type":"agent_end","messages":[{"role":"user","content":"prompt"},{"role":"assistant","content":"{\"ok\":true}"}]}'
`, strings.Join([]string{
		"@echo off",
		"more > nul",
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

func TestOMPAgent_RunReportsAssistantError(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeOMP(t, dir, `#!/bin/sh
cat > /dev/null
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","responseId":"r1","stopReason":"error","errorMessage":"model overloaded"}}'
printf '%s\n' '{"type":"agent_end","messages":[]}'
`, strings.Join([]string{
		"@echo off",
		"more > nul",
		"echo {\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"responseId\":\"r1\",\"stopReason\":\"error\",\"errorMessage\":\"model overloaded\"}}",
		"echo {\"type\":\"agent_end\",\"messages\":[]}",
	}, "\r\n"))

	oa := &ompAgent{bin: bin}
	_, err := oa.Run(context.Background(), RunOpts{
		Prompt: "review",
		CWD:    t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected error for assistant error")
	}
	if !strings.Contains(err.Error(), "model overloaded") {
		t.Errorf("expected error to contain 'model overloaded', got: %v", err)
	}
}

func TestNew_OMP(t *testing.T) {
	a, err := New(types.AgentOMP, "omp", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.Name() != "omp" {
		t.Errorf("expected name %q, got %q", "omp", a.Name())
	}
}

func TestOMPAgent_BuildArgs_WithMultipleExtraArgs(t *testing.T) {
	oa := &ompAgent{bin: "omp", extraArgs: []string{"--provider", "anthropic", "--thinking", "high"}}
	args := oa.buildArgs()

	expected := []string{"--provider", "anthropic", "--thinking", "high", "--mode", "json", "--no-session"}

	if len(args) != len(expected) {
		t.Fatalf("expected %d args, got %d: %v", len(expected), len(args), args)
	}
	for i, want := range expected {
		if args[i] != want {
			t.Errorf("arg[%d]: expected %q, got %q", i, want, args[i])
		}
	}
}
