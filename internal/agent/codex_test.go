package agent

import (
	"context"
	"strings"
	"testing"
)

func TestCodexAgent_BuildArgs(t *testing.T) {
	ca := &codexAgent{bin: "codex"}
	args := ca.buildArgs("fix the bug")

	expected := []string{
		"exec", "fix the bug",
		"--json",
		"--dangerously-bypass-approvals-and-sandbox",
		"--color", "never",
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

func TestCodexAgent_BuildArgs_ExtraArgsAfterExec(t *testing.T) {
	ca := &codexAgent{bin: "codex", extraArgs: []string{"-m", "gpt-5.4"}}
	args := ca.buildArgs("fix it")

	expected := []string{
		"exec",
		"-m", "gpt-5.4",
		"fix it",
		"--json",
		"--dangerously-bypass-approvals-and-sandbox",
		"--color", "never",
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

func TestCodexAgent_BuildArgs_UserExecutionModeSuppressesBypass(t *testing.T) {
	tests := [][]string{
		{"--ask-for-approval", "untrusted"},
		{"--sandbox", "read-only"},
		{"--sandbox=workspace-write"},
		{"--dangerously-bypass-approvals-and-sandbox"},
	}
	for _, extra := range tests {
		ca := &codexAgent{bin: "codex", extraArgs: extra}
		args := ca.buildArgs("p")

		bypassCount := 0
		for _, a := range args {
			if a == "--dangerously-bypass-approvals-and-sandbox" {
				bypassCount++
			}
		}
		if len(extra) == 1 && extra[0] == "--dangerously-bypass-approvals-and-sandbox" {
			if bypassCount != 1 {
				t.Errorf("extra=%v expected single bypass, got %d: %v", extra, bypassCount, args)
			}
		} else if bypassCount != 0 {
			t.Errorf("extra=%v expected no default bypass, got: %v", extra, args)
		}
	}
}

func TestParseCodexEvents_AgentMessage(t *testing.T) {
	events := strings.Join([]string{
		`{"type":"item.completed","item":{"type":"agent_message","text":"{\"success\":true,\"summary\":\"done\"}"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":200,"cached_input_tokens":50,"output_tokens":100}}`,
		"",
	}, "\n")

	var usage TokenUsage
	var lastMessage string

	err := parseCodexEvents(
		context.Background(),
		strings.NewReader(events),
		nil,
		&usage,
		&lastMessage,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lastMessage != `{"success":true,"summary":"done"}` {
		t.Errorf("unexpected last message: %s", lastMessage)
	}
	if usage.InputTokens != 200 {
		t.Errorf("expected input tokens 200, got %d", usage.InputTokens)
	}
	if usage.OutputTokens != 100 {
		t.Errorf("expected output tokens 100, got %d", usage.OutputTokens)
	}
	if usage.CacheReadTokens != 50 {
		t.Errorf("expected cache read tokens 50, got %d", usage.CacheReadTokens)
	}
}

func TestParseCodexEvents_SeparatesMultipleMessages(t *testing.T) {
	events := strings.Join([]string{
		`{"type":"item.completed","item":{"type":"agent_message","text":"first"}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"second"}}`,
		"",
	}, "\n")

	var chunks []string
	var usage TokenUsage
	var lastMessage string

	err := parseCodexEvents(
		context.Background(),
		strings.NewReader(events),
		func(text string) { chunks = append(chunks, text) },
		&usage,
		&lastMessage,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d: %v", len(chunks), chunks)
	}
	if chunks[0] != "first" {
		t.Errorf("expected 'first', got %q", chunks[0])
	}
	if chunks[1] != "second" {
		t.Errorf("expected 'second', got %q", chunks[1])
	}
}

func TestParseCodexEvents_DoesNotSeparateSplitTurnMessages(t *testing.T) {
	events := strings.Join([]string{
		`{"type":"item.completed","item":{"type":"agent_message","text":"hello "}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"world"}}`,
		"",
	}, "\n")

	var chunks []string
	var usage TokenUsage
	var lastMessage string

	err := parseCodexEvents(
		context.Background(),
		strings.NewReader(events),
		func(text string) { chunks = append(chunks, text) },
		&usage,
		&lastMessage,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d: %v", len(chunks), chunks)
	}
	if chunks[0] != "hello " || chunks[1] != "world" {
		t.Fatalf("expected streamed turn chunks, got %v", chunks)
	}
	if lastMessage != "world" {
		t.Fatalf("expected last message 'world', got %q", lastMessage)
	}
}

func TestParseCodexEvents_SkipsMalformedLines(t *testing.T) {
	events := "garbage\n{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}\n"

	var usage TokenUsage
	var lastMessage string
	err := parseCodexEvents(context.Background(), strings.NewReader(events), nil, &usage, &lastMessage)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage.InputTokens != 10 {
		t.Errorf("expected 10 input tokens, got %d", usage.InputTokens)
	}
}
