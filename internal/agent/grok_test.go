package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestGrokAgent_BuildArgs_Streaming(t *testing.T) {
	ga := &grokAgent{bin: "grok"}
	args := ga.buildArgs("fix the bug", nil)

	expected := []string{
		"-p", "fix the bug",
		"--output-format", "streaming-json",
		"--always-approve",
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

func TestGrokAgent_BuildArgs_SchemaUsesJSONSchema(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`)
	ga := &grokAgent{bin: "grok"}
	args := ga.buildArgs("return structured", schema)

	if !containsPair(args, "-p", "return structured") {
		t.Fatalf("missing prompt args: %v", args)
	}
	if !containsPair(args, "--json-schema", string(schema)) {
		t.Fatalf("missing --json-schema: %v", args)
	}
	for i, a := range args {
		if a == "--output-format" {
			t.Fatalf("should not set --output-format when --json-schema is used (implies json); args=%v at %d", args, i)
		}
	}
	if !containsArg(args, "--always-approve") {
		t.Fatalf("expected default --always-approve: %v", args)
	}
}

func TestGrokAgent_BuildArgs_ExtraArgsFirst(t *testing.T) {
	ga := &grokAgent{bin: "grok", extraArgs: []string{"-m", "grok-build", "--effort", "low"}}
	args := ga.buildArgs("fix it", nil)

	expected := []string{
		"-m", "grok-build",
		"--effort", "low",
		"-p", "fix it",
		"--output-format", "streaming-json",
		"--always-approve",
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

func TestGrokAgent_BuildArgs_UserApprovalSuppressesDefault(t *testing.T) {
	tests := []struct {
		name     string
		extra    []string
		suppress bool
	}{
		{"always-approve", []string{"--always-approve"}, true},
		{"yolo", []string{"--yolo"}, true},
		{"permission-mode", []string{"--permission-mode", "bypassPermissions"}, true},
		{"permission-mode-eq", []string{"--permission-mode=dontAsk"}, true},
		{"model-only", []string{"-m", "grok-build"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ga := &grokAgent{bin: "grok", extraArgs: tt.extra}
			args := ga.buildArgs("p", nil)

			count := 0
			for _, a := range args {
				if a == "--always-approve" {
					count++
				}
			}
			if tt.suppress {
				want := 0
				for _, e := range tt.extra {
					if e == "--always-approve" {
						want = 1
					}
				}
				if count != want {
					t.Errorf("extra=%v expected %d --always-approve, got %d: %v", tt.extra, want, count, args)
				}
			} else if count != 1 {
				t.Errorf("extra=%v expected default --always-approve, got %d: %v", tt.extra, count, args)
			}
		})
	}
}

func TestParseGrokStreamingEvents_TextAndError(t *testing.T) {
	events := strings.Join([]string{
		`{"type":"thought","data":"thinking..."}`,
		`{"type":"text","data":"{\"ok\":"}`,
		`{"type":"text","data":"true}"}`,
		`{"type":"end","stopReason":"EndTurn","sessionId":"abc"}`,
		"",
	}, "\n")

	var chunks []string
	var usage TokenUsage
	var text string
	var grokErr string
	err := parseGrokStreamingEvents(
		context.Background(),
		strings.NewReader(events),
		func(s string) { chunks = append(chunks, s) },
		&usage,
		&text,
		&grokErr,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != `{"ok":true}` {
		t.Errorf("text = %q, want structured JSON", text)
	}
	if strings.Join(chunks, "") != `{"ok":true}` {
		t.Errorf("chunks = %v", chunks)
	}
	if grokErr != "" {
		t.Errorf("grokErr = %q, want empty", grokErr)
	}
}

func TestParseGrokStreamingEvents_CapturesError(t *testing.T) {
	events := strings.Join([]string{
		`{"type":"error","message":"auth failed"}`,
		`{"type":"end","stopReason":"Error"}`,
		"",
	}, "\n")

	var text string
	var grokErr string
	err := parseGrokStreamingEvents(context.Background(), strings.NewReader(events), nil, nil, &text, &grokErr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if grokErr != "auth failed" {
		t.Errorf("grokErr = %q, want 'auth failed'", grokErr)
	}
}

func TestParseGrokJSONResult(t *testing.T) {
	raw := `{"text":"{\"ok\":true}","stopReason":"EndTurn","sessionId":"s1","requestId":"r1"}`
	text, grokErr, err := parseGrokJSONResult([]byte(raw))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if grokErr != "" {
		t.Fatalf("grokErr = %q", grokErr)
	}
	if text != `{"ok":true}` {
		t.Errorf("text = %q", text)
	}
}

func TestParseGrokJSONResult_ErrorObject(t *testing.T) {
	raw := `{"type":"error","message":"Couldn't start session: boom"}`
	text, grokErr, err := parseGrokJSONResult([]byte(raw))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "" {
		t.Errorf("text = %q, want empty", text)
	}
	if grokErr != "Couldn't start session: boom" {
		t.Errorf("grokErr = %q", grokErr)
	}
}

func TestGrokAgent_Name(t *testing.T) {
	if got := (&grokAgent{}).Name(); got != "grok" {
		t.Errorf("Name() = %q, want grok", got)
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func containsPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
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

func TestGrokAgent_Run_StreamingJSON(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeGrok(t, dir, `#!/bin/sh
dir=$(dirname "$0")
: > "$dir/args.txt"
for arg do
  printf '%s\n' "$arg" >> "$dir/args.txt"
done
printf '%s\n' '{"type":"thought","data":"thinking"}'
printf '%s\n' '{"type":"text","data":"{\"ok\":"}'
printf '%s\n' '{"type":"text","data":"true}"}'
printf '%s\n' '{"type":"end","stopReason":"EndTurn","sessionId":"s1"}'
`, strings.Join([]string{
		"@echo off",
		"setlocal",
		"set \"dir=%~dp0\"",
		"if exist \"%dir%args.txt\" del \"%dir%args.txt\"",
		":loop",
		"if \"%~1\"==\"\" goto done",
		">> \"%dir%args.txt\" echo(%~1",
		"shift",
		"goto loop",
		":done",
		"echo {\"type\":\"thought\",\"data\":\"thinking\"}",
		"echo {\"type\":\"text\",\"data\":\"{\\\"ok\\\":true}\"}",
		"echo {\"type\":\"end\",\"stopReason\":\"EndTurn\",\"sessionId\":\"s1\"}",
	}, "\r\n"))

	var chunks []string
	ga := &grokAgent{bin: bin}
	result, err := ga.Run(context.Background(), RunOpts{
		Prompt:  "do work",
		CWD:     t.TempDir(),
		OnChunk: func(s string) { chunks = append(chunks, s) },
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	// Streaming without JSONSchema fills Result.Text (not Output).
	if result.Text != `{"ok":true}` {
		t.Fatalf("Text = %q, want {\"ok\":true}", result.Text)
	}
	if strings.Join(chunks, "") != `{"ok":true}` {
		t.Fatalf("chunks = %v", chunks)
	}

	argsRaw, err := os.ReadFile(filepath.Join(dir, "args.txt"))
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	args := strings.Split(strings.TrimSpace(strings.ReplaceAll(string(argsRaw), "\r\n", "\n")), "\n")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-p do work") {
		t.Fatalf("args missing prompt: %v", args)
	}
	if !strings.Contains(joined, "--output-format streaming-json") {
		t.Fatalf("args missing streaming-json: %v", args)
	}
	if !strings.Contains(joined, "--always-approve") {
		t.Fatalf("args missing --always-approve: %v", args)
	}
	t.Logf("fake grok received args: %v", args)
	t.Logf("agent text: %s", result.Text)
}

func TestGrokAgent_Run_JSONSchema(t *testing.T) {
	dir := t.TempDir()
	schema := `{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`
	bin := writeFakeGrok(t, dir, `#!/bin/sh
dir=$(dirname "$0")
: > "$dir/args.txt"
for arg do
  printf '%s\n' "$arg" >> "$dir/args.txt"
done
printf '%s\n' '{"text":"{\"ok\":true}","stopReason":"EndTurn","sessionId":"s2"}'
`, strings.Join([]string{
		"@echo off",
		"setlocal",
		"set \"dir=%~dp0\"",
		"if exist \"%dir%args.txt\" del \"%dir%args.txt\"",
		":loop",
		"if \"%~1\"==\"\" goto done",
		">> \"%dir%args.txt\" echo(%~1",
		"shift",
		"goto loop",
		":done",
		"echo {\"text\":\"{\\\"ok\\\":true}\",\"stopReason\":\"EndTurn\",\"sessionId\":\"s2\"}",
	}, "\r\n"))

	ga := &grokAgent{bin: bin}
	result, err := ga.Run(context.Background(), RunOpts{
		Prompt:     "return structured",
		CWD:        t.TempDir(),
		JSONSchema: json.RawMessage(schema),
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if string(result.Output) != `{"ok":true}` {
		t.Fatalf("output = %q", string(result.Output))
	}

	argsRaw, err := os.ReadFile(filepath.Join(dir, "args.txt"))
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	argsText := strings.ReplaceAll(string(argsRaw), "\r\n", "\n")
	if !strings.Contains(argsText, "--json-schema") {
		t.Fatalf("missing --json-schema in args:\n%s", argsText)
	}
	if strings.Contains(argsText, "--output-format") {
		t.Fatalf("must not set --output-format with schema:\n%s", argsText)
	}
	t.Logf("fake grok schema-mode args:\n%s", argsText)
	t.Logf("structured agent output: %s", string(result.Output))
}

func TestGrokAgent_Run_ReportsStreamError(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeGrok(t, dir, `#!/bin/sh
printf '%s\n' '{"type":"error","message":"auth failed"}'
printf '%s\n' '{"type":"end","stopReason":"Error"}'
exit 1
`, strings.Join([]string{
		"@echo off",
		"echo {\"type\":\"error\",\"message\":\"auth failed\"}",
		"echo {\"type\":\"end\",\"stopReason\":\"Error\"}",
		"exit /b 1",
	}, "\r\n"))

	ga := &grokAgent{bin: bin}
	_, err := ga.Run(context.Background(), RunOpts{
		Prompt: "do work",
		CWD:    t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "auth failed") {
		t.Fatalf("error = %v, want auth failed detail", err)
	}
}

func TestNewGrokAgent_EndToEndWithRealCLI(t *testing.T) {
	if os.Getenv("NM_TEST_REAL_GROK") != "1" {
		t.Skip("set NM_TEST_REAL_GROK=1 to exercise the real grok CLI")
	}
	if _, err := exec.LookPath("grok"); err != nil {
		t.Skip("grok not on PATH")
	}

	a, err := New(types.AgentGrok, "grok", nil)
	if err != nil {
		t.Fatalf("New(AgentGrok): %v", err)
	}
	if a.Name() != "grok" {
		t.Fatalf("Name() = %q", a.Name())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	schema := json.RawMessage(`{"type":"object","properties":{"pong":{"type":"string"}},"required":["pong"]}`)
	result, err := a.Run(ctx, RunOpts{
		Prompt:     `Reply with JSON only matching the schema: {"pong":"ok"}`,
		CWD:        t.TempDir(),
		JSONSchema: schema,
	})
	if err != nil {
		t.Fatalf("Run with real grok: %v", err)
	}
	t.Logf("real grok structured output: %s", string(result.Output))

	var out struct {
		Pong string `json:"pong"`
	}
	if err := json.Unmarshal(result.Output, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", string(result.Output), err)
	}
	if out.Pong == "" {
		t.Fatalf("expected non-empty pong field, got %s", string(result.Output))
	}
}
