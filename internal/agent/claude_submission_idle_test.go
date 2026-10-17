package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// claudeEarlyPartialSubmission covers 2 of the changed files, made while the
// background reviewers were still running.
const claudeEarlyPartialSubmission = `{"findings":[],"risk_level":"medium","reviewed_paths":["a_test.go","b_test.go"]}`

// claudeSubmitThenIdleStream replays the shape of a Review session that ended
// on its invocation budget: background fan-out, an early partial
// StructuredOutput submission, a prose verdict as each background group
// finishes, and an announced re-submission that never comes. There is no
// result event; the caller decides whether the stream ever closes.
func claudeSubmitThenIdleStream(t testing.TB, withSubmission bool) string {
	t.Helper()
	var b strings.Builder
	write := func(event map[string]any) {
		line, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	assistant := func(id string, parent any, content ...map[string]any) {
		write(map[string]any{
			"type": "assistant", "session_id": "idle-session", "parent_tool_use_id": parent,
			"message": map[string]any{"id": id, "model": "claude-test", "content": content},
		})
	}
	toolResult := func(toolUseID string, parent any, text string) {
		write(map[string]any{
			"type": "user", "session_id": "idle-session", "parent_tool_use_id": parent,
			"message": map[string]any{"content": []map[string]any{{
				"type": "tool_result", "tool_use_id": toolUseID, "content": text,
			}}},
		})
	}
	text := func(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

	assistant("msg_plan", nil, text("The mechanical checks came back clean. Next I'm splitting the test files across parallel reviewers."))
	for _, group := range []string{"1", "2", "3", "4", "5"} {
		id := "toolu_task_" + group
		assistant("msg_task_"+group, nil, map[string]any{
			"type": "tool_use", "id": id, "name": "Task",
			"input": map[string]any{"description": "review group " + group, "run_in_background": true},
		})
		toolResult(id, nil, "Async agent launched successfully.")
	}
	assistant("msg_wait", nil, text("Five reviewers are reading the test-file groups. I'll compile findings once they finish."))
	if withSubmission {
		assistant("msg_submit", nil, map[string]any{
			"type": "tool_use", "id": "toolu_submit", "name": claudeStructuredOutputTool,
			"input": json.RawMessage(claudeEarlyPartialSubmission),
		})
		toolResult("toolu_submit", nil, "Structured output provided successfully")
	}
	// A background reviewer's own tool call never gets a result on the
	// stream; it must not count as the session's work in flight.
	assistant("msg_sub", "toolu_task_1", map[string]any{
		"type": "tool_use", "id": "toolu_sub_read", "name": "Read", "input": map[string]any{"file_path": "a_test.go"},
	})
	for i, tail := range []string{
		"No change to the submitted result.",
		"Three groups (61 files) are still running.",
		"Two groups (40 files) are still running, and I'll send an updated result once they finish.",
		"One group of 20 files is still running, and I'll submit the full result when it finishes.",
		"",
	} {
		assistant("msg_verdict_"+string(rune('1'+i)), nil, text("**Clean: no defects in any of the 20 files.** "+tail))
	}
	return b.String()
}

func withClaudeSubmittedIdleGrace(t *testing.T, grace time.Duration) {
	t.Helper()
	prev := claudeSubmittedIdleGrace
	claudeSubmittedIdleGrace = grace
	t.Cleanup(func() { claudeSubmittedIdleGrace = prev })
}

// openClaudeStream feeds stream to a reader that never reaches EOF while the
// test runs, like a session that never exits.
func openClaudeStream(t *testing.T, stream string) io.Reader {
	t.Helper()
	r, w := io.Pipe()
	t.Cleanup(func() { _ = w.Close() })
	go func() { _, _ = io.WriteString(w, stream) }()
	return r
}

func TestParseClaudeEvents_IdleAfterPartialSubmissionKeepsIt(t *testing.T) {
	withClaudeSubmittedIdleGrace(t, 50*time.Millisecond)
	var usage TokenUsage
	var result *claudeResult
	done := make(chan error, 1)
	go func() {
		done <- parseClaudeEvents(context.Background(), openClaudeStream(t, claudeSubmitThenIdleStream(t, true)), nil, &usage, &result)
	}()

	select {
	case err := <-done:
		if !errors.Is(err, errClaudeIdleAfterSubmission) {
			t.Fatalf("parse error = %v, want errClaudeIdleAfterSubmission", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("parse kept waiting on a session idle after its submission")
	}
	if result == nil {
		t.Fatal("expected the accepted submission to be recorded as the result")
	}
	res, err := finalizeClaudeResult(result, json.RawMessage(`{"type":"object"}`), usage)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if string(res.Output) != claudeEarlyPartialSubmission {
		t.Fatalf("output = %s, want the early partial submission", res.Output)
	}
	if result.sessionID != "idle-session" {
		t.Fatalf("session ID = %q, want idle-session", result.sessionID)
	}
}

func TestParseClaudeEvents_IdleProseVerdictWithoutSubmissionIsNotAResult(t *testing.T) {
	withClaudeSubmittedIdleGrace(t, 20*time.Millisecond)
	assertClaudeParseStillWaiting(t, claudeSubmitThenIdleStream(t, false))
}

func TestParseClaudeEvents_InFlightToolHoldsOffIdleEnd(t *testing.T) {
	withClaudeSubmittedIdleGrace(t, 20*time.Millisecond)
	stream := claudeSubmissionStream(t, claudeSubmissionCall{id: "toolu_1", input: claudeEarlyPartialSubmission}) +
		`{"type":"assistant","parent_tool_use_id":null,"message":{"id":"msg_bash","content":[{"type":"tool_use","id":"toolu_bash","name":"Bash","input":{"command":"make test"}}]}}` + "\n"
	assertClaudeParseStillWaiting(t, stream)
}

// assertClaudeParseStillWaiting checks parse neither returns nor records a
// result on its own well past the idle grace, and stops only on cancel.
func assertClaudeParseStillWaiting(t *testing.T, stream string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var usage TokenUsage
	var result *claudeResult
	done := make(chan error, 1)
	go func() {
		done <- parseClaudeEvents(ctx, openClaudeStream(t, stream), nil, &usage, &result)
	}()

	select {
	case err := <-done:
		t.Fatalf("parse returned %v before cancellation", err)
	case <-time.After(10 * claudeSubmittedIdleGrace):
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("parse error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("parse did not stop on cancellation")
	}
	if result != nil {
		t.Fatalf("recorded result %+v, want none", result)
	}
}

func TestClaudeAgent_IdleAfterPartialSubmissionEndsTheSessionKeepingIt(t *testing.T) {
	withClaudeSubmittedIdleGrace(t, 200*time.Millisecond)
	t.Setenv("NM_CLAUDE_STDIN_HELPER", "submit-then-idle")
	a := newClaudeStdinHelperAgent(t)

	var chunks []string
	done := make(chan struct{})
	var res *Result
	var err error
	go func() {
		defer close(done)
		res, err = a.runOnce(context.Background(), RunOpts{
			Prompt:     "review",
			CWD:        t.TempDir(),
			JSONSchema: json.RawMessage(`{"type":"object"}`),
			OnChunk:    func(s string) { chunks = append(chunks, s) },
		})
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runOnce kept waiting on a session idle after its submission")
	}
	if err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if string(res.Output) != claudeEarlyPartialSubmission {
		t.Fatalf("output = %s, want the early partial submission", res.Output)
	}
	if res.SessionID != "idle-session" {
		t.Fatalf("session ID = %q, want idle-session", res.SessionID)
	}
	last := chunks[len(chunks)-1]
	if !strings.Contains(last, "ending the session and keeping that submission") {
		t.Fatalf("last chunk = %q, want the idle-end notice", last)
	}
}

func TestClaudeAgent_IdleAfterSubmissionWithRequiredChildCompletionFails(t *testing.T) {
	withClaudeSubmittedIdleGrace(t, 200*time.Millisecond)
	t.Setenv("NM_CLAUDE_STDIN_HELPER", "submit-then-idle")
	a := newClaudeStdinHelperAgent(t)

	done := make(chan struct{})
	var res *Result
	var err error
	go func() {
		defer close(done)
		res, err = a.runOnce(context.Background(), RunOpts{
			Prompt:                   "fix the failing tests",
			CWD:                      t.TempDir(),
			JSONSchema:               json.RawMessage(`{"type":"object"}`),
			RequireCompletedChildren: true,
		})
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runOnce kept waiting on a session idle after its submission")
	}
	if err == nil {
		t.Fatalf("runOnce succeeded with output %s, want an unverified-completion error", res.Output)
	}
	if !errors.Is(err, errClaudeIdleAfterSubmission) || !strings.Contains(err.Error(), "unverified") {
		t.Fatalf("runOnce error = %v, want an unverified completion after the idle end", err)
	}
	if res != nil && len(res.Output) != 0 {
		t.Fatalf("output = %s, want none for an unverified completion", res.Output)
	}
}
