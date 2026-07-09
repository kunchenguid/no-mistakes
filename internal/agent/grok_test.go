package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
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
