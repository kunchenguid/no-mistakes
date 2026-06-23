package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCopilotAgent_BuildArgs(t *testing.T) {
	ca := &copilotAgent{bin: "copilot"}
	args := ca.buildArgs("fix the bug")

	expected := []string{
		"-p", "fix the bug",
		"--output-format", "json",
		"--no-color",
		"--no-ask-user",
		"--allow-all-tools",
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

func TestCopilotAgent_BuildArgs_ExtraArgsFirst(t *testing.T) {
	ca := &copilotAgent{bin: "copilot", extraArgs: []string{"--model", "gpt-5.4"}}
	args := ca.buildArgs("fix it")

	expected := []string{
		"--model", "gpt-5.4",
		"-p", "fix it",
		"--output-format", "json",
		"--no-color",
		"--no-ask-user",
		"--allow-all-tools",
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

func TestCopilotAgent_BuildArgs_UserPermissionSuppressesDefault(t *testing.T) {
	tests := []struct {
		name     string
		extra    []string
		suppress bool
	}{
		{"allow-all-tools", []string{"--allow-all-tools"}, true},
		{"allow-all", []string{"--allow-all"}, true},
		{"yolo", []string{"--yolo"}, true},
		{"allow-tool", []string{"--allow-tool", "write"}, true},
		{"allow-tool-eq", []string{"--allow-tool=shell(git:*)"}, true},
		{"excluded-tools", []string{"--excluded-tools", "shell"}, false},
		{"available-tools", []string{"--available-tools", "write"}, false},
		{"deny-tool", []string{"--deny-tool", "shell(rm)"}, false},
		{"allow-all-paths", []string{"--allow-all-paths"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ca := &copilotAgent{bin: "copilot", extraArgs: tt.extra}
			args := ca.buildArgs("p")

			count := 0
			for _, a := range args {
				if a == "--allow-all-tools" {
					count++
				}
			}
			if tt.suppress {
				want := 0
				for _, e := range tt.extra {
					if e == "--allow-all-tools" {
						want = 1
					}
				}
				if count != want {
					t.Errorf("extra=%v expected %d --allow-all-tools, got %d: %v", tt.extra, want, count, args)
				}
			} else if count != 1 {
				t.Errorf("extra=%v expected default --allow-all-tools to be added, got %d: %v", tt.extra, count, args)
			}
		})
	}
}

func TestCopilotAgent_BuildArgs_UserAskUserSuppressesDefault(t *testing.T) {
	ca := &copilotAgent{bin: "copilot", extraArgs: []string{"--no-ask-user"}}
	args := ca.buildArgs("p")

	count := 0
	for _, a := range args {
		if a == "--no-ask-user" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected single --no-ask-user, got %d: %v", count, args)
	}
}

func TestBuildCopilotPrompt_InlinesSchema(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`)
	prompt := buildCopilotPrompt("do the thing", schema)

	if !strings.HasPrefix(prompt, "do the thing") {
		t.Errorf("prompt should start with the user prompt, got %q", prompt)
	}
	if !strings.Contains(prompt, "final output contract") {
		t.Errorf("prompt should include the output contract, got %q", prompt)
	}
	if !strings.Contains(prompt, `"ok"`) {
		t.Errorf("prompt should embed the schema, got %q", prompt)
	}
}

func TestBuildCopilotPrompt_NoSchemaIsUnchanged(t *testing.T) {
	if got := buildCopilotPrompt("hi", nil); got != "hi" {
		t.Errorf("expected unchanged prompt, got %q", got)
	}
}

func TestParseCopilotEvents_FinalMessageAndUsage(t *testing.T) {
	events := strings.Join([]string{
		`{"type":"assistant.message_delta","data":{"deltaContent":"po"}}`,
		`{"type":"assistant.message_delta","data":{"deltaContent":"ng"}}`,
		`{"type":"assistant.message","data":{"content":"","outputTokens":3,"toolRequests":[{"name":"shell"}]}}`,
		`{"type":"assistant.message","data":{"content":"{\"ok\":true}","outputTokens":4}}`,
		`{"type":"result","exitCode":0,"usage":{"premiumRequests":1}}`,
		"",
	}, "\n")

	var chunks []string
	var usage TokenUsage
	var messages []string
	var copilotErr string
	exitCode := -1

	err := parseCopilotEvents(
		context.Background(),
		strings.NewReader(events),
		func(text string) { chunks = append(chunks, text) },
		&usage,
		&messages,
		&copilotErr,
		&exitCode,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := lastOf(messages); got != `{"ok":true}` {
		t.Errorf("last message = %q, want final assistant content", got)
	}
	if strings.Join(chunks, "") != "pong" {
		t.Errorf("chunks = %v, want streamed deltas po+ng", chunks)
	}
	if usage.OutputTokens != 7 {
		t.Errorf("output tokens = %d, want 7 (3+4)", usage.OutputTokens)
	}
	if exitCode != 0 {
		t.Errorf("exit code = %d, want 0", exitCode)
	}
	if copilotErr != "" {
		t.Errorf("copilotErr = %q, want empty", copilotErr)
	}
}

func TestParseCopilotEvents_CapturesErrorEvent(t *testing.T) {
	events := strings.Join([]string{
		`{"type":"error","data":{"message":"model overloaded"}}`,
		`{"type":"result","exitCode":1}`,
		"",
	}, "\n")

	var usage TokenUsage
	var messages []string
	var copilotErr string
	exitCode := 0
	err := parseCopilotEvents(context.Background(), strings.NewReader(events), nil, &usage, &messages, &copilotErr, &exitCode)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if copilotErr != "model overloaded" {
		t.Errorf("copilotErr = %q, want 'model overloaded'", copilotErr)
	}
	if exitCode != 1 {
		t.Errorf("exit code = %d, want 1", exitCode)
	}
}

func TestParseCopilotEvents_SkipsMalformedAndSessionLines(t *testing.T) {
	events := strings.Join([]string{
		"garbage",
		`{"type":"session.mcp_server_status_changed","data":{"serverName":"x","status":"connected"}}`,
		`{"type":"assistant.message","data":{"content":"done","outputTokens":2}}`,
		"",
	}, "\n")

	var usage TokenUsage
	var messages []string
	var copilotErr string
	exitCode := 0
	err := parseCopilotEvents(context.Background(), strings.NewReader(events), nil, &usage, &messages, &copilotErr, &exitCode)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := lastOf(messages); got != "done" {
		t.Errorf("last message = %q, want 'done'", got)
	}
	if usage.OutputTokens != 2 {
		t.Errorf("output tokens = %d, want 2", usage.OutputTokens)
	}
}

// writeFakeCopilot writes a fake copilot binary that emits the given JSONL
// lines on stdout (one echo per line) and exits with exitCode. It returns the
// path to the fake binary.
func writeFakeCopilot(t *testing.T, dir string, jsonlLines []string, exitCode int) string {
	t.Helper()

	name := "copilot"
	if runtime.GOOS == "windows" {
		name = "copilot.cmd"
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
		t.Fatalf("write fake copilot: %v", err)
	}
	return bin
}

func itoa(n int) string { return strings.TrimSpace(jsonNumber(n)) }

func jsonNumber(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// winEchoEscape escapes a JSON line so cmd.exe `echo` emits it verbatim. JSON
// produced by these tests contains no cmd metacharacters except quotes, which
// echo prints literally; escape the shell-significant ones defensively.
func winEchoEscape(s string) string {
	r := strings.NewReplacer(
		"^", "^^",
		"&", "^&",
		"<", "^<",
		">", "^>",
		"|", "^|",
	)
	return r.Replace(s)
}

func TestCopilotAgent_RunParsesJSONOutput(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeCopilot(t, dir, []string{
		`{"type":"assistant.message_delta","data":{"deltaContent":"{\"ok\":true}"}}`,
		`{"type":"assistant.message","data":{"content":"{\"ok\":true}","outputTokens":4}}`,
		`{"type":"result","exitCode":0}`,
	}, 0)

	var chunks []string
	ca := &copilotAgent{bin: bin}
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
	if result.Usage.OutputTokens != 4 {
		t.Errorf("output tokens = %d, want 4", result.Usage.OutputTokens)
	}
	if len(chunks) != 1 || chunks[0] != `{"ok":true}` {
		t.Errorf("chunks = %q", chunks)
	}
}

func TestCopilotAgent_RunReportsErrorOnNonZeroExit(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeCopilot(t, dir, []string{
		`{"type":"error","data":{"message":"not authenticated"}}`,
		`{"type":"result","exitCode":1}`,
	}, 1)

	ca := &copilotAgent{bin: bin}
	_, err := ca.Run(context.Background(), RunOpts{
		Prompt: "do work",
		CWD:    t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected error on non-zero exit")
	}
	if !strings.Contains(err.Error(), "not authenticated") {
		t.Fatalf("error = %v, want copilot error detail", err)
	}
}

func lastOf(messages []string) string {
	if len(messages) == 0 {
		return ""
	}
	return messages[len(messages)-1]
}

func TestParseCopilotEvents_CollectsAllAssistantMessages(t *testing.T) {
	events := strings.Join([]string{
		`{"type":"assistant.message","data":{"content":"{\"ok\":true}","outputTokens":4}}`,
		`{"type":"assistant.message","data":{"content":"","outputTokens":1}}`,
		`{"type":"assistant.message","data":{"content":"Now I've applied the fix.","outputTokens":2}}`,
		`{"type":"result","exitCode":0}`,
		"",
	}, "\n")

	var usage TokenUsage
	var messages []string
	var copilotErr string
	exitCode := 0
	if err := parseCopilotEvents(context.Background(), strings.NewReader(events), nil, &usage, &messages, &copilotErr, &exitCode); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{`{"ok":true}`, "Now I've applied the fix."}
	if len(messages) != len(want) {
		t.Fatalf("messages = %v, want %v (empty content should be skipped)", messages, want)
	}
	for i := range want {
		if messages[i] != want[i] {
			t.Errorf("messages[%d] = %q, want %q", i, messages[i], want[i])
		}
	}
}

// TestFinalizeCopilotResult_RecoversJSONBeforeProseSummary is the core
// regression for the fix-step parse bug: Copilot emitted the schema JSON in an
// earlier assistant message, then closed with a prose summary beginning with
// 'N'. The final message alone cannot be parsed, so scanning newest-first must
// recover the earlier JSON object.
func TestFinalizeCopilotResult_RecoversJSONBeforeProseSummary(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"findings":{"type":"array"},
			"summary":{"type":"string"}
		},
		"required":["findings","summary"]
	}`)
	messages := []string{
		`{"findings":[],"summary":"applied four fixes"}`,
		"Now I've applied all four fixes and verified the build passes.",
	}

	result, err := finalizeCopilotResult(messages, schema, TokenUsage{})
	if err != nil {
		t.Fatalf("expected recovery of earlier JSON message, got error: %v", err)
	}
	var output struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(result.Output, &output); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if output.Summary != "applied four fixes" {
		t.Errorf("summary = %q, want recovered JSON summary", output.Summary)
	}
}

func TestFinalizeCopilotResult_PrefersNewestParsingMessage(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`)
	messages := []string{
		`{"n":1}`,
		`{"n":2}`,
		"All done.",
	}

	result, err := finalizeCopilotResult(messages, schema, TokenUsage{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(result.Output) != `{"n":2}` {
		t.Errorf("output = %s, want newest parsing message {\"n\":2}", string(result.Output))
	}
}

func TestFinalizeCopilotResult_NoSchemaUsesLastMessage(t *testing.T) {
	messages := []string{"first", "second"}
	result, err := finalizeCopilotResult(messages, nil, TokenUsage{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "second" {
		t.Errorf("text = %q, want last message", result.Text)
	}
}

func TestFinalizeCopilotResult_NoneParseReturnsDebuggableError(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`)
	messages := []string{
		"Now I've applied all four fixes and verified the build passes.",
	}

	_, err := finalizeCopilotResult(messages, schema, TokenUsage{})
	if err == nil {
		t.Fatal("expected parse error when no message satisfies the schema")
	}
	if !strings.Contains(err.Error(), "output snippet:") {
		t.Errorf("error should include an output snippet for debuggability, got %v", err)
	}
	if !strings.Contains(err.Error(), "Now I've applied") {
		t.Errorf("error snippet should reflect the final prose message, got %v", err)
	}
}

func TestCopilotAgent_RunRecoversJSONWhenFinalMessageIsProse(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeCopilot(t, dir, []string{
		`{"type":"assistant.message","data":{"content":"{\"findings\":[],\"summary\":\"done\"}","outputTokens":5}}`,
		`{"type":"assistant.message","data":{"content":"Now I have applied the fixes.","outputTokens":3}}`,
		`{"type":"result","exitCode":0}`,
	}, 0)

	ca := &copilotAgent{bin: bin}
	result, err := ca.Run(context.Background(), RunOpts{
		Prompt:     "fix it",
		CWD:        t.TempDir(),
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"findings":{"type":"array"},"summary":{"type":"string"}},"required":["findings","summary"]}`),
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	var output struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(result.Output, &output); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if output.Summary != "done" {
		t.Fatalf("output = %s, want recovered summary 'done'", string(result.Output))
	}
}
