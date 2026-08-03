package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestClaudeAgent_BuildArgs(t *testing.T) {
	ca := &claudeAgent{bin: "/usr/bin/claude"}
	schema := json.RawMessage(`{"type":"object"}`)
	args := ca.buildArgs(schema, "")

	// Default (no opt-out): pristine args, no setting-sources restriction -
	// ordinary repos keep loading project CLAUDE.md/AGENTS.md (backward-compat).
	expected := []string{
		"-p",
		"--verbose",
		"--output-format", "stream-json",
		"--json-schema", `{"type":"object"}`,
		"--dangerously-skip-permissions",
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

func TestClaudeAgent_BuildArgs_NoSchema(t *testing.T) {
	ca := &claudeAgent{bin: "claude"}
	args := ca.buildArgs(nil, "")

	// Without schema, should not include --json-schema flag
	for _, arg := range args {
		if arg == "--json-schema" {
			t.Error("should not include --json-schema when schema is nil")
		}
	}
	// Should still have core args
	if args[0] != "-p" {
		t.Error("missing -p flag")
	}
}

func TestClaudeAgent_BuildArgs_ExtraArgsPrepended(t *testing.T) {
	ca := &claudeAgent{bin: "claude", extraArgs: []string{"--model", "sonnet"}}
	args := ca.buildArgs(nil, "")

	expected := []string{
		"--model", "sonnet",
		"-p",
		"--verbose",
		"--output-format", "stream-json",
		"--dangerously-skip-permissions",
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

func TestClaudeAgent_BuildArgs_UserPermissionModeSuppressesDefault(t *testing.T) {
	tests := [][]string{
		{"--permission-mode", "acceptEdits"},
		{"--permission-mode=plan"},
		{"--dangerously-skip-permissions"},
	}
	for _, extra := range tests {
		ca := &claudeAgent{bin: "claude", extraArgs: extra}
		args := ca.buildArgs(nil, "")

		dangerCount := 0
		for _, a := range args {
			if a == "--dangerously-skip-permissions" {
				dangerCount++
			}
		}
		if len(extra) == 1 && extra[0] == "--dangerously-skip-permissions" {
			if dangerCount != 1 {
				t.Errorf("extra=%v expected single --dangerously-skip-permissions, got %d: %v", extra, dangerCount, args)
			}
		} else if dangerCount != 0 {
			t.Errorf("extra=%v expected no default --dangerously-skip-permissions, got: %v", extra, args)
		}
	}
}

func TestParseClaudeEvents_AssistantMessage(t *testing.T) {
	events := `{"type":"assistant","message":{"usage":{"input_tokens":100,"output_tokens":50},"content":[{"type":"text","text":"hello world"}]}}
`
	var chunks []string
	var usage TokenUsage

	err := parseClaudeEvents(
		context.Background(),
		strings.NewReader(events),
		func(text string) { chunks = append(chunks, text) },
		&usage,
		nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 || chunks[0] != "hello world" {
		t.Errorf("expected chunk 'hello world', got %v", chunks)
	}
	if usage.InputTokens != 100 {
		t.Errorf("expected input tokens 100, got %d", usage.InputTokens)
	}
	if usage.OutputTokens != 50 {
		t.Errorf("expected output tokens 50, got %d", usage.OutputTokens)
	}
}

func TestParseClaudeEvents_ResultEvent(t *testing.T) {
	output := map[string]any{"success": true, "summary": "done"}
	outputJSON, _ := json.Marshal(output)
	event := map[string]any{
		"type":              "result",
		"subtype":           "success",
		"structured_output": json.RawMessage(outputJSON),
		"usage": map[string]any{
			"input_tokens":  200,
			"output_tokens": 100,
		},
	}
	line, _ := json.Marshal(event)

	var usage TokenUsage
	var result *claudeResult

	err := parseClaudeEvents(
		context.Background(),
		bytes.NewReader(append(line, '\n')),
		nil,
		&usage,
		&result,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected result event")
	}
	if result.Subtype != "success" {
		t.Errorf("expected subtype 'success', got %q", result.Subtype)
	}
	if result.StructuredOutput == nil {
		t.Fatal("expected structured_output")
	}
}

func TestParseClaudeEvents_LargeAssistantEvent(t *testing.T) {
	largeText := strings.Repeat("x", 128*1024)
	line, err := json.Marshal(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"usage": map[string]any{
				"input_tokens":  10,
				"output_tokens": 5,
			},
			"content": []map[string]any{{
				"type": "text",
				"text": largeText,
			}},
		},
	})
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}

	var chunks []string
	var usage TokenUsage

	err = parseClaudeEvents(
		context.Background(),
		bytes.NewReader(append(line, '\n')),
		func(text string) { chunks = append(chunks, text) },
		&usage,
		nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 || chunks[0] != largeText {
		t.Fatalf("unexpected chunks: got %d chunks", len(chunks))
	}
	if usage.InputTokens != 10 || usage.OutputTokens != 5 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
}

func TestParseClaudeEvents_MultipleEvents(t *testing.T) {
	events := strings.Join([]string{
		`{"type":"assistant","message":{"usage":{"input_tokens":50,"output_tokens":10},"content":[{"type":"text","text":"thinking..."}]}}`,
		`{"type":"assistant","message":{"usage":{"input_tokens":50,"output_tokens":40},"content":[{"type":"text","text":"done"}]}}`,
		`{"type":"result","subtype":"success","structured_output":{"success":true},"usage":{"input_tokens":100,"output_tokens":50}}`,
		"",
	}, "\n")

	var chunks []string
	var usage TokenUsage
	var result *claudeResult

	err := parseClaudeEvents(
		context.Background(),
		strings.NewReader(events),
		func(text string) { chunks = append(chunks, text) },
		&usage,
		&result,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d: %v", len(chunks), chunks)
	}
	if chunks[0] != "thinking..." {
		t.Errorf("expected first chunk 'thinking...', got %q", chunks[0])
	}
	if chunks[1] != "done" {
		t.Errorf("expected second chunk 'done', got %q", chunks[1])
	}
	// Usage accumulates across assistant events
	if usage.InputTokens != 100 {
		t.Errorf("expected accumulated input tokens 100, got %d", usage.InputTokens)
	}
	if usage.OutputTokens != 50 {
		t.Errorf("expected accumulated output tokens 50, got %d", usage.OutputTokens)
	}
	if result == nil {
		t.Fatal("expected result event")
	}
}

func TestParseClaudeEvents_NoSeparatorForFirstMessage(t *testing.T) {
	events := `{"type":"assistant","message":{"usage":{"input_tokens":10,"output_tokens":5},"content":[{"type":"text","text":"only message"}]}}
`
	var chunks []string
	var usage TokenUsage

	err := parseClaudeEvents(
		context.Background(),
		strings.NewReader(events),
		func(text string) { chunks = append(chunks, text) },
		&usage,
		nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 || chunks[0] != "only message" {
		t.Errorf("expected 1 chunk 'only message', got %v", chunks)
	}
}

func TestParseClaudeEvents_NoSeparatorAfterToolOnlyEvent(t *testing.T) {
	// First assistant event has only tool_use (no text), second has text.
	// No separator because no text was emitted before.
	events := strings.Join([]string{
		`{"type":"assistant","message":{"usage":{"input_tokens":10,"output_tokens":5},"content":[{"type":"tool_use","text":""}]}}`,
		`{"type":"assistant","message":{"usage":{"input_tokens":10,"output_tokens":5},"content":[{"type":"text","text":"after tools"}]}}`,
		"",
	}, "\n")

	var chunks []string
	var usage TokenUsage

	err := parseClaudeEvents(
		context.Background(),
		strings.NewReader(events),
		func(text string) { chunks = append(chunks, text) },
		&usage,
		nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 || chunks[0] != "after tools" {
		t.Errorf("expected 1 chunk 'after tools', got %v", chunks)
	}
}

func TestParseClaudeEvents_DoesNotSeparateSplitAssistantReply(t *testing.T) {
	events := strings.Join([]string{
		`{"type":"assistant","message":{"usage":{"input_tokens":10,"output_tokens":5},"content":[{"type":"text","text":"hello "}]}}`,
		`{"type":"assistant","message":{"usage":{"input_tokens":10,"output_tokens":5},"content":[{"type":"text","text":"world"}]}}`,
		"",
	}, "\n")

	var chunks []string
	var usage TokenUsage

	err := parseClaudeEvents(
		context.Background(),
		strings.NewReader(events),
		func(text string) { chunks = append(chunks, text) },
		&usage,
		nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d: %v", len(chunks), chunks)
	}
	if chunks[0] != "hello " || chunks[1] != "world" {
		t.Fatalf("expected streamed reply chunks, got %v", chunks)
	}
}

func TestParseClaudeEvents_SkipsMalformedLines(t *testing.T) {
	events := "not json\n{\"type\":\"assistant\",\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":5},\"content\":[{\"type\":\"text\",\"text\":\"ok\"}]}}\n"

	var chunks []string
	var usage TokenUsage

	err := parseClaudeEvents(
		context.Background(),
		strings.NewReader(events),
		func(text string) { chunks = append(chunks, text) },
		&usage,
		nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 || chunks[0] != "ok" {
		t.Errorf("expected 1 chunk 'ok', got %v", chunks)
	}
}

func TestParseClaudeEvents_CacheTokens(t *testing.T) {
	events := `{"type":"assistant","message":{"usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":30,"cache_creation_input_tokens":10},"content":[]}}
`
	var usage TokenUsage
	err := parseClaudeEvents(context.Background(), strings.NewReader(events), nil, &usage, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage.CacheReadTokens != 30 {
		t.Errorf("expected cache read tokens 30, got %d", usage.CacheReadTokens)
	}
	if usage.CacheCreationTokens != 10 {
		t.Errorf("expected cache creation tokens 10, got %d", usage.CacheCreationTokens)
	}
}

func TestParseClaudeEvents_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	// Create a reader that would block — but context cancellation should stop parsing
	events := `{"type":"assistant","message":{"usage":{"input_tokens":10,"output_tokens":5},"content":[{"type":"text","text":"ok"}]}}
`
	var usage TokenUsage
	err := parseClaudeEvents(ctx, strings.NewReader(events), nil, &usage, nil)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

// PERSONAL: skill use is captured from the stream so steps can require a
// specific skill instead of trusting the prompt. An empty (non-nil) slice is
// the load-bearing case - it is what lets a caller prove nothing was invoked.
func TestParseClaudeEvents_CapturesSkillInvocations(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []string
	}{{
		name:    "skill calls captured in order",
		content: `{"type":"tool_use","name":"Skill","input":{"skill":"comprehensive-code-review"}},{"type":"tool_use","name":"Bash","input":{"command":"ls"}},{"type":"tool_use","name":"Skill","input":{"skill":"coding-standards"}}`,
		want:    []string{"comprehensive-code-review", "coding-standards"},
	}, {
		name:    "no skill calls yields empty, not nil",
		content: `{"type":"tool_use","name":"Bash","input":{"command":"ls"}},{"type":"text","text":"done"}`,
		want:    []string{},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			events := `{"type":"assistant","message":{"usage":{"input_tokens":1,"output_tokens":1},"content":[` + tc.content + `]}}
{"type":"result","subtype":"success","usage":{"input_tokens":1,"output_tokens":1}}
`
			var usage TokenUsage
			var result *claudeResult
			if err := parseClaudeEvents(context.Background(), strings.NewReader(events), nil, &usage, &result); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result == nil {
				t.Fatal("expected result")
			}
			if result.skillsUsed == nil {
				t.Fatal("skillsUsed must be non-nil so callers can distinguish it from an adapter that reports nothing")
			}
			if !slices.Equal(result.skillsUsed, tc.want) {
				t.Errorf("skillsUsed = %v, want %v", result.skillsUsed, tc.want)
			}
		})
	}
}

func TestParseClaudeEvents_ErrorResult(t *testing.T) {
	events := `{"type":"result","subtype":"error","is_error":true,"structured_output":null,"usage":{"input_tokens":0,"output_tokens":0}}
`
	var usage TokenUsage
	var result *claudeResult

	err := parseClaudeEvents(context.Background(), strings.NewReader(events), nil, &usage, &result)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected result")
	}
	if !result.IsError {
		t.Error("expected IsError to be true")
	}
}

func TestClaudeAgent_FinalizeResult_NoSchemaAllowsTextOnly(t *testing.T) {
	result, err := finalizeClaudeResult(&claudeResult{
		Subtype: "success",
		text:    "All tests pass. Here's what I fixed:",
	}, nil, TokenUsage{InputTokens: 10, OutputTokens: 5})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "All tests pass. Here's what I fixed:" {
		t.Errorf("unexpected text: %q", result.Text)
	}
	if result.Output != nil {
		t.Fatalf("expected nil structured output, got %s", string(result.Output))
	}
	if result.Usage.InputTokens != 10 || result.Usage.OutputTokens != 5 {
		t.Errorf("unexpected usage: %+v", result.Usage)
	}
}

func TestClaudeAgent_FinalizeResult_WithSchemaRequiresStructuredOutput(t *testing.T) {
	_, err := finalizeClaudeResult(&claudeResult{Subtype: "success", text: "plain text"}, json.RawMessage(`{"type":"object"}`), TokenUsage{})
	if err == nil {
		t.Fatal("expected error when structured output is missing")
	}
	if !errors.Is(err, errNoStructuredOutput) {
		t.Fatalf("expected errNoStructuredOutput, got: %v", err)
	}
}

func TestClaudeAgent_FinalizeResult_ErrorSubtypeNotRetryable(t *testing.T) {
	_, err := finalizeClaudeResult(&claudeResult{Subtype: "error", IsError: true}, json.RawMessage(`{"type":"object"}`), TokenUsage{})
	if err == nil {
		t.Fatal("expected error for error subtype")
	}
	if errors.Is(err, errNoStructuredOutput) {
		t.Fatal("error subtype should not be retryable")
	}
}

func TestParseClaudeEvents_ResultCapturesRawEvent(t *testing.T) {
	events := `{"type":"result","subtype":"success","is_error":false,"structured_output":null}` + "\n"

	var usage TokenUsage
	var result *claudeResult

	err := parseClaudeEvents(context.Background(), strings.NewReader(events), nil, &usage, &result)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected result event")
	}
	if result.rawEvent == nil {
		t.Fatal("expected rawEvent to be captured")
	}
	if !strings.Contains(string(result.rawEvent), `"subtype":"success"`) {
		t.Errorf("rawEvent should contain original JSON, got: %s", string(result.rawEvent))
	}
}

// TestClaudeAgent_BuildArgs_SuppressesProjectMemoryUnderOptOut locks in the
// claude project-settings contract UNDER the trusted opt-out: load only
// user-level settings/memory, never the target repo's project/local
// CLAUDE.md/AGENTS.md or settings. Verified empirically: with project memory
// loaded claude adopts the firstmate identity; with --setting-sources user it
// does not.
func TestClaudeAgent_BuildArgs_SuppressesProjectMemoryUnderOptOut(t *testing.T) {
	ca := &claudeAgent{bin: "claude", disableProjectSettings: true}
	args := ca.buildArgs(nil, "")
	if !claudeArgsContainPair(args, "--setting-sources", "user") {
		t.Errorf("buildArgs = %v, want a `--setting-sources user` pair", args)
	}
}

// TestClaudeAgent_BuildArgs_NoSuppressionWithoutOptOut is the backward-compat
// guarantee: without the opt-out, claude adds no setting-sources restriction and
// loads its project memory exactly as before.
func TestClaudeAgent_BuildArgs_NoSuppressionWithoutOptOut(t *testing.T) {
	ca := &claudeAgent{bin: "claude"}
	args := ca.buildArgs(nil, "")
	for _, a := range args {
		if a == "--setting-sources" {
			t.Errorf("buildArgs = %v, must not restrict setting-sources when the repo did not opt out", args)
		}
	}
}

// TestClaudeAgent_BuildArgs_UserSettingSourcesOverrideWins ensures an operator
// who pinned their own --setting-sources is not double-set even under opt-out.
func TestClaudeAgent_BuildArgs_UserSettingSourcesOverrideWins(t *testing.T) {
	ca := &claudeAgent{bin: "claude", disableProjectSettings: true, extraArgs: []string{"--setting-sources", "user,project"}}
	args := ca.buildArgs(nil, "")
	if claudeArgsContainPair(args, "--setting-sources", "user") {
		t.Errorf("buildArgs = %v, must not add default over a user --setting-sources", args)
	}
}

type claudeStdinObservation struct {
	Args   []string `json:"args"`
	Bytes  int      `json:"bytes"`
	SHA256 string   `json:"sha256"`
	EOF    bool     `json:"eof"`
}

func TestClaudeAgent_LargePromptUsesExactStdinForColdAndResumedRuns(t *testing.T) {
	marker := "CLAUDE_STDIN_SECRET_MARKER_7f9c_"
	prompt := marker + strings.Repeat("x", 2*1024*1024) + "_END"
	sum := sha256.Sum256([]byte(prompt))
	wantHash := hex.EncodeToString(sum[:])
	schema := json.RawMessage(`{"type":"object","required":["ok"]}`)

	for _, tc := range []struct {
		name      string
		sessionID string
	}{
		{name: "cold"},
		{name: "resumed", sessionID: "session-123"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observationPath := filepath.Join(t.TempDir(), "observation.json")
			t.Setenv("NM_CLAUDE_STDIN_HELPER", "read")
			t.Setenv("NM_CLAUDE_STDIN_OBSERVATION", observationPath)
			a := newClaudeStdinHelperAgent(t)
			opts := RunOpts{Prompt: prompt, CWD: t.TempDir(), JSONSchema: schema}
			if tc.sessionID != "" {
				opts.Session = &SessionRef{ID: tc.sessionID}
			}

			result, err := a.runOnce(context.Background(), opts)
			if err != nil {
				t.Fatalf("runOnce with 2 MiB prompt: %v", err)
			}
			if result.SessionID != "helper-session" {
				t.Fatalf("session ID = %q, want helper-session", result.SessionID)
			}

			data, err := os.ReadFile(observationPath)
			if err != nil {
				t.Fatalf("read helper observation: %v", err)
			}
			var got claudeStdinObservation
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("parse helper observation: %v", err)
			}
			if got.Bytes != len(prompt) || got.SHA256 != wantHash || !got.EOF {
				t.Fatalf("stdin observation = %+v, want bytes=%d sha256=%s EOF", got, len(prompt), wantHash)
			}
			for _, arg := range got.Args {
				if strings.Contains(arg, marker) || strings.Contains(arg, prompt[len(prompt)-32:]) {
					t.Fatalf("prompt bytes leaked into argv: arg length %d", len(arg))
				}
			}
			for _, pair := range [][2]string{{"--output-format", "stream-json"}, {"--json-schema", string(schema)}, {"--setting-sources", "user"}} {
				if !claudeArgsContainPair(got.Args, pair[0], pair[1]) {
					t.Errorf("argv %v missing %q %q", got.Args, pair[0], pair[1])
				}
			}
			for _, flag := range []string{"-p", "--verbose", "--dangerously-skip-permissions"} {
				if !slicesContain(got.Args, flag) {
					t.Errorf("argv %v missing %q", got.Args, flag)
				}
			}
			if tc.sessionID == "" {
				if slicesContain(got.Args, "--resume") {
					t.Fatalf("cold argv unexpectedly contains --resume: %v", got.Args)
				}
			} else if !claudeArgsContainPair(got.Args, "--resume", tc.sessionID) {
				t.Fatalf("resumed argv %v missing session ID", got.Args)
			}
		})
	}
}

func TestClaudeAgent_EarlyExitWithoutReadingLargeStdinDoesNotLeakGoroutines(t *testing.T) {
	t.Setenv("NM_CLAUDE_STDIN_HELPER", "exit-early")
	a := newClaudeStdinHelperAgent(t)
	prompt := strings.Repeat("e", 2*1024*1024)
	before := runtime.NumGoroutine()

	for i := 0; i < 8; i++ {
		started := time.Now()
		_, err := a.runOnce(context.Background(), RunOpts{Prompt: prompt, CWD: t.TempDir()})
		if err == nil {
			t.Fatal("early helper exit unexpectedly succeeded")
		}
		if time.Since(started) > 3*time.Second {
			t.Fatalf("early helper exit took too long: %v", time.Since(started))
		}
	}

	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > before+2 && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(20 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > before+2 {
		t.Fatalf("goroutines grew from %d to %d after early stdin exits", before, got)
	}
}

func TestClaudeAgent_CancellationWithBlockedStdinAndInheritedPipesIsBounded(t *testing.T) {
	for _, mode := range []string{"block", "spawn-grandchild"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("NM_CLAUDE_STDIN_HELPER", mode)
			ready := filepath.Join(t.TempDir(), "ready")
			t.Setenv("NM_CLAUDE_STDIN_READY", ready)
			t.Setenv("NM_CLAUDE_STDIN_PID", filepath.Join(t.TempDir(), "grandchild.pid"))
			a := newClaudeStdinHelperAgent(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := a.runOnce(ctx, RunOpts{Prompt: strings.Repeat("p", 2*1024*1024), CWD: t.TempDir()})
				done <- err
			}()

			waitForClaudeHelperFile(t, ready, 5*time.Second)
			if mode == "block" {
				cancel()
			}
			select {
			case <-done:
			case <-time.After(6 * time.Second):
				cancel()
				t.Fatal("Claude run did not complete after cancellation or clean leader exit")
			}
		})
	}
}

// The review-skill mandate fails open on a nil SkillsUsed, so the adapter seam
// that fills it has to be exercised end to end rather than assumed: this drives
// the real subprocess reader against a claude stream that invokes a skill, and
// fails if the assignment is ever dropped.
func TestClaudeAgent_RunOnceReportsSkillsUsedFromTheStream(t *testing.T) {
	t.Setenv("NM_CLAUDE_STDIN_HELPER", "skill")
	a := newClaudeStdinHelperAgent(t)

	res, err := a.runOnce(context.Background(), RunOpts{Prompt: "review the change", CWD: t.TempDir()})
	if err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if res.SkillsUsed == nil {
		t.Fatal("SkillsUsed is nil, so the mandate that fails open on nil can never see a skill")
	}
	if !slices.Contains(res.SkillsUsed, "comprehensive-code-review") {
		t.Fatalf("SkillsUsed = %v, want it to carry the invoked skill", res.SkillsUsed)
	}
}

func newClaudeStdinHelperAgent(t *testing.T) *claudeAgent {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("current test executable: %v", err)
	}
	return &claudeAgent{
		bin:                    exe,
		extraArgs:              []string{"-test.run=^TestClaudeStdinHelper$", "--"},
		disableProjectSettings: true,
	}
}

func TestClaudeStdinHelper(t *testing.T) {
	mode := os.Getenv("NM_CLAUDE_STDIN_HELPER")
	if mode == "" {
		return
	}
	args := argsAfterDoubleDash(os.Args)
	switch mode {
	case "exit-early":
		os.Exit(0)
	case "block":
		_ = os.WriteFile(os.Getenv("NM_CLAUDE_STDIN_READY"), []byte("ready"), 0o644)
		for {
			time.Sleep(time.Second)
		}
	case "spawn-grandchild":
		child := exec.Command(os.Args[0], "-test.run=^TestClaudeStdinHelper$")
		child.Env = append(os.Environ(), "NM_CLAUDE_STDIN_HELPER=grandchild")
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		_ = os.WriteFile(os.Getenv("NM_CLAUDE_STDIN_PID"), []byte(strconv.Itoa(child.Process.Pid)), 0o644)
		_ = os.WriteFile(os.Getenv("NM_CLAUDE_STDIN_READY"), []byte("ready"), 0o644)
		emitClaudeHelperResult()
		return
	case "grandchild":
		for {
			time.Sleep(time.Second)
		}
	case "stall", "stall-then-succeed":
		// Reproduce a stalled claude stream exactly as the CLI reports one: the
		// diagnostic arrives as assistant text on stdout, stderr stays empty,
		// and no result event is ever emitted.
		attempt := recordClaudeHelperAttempt()
		if mode == "stall-then-succeed" && attempt > 1 {
			emitClaudeHelperResult()
			return
		}
		_, _ = io.WriteString(os.Stdout, `{"type":"assistant","session_id":"helper-session","message":{"model":"helper-model","usage":{"input_tokens":1,"output_tokens":1},"content":[{"type":"text","text":"API Error: Stream idle timeout - no chunks received"}]}}`+"\n")
		os.Exit(1)
	case "stall-then-hang":
		// A stalled claude that never exits: it writes a deprecation notice on
		// stderr, reports the stall as assistant text, emits one more event,
		// then hangs holding both pipes open so the reader is still parsing
		// when the caller's context is cancelled.
		_, _ = io.WriteString(os.Stderr, "warning: --print is deprecated\n")
		_, _ = io.WriteString(os.Stdout, `{"type":"assistant","session_id":"helper-session","message":{"model":"helper-model","usage":{"input_tokens":1,"output_tokens":1},"content":[{"type":"text","text":"API Error: Stream idle timeout - no chunks received"}]}}`+"\n")
		_, _ = io.WriteString(os.Stdout, `{"type":"assistant","message":{"content":[{"type":"text","text":"still waiting"}]}}`+"\n")
		for {
			time.Sleep(time.Second)
		}
	case "skill":
		// A claude that invokes one skill: the CLI reports it as a tool_use
		// content block on the assistant event, which is the only wire the
		// adapter can learn skill usage from.
		_, _ = io.Copy(io.Discard, os.Stdin)
		_, _ = io.WriteString(os.Stdout, `{"type":"assistant","session_id":"helper-session","message":{"model":"helper-model","usage":{"input_tokens":1,"output_tokens":1},"content":[{"type":"tool_use","name":"Skill","input":{"skill":"comprehensive-code-review"}}]}}`+"\n")
		emitClaudeHelperResult()
		return
	case "read":
		prompt, err := io.ReadAll(os.Stdin)
		if err != nil {
			os.Exit(3)
		}
		sum := sha256.Sum256(prompt)
		observation := claudeStdinObservation{
			Args:   args,
			Bytes:  len(prompt),
			SHA256: hex.EncodeToString(sum[:]),
			EOF:    true,
		}
		data, _ := json.Marshal(observation)
		if err := os.WriteFile(os.Getenv("NM_CLAUDE_STDIN_OBSERVATION"), data, 0o644); err != nil {
			os.Exit(4)
		}
		emitClaudeHelperResult()
		return
	default:
		os.Exit(5)
	}
}

// recordClaudeHelperAttempt appends one byte per invocation to the counter file
// and returns the 1-indexed attempt number, so the helper can behave
// differently across the retry loop's separate processes.
func recordClaudeHelperAttempt() int {
	path := os.Getenv("NM_CLAUDE_STDIN_ATTEMPTS")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		os.Exit(6)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString("x"); err != nil {
		os.Exit(6)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		os.Exit(6)
	}
	return len(data)
}

func emitClaudeHelperResult() {
	_, _ = io.WriteString(os.Stdout, `{"type":"assistant","session_id":"helper-session","message":{"model":"helper-model","usage":{"input_tokens":1,"output_tokens":1},"content":[{"type":"text","text":"ok"}]}}`+"\n")
	_, _ = io.WriteString(os.Stdout, `{"type":"result","subtype":"success","session_id":"helper-session","structured_output":{"ok":true}}`+"\n")
}

func argsAfterDoubleDash(args []string) []string {
	for i, arg := range args {
		if arg == "--" {
			return append([]string(nil), args[i+1:]...)
		}
	}
	return nil
}

func waitForClaudeHelperFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for helper file %s", path)
}

func slicesContain(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func claudeArgsContainPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// The claude CLI reports its own transport failures as assistant text on the
// stdout event stream and leaves stderr empty, so these cover the two halves of
// surviving one: the exit error has to name the cause, and classifyTransient
// has to see that name in order to spend a retry.

func TestClaudeStream_APIErrorsKeepOnlyCLIDiagnostics(t *testing.T) {
	var stream claudeStream
	onChunk := stream.tee(nil)
	onChunk("Scope pinned. Spawning all five review agents in one batch.")
	onChunk("API Error: Response stalled mid-stream. The response above may be incomplete.")
	onChunk("Standards agent stalled on the large diff. Respawning it.")
	onChunk("API Error: Response stalled mid-stream. The response above may be incomplete.")
	onChunk("API Error: Stream idle timeout - no chunks received\n")

	got := stream.apiErrors()
	for _, want := range []string{
		"API Error: Response stalled mid-stream. The response above may be incomplete.",
		"API Error: Stream idle timeout - no chunks received",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("api errors %q missing %q", got, want)
		}
	}
	if n := strings.Count(got, "Response stalled"); n != 1 {
		t.Errorf("repeated diagnostic kept %d times, want 1: %q", n, got)
	}
	for _, leaked := range []string{"Scope pinned", "Standards agent stalled", "Respawning"} {
		if strings.Contains(got, leaked) {
			t.Errorf("model output %q leaked into persisted diagnostics: %q", leaked, got)
		}
	}
}

func TestClaudeStream_APIErrorsAreBounded(t *testing.T) {
	var stream claudeStream
	onChunk := stream.tee(nil)
	for i := 0; i < 200; i++ {
		onChunk("API Error: " + strings.Repeat("x", 400) + strconv.Itoa(i) + "\n")
	}
	if got := len(stream.apiErrors()); got > claudeAPIErrorLimit {
		t.Fatalf("api errors length %d exceeds bound %d", got, claudeAPIErrorLimit)
	}
}

// The bound is a byte count over CLI text that can carry multi-byte runes, and
// the diagnostics it cuts are persisted to runs.error. The payload places a
// two-byte rune across the limit, so a naive byte slice would store half of one
// and render as a replacement character.
func TestClaudeStream_APIErrorsCutOnRuneBoundary(t *testing.T) {
	diagnostic := "API Error: " + strings.Repeat("\u00e9", claudeAPIErrorLimit)
	if utf8.RuneStart(diagnostic[claudeAPIErrorLimit]) {
		t.Fatalf("payload does not exercise a mid-rune cut at byte %d", claudeAPIErrorLimit)
	}

	var stream claudeStream
	stream.tee(nil)(diagnostic + "\n")

	got := stream.apiErrors()
	if len(got) > claudeAPIErrorLimit {
		t.Fatalf("api errors length %d exceeds bound %d", len(got), claudeAPIErrorLimit)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("bounded api errors are not valid UTF-8: %q", got)
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Fatalf("bounded api errors emitted a replacement character: %q", got)
	}
}

func TestClaudeStream_TeeForwardsEveryChunk(t *testing.T) {
	var forwarded []string
	var stream claudeStream
	onChunk := stream.tee(func(chunk string) { forwarded = append(forwarded, chunk) })
	onChunk("one")
	onChunk("two")
	if !slices.Equal(forwarded, []string{"one", "two"}) {
		t.Fatalf("forwarded chunks = %v, want [one two]", forwarded)
	}
}

func TestClaudeExitError_ReportsStreamDiagnosticWhenStderrEmpty(t *testing.T) {
	var stream claudeStream
	stream.tee(nil)("API Error: Stream idle timeout - no chunks received")

	err := claudeExitError(errors.New("exit status 1"), nil, &stream)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "Stream idle timeout") {
		t.Fatalf("error %q does not name the stream failure", err)
	}
	if _, ok := classifyTransient(err); !ok {
		t.Fatalf("error %q is not classified transient, so the run would not retry", err)
	}
}

// Both channels are reported: stderr leads, and the stream diagnostic still
// reaches the error. Anything at all on stderr - a warning, a deprecation
// notice - used to suppress the stall detail entirely, and that detail is the
// only text classifyTransient can match to spend a retry on.
func TestClaudeExitError_ReportsStderrAndStreamDetail(t *testing.T) {
	var stream claudeStream
	stream.tee(nil)("API Error: Stream idle timeout - no chunks received")

	err := claudeExitError(errors.New("exit status 1"), []byte("warning: --print is deprecated"), &stream)
	if !strings.Contains(err.Error(), "--print is deprecated") {
		t.Fatalf("error %q dropped the stderr detail", err)
	}
	if !strings.Contains(err.Error(), "Stream idle timeout") {
		t.Fatalf("error %q dropped the stream diagnostic behind stderr noise", err)
	}
	if _, ok := classifyTransient(err); !ok {
		t.Fatalf("error %q is not classified transient, so the stalled run would not retry", err)
	}
}

// A stderr failure that already names the cause is reported once, not twice.
func TestClaudeExitError_DoesNotRepeatADetailStderrAlreadyCarries(t *testing.T) {
	var stream claudeStream
	stream.tee(nil)("API Error: Stream idle timeout - no chunks received")

	err := claudeExitError(errors.New("exit status 1"), []byte("API Error: Stream idle timeout - no chunks received"), &stream)
	if n := strings.Count(err.Error(), "Stream idle timeout"); n != 1 {
		t.Fatalf("error %q names the same diagnostic %d times, want 1", err, n)
	}
}

// The CLI's prefix is a line prefix. Model prose that merely mentions it is
// not a transport diagnostic, and capturing it would present the model's own
// words as why claude stopped - persisted to runs.error.
func TestClaudeStream_IgnoresTheLiteralInsideModelProse(t *testing.T) {
	var stream claudeStream
	onChunk := stream.tee(nil)
	onChunk("The retry classifier matches on API Error: overloaded_error, which is why the run retries.\n")

	if got := stream.apiErrors(); got != "" {
		t.Fatalf("api errors = %q, want empty: model prose is not a transport diagnostic", got)
	}

	onChunk("API Error: Stream idle timeout - no chunks received\n")
	if got := stream.apiErrors(); !strings.Contains(got, "Stream idle timeout") {
		t.Fatalf("api errors = %q, want the CLI's own diagnostic kept", got)
	}
	if strings.Contains(stream.apiErrors(), "retry classifier") {
		t.Fatalf("api errors = %q leaked model prose", stream.apiErrors())
	}
}

func TestClaudeExitError_NoTrailingSeparatorWithoutDetail(t *testing.T) {
	err := claudeExitError(errors.New("exit status 1"), nil, &claudeStream{})
	if got := err.Error(); got != "claude exited: exit status 1" {
		t.Fatalf("error = %q, want no empty detail separator", got)
	}
}

func TestClaudeAgent_StreamStallIsReportedAndRetried(t *testing.T) {
	defer withFastBackoff(t)()
	countPath := filepath.Join(t.TempDir(), "attempts")
	t.Setenv("NM_CLAUDE_STDIN_HELPER", "stall-then-succeed")
	t.Setenv("NM_CLAUDE_STDIN_ATTEMPTS", countPath)
	a := newClaudeStdinHelperAgent(t)

	result, err := a.Run(context.Background(), RunOpts{Prompt: "review", CWD: t.TempDir()})
	if err != nil {
		t.Fatalf("run through a recoverable stream stall: %v", err)
	}
	if result.SessionID != "helper-session" {
		t.Fatalf("session ID = %q, want helper-session", result.SessionID)
	}
	if got := claudeHelperAttempts(t, countPath); got != 2 {
		t.Fatalf("claude invoked %d times, want 2 (one stall, one success)", got)
	}
}

func TestClaudeAgent_ExhaustedStreamStallNamesTheCause(t *testing.T) {
	defer withFastBackoff(t)()
	t.Setenv("NM_CLAUDE_STDIN_HELPER", "stall")
	t.Setenv("NM_CLAUDE_STDIN_ATTEMPTS", filepath.Join(t.TempDir(), "attempts"))
	a := newClaudeStdinHelperAgent(t)

	_, err := a.Run(context.Background(), RunOpts{Prompt: "review", CWD: t.TempDir()})
	if err == nil {
		t.Fatal("expected the exhausted stall to fail")
	}
	if !strings.Contains(err.Error(), "Stream idle timeout") {
		t.Fatalf("error %q does not name the stream failure", err)
	}
}

// A stalled claude that hangs instead of exiting never reaches wait, so the
// trigger here is this test's own OnChunk calling cancel() once the stall
// diagnostic arrives; parseClaudeEvents observes the cancelled context at the
// top of its next loop iteration and the failure leaves through the
// parse-error path rather than claudeExitError. That path has to explain the
// stop from both channels - the stderr text and the stall diagnostic the CLI
// put on the stream - because they are the only evidence of why the run
// stopped making progress. Retrying is deliberately NOT asserted: a cancelled
// parse error is never retried, which is why the sibling case below covers a
// non-cancellation read failure.
func TestClaudeAgent_StreamReadFailureCarriesBothChannels(t *testing.T) {
	t.Setenv("NM_CLAUDE_STDIN_HELPER", "stall-then-hang")
	a := newClaudeStdinHelperAgent(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts := RunOpts{Prompt: "review", CWD: t.TempDir()}
	opts.OnChunk = func(chunk string) {
		if strings.Contains(chunk, "Stream idle timeout") {
			cancel()
		}
	}

	_, err := a.runOnce(ctx, opts)
	if err == nil {
		t.Fatal("expected the interrupted event stream to fail")
	}
	if !strings.Contains(err.Error(), "parse events") {
		t.Fatalf("error %q did not take the parse-error path", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error %q lost the cancellation cause", err)
	}
	if !strings.Contains(err.Error(), "warning: --print is deprecated") {
		t.Errorf("error %q dropped the stderr detail", err)
	}
	if !strings.Contains(err.Error(), "Stream idle timeout") {
		t.Errorf("error %q dropped the stream diagnostic", err)
	}
}

// The other way the event stream stops being readable is a read failure on the
// pipe, and that one is retryable. The read failure chosen here says nothing a
// transient needle matches, so the bare wrap must classify non-transient and
// the only thing that can make the same failure retryable is the stall
// diagnostic the parse-error path appends: drop that suffix and a recoverable
// stall fails the run on its first attempt.
func TestClaudeParseError_ReadFailureWithStreamDetailRetries(t *testing.T) {
	var stream claudeStream
	stream.tee(nil)("API Error: Stream idle timeout - no chunks received")

	readErr := errors.New("read |0: file already closed")
	bare := fmt.Errorf("claude parse events: %w", readErr)
	if label, ok := classifyTransient(bare); ok {
		t.Fatalf("error %q classified transient as %q on its own text, so the detail proves nothing", bare, label)
	}

	detail := claudeFailureDetail([]byte("warning: --print is deprecated"), &stream)
	err := fmt.Errorf("claude parse events: %w: %s", readErr, detail)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %q is a cancellation, which is never retried", err)
	}
	if _, ok := classifyTransient(err); !ok {
		t.Fatalf("error %q is not classified transient, so the stalled run would not retry", err)
	}
}

func claudeHelperAttempts(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read helper attempt counter: %v", err)
	}
	return len(strings.TrimSpace(string(data)))
}
