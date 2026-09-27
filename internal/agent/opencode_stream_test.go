package agent

import (
	"strings"
	"testing"
)

func TestOpencodeTokensToUsage(t *testing.T) {
	reasoning := 7
	tokens := &opencodeTokens{
		Input:     100,
		Output:    50,
		Reasoning: &reasoning,
		Cache:     &opencodeCache{Read: 20, Write: 10},
	}
	usage := opencodeTokensToUsage(tokens)
	if usage.InputTokens != 100 || usage.OutputTokens != 50 {
		t.Errorf("input/output = %d/%d, want 100/50", usage.InputTokens, usage.OutputTokens)
	}
	if usage.CacheReadTokens != 20 || usage.CacheCreationTokens != 10 || !usage.CacheCreationReported {
		t.Errorf("cache = %d/%d reported=%v, want 20/10 reported", usage.CacheReadTokens, usage.CacheCreationTokens, usage.CacheCreationReported)
	}
	if usage.ReasoningTokens != 7 || !usage.ReasoningReported {
		t.Errorf("reasoning = %d reported=%v, want 7 reported", usage.ReasoningTokens, usage.ReasoningReported)
	}
	if !usage.Reported {
		t.Error("usage should be marked reported")
	}
}

func TestOpencodeTokensToUsage_NoCacheOrReasoning(t *testing.T) {
	usage := opencodeTokensToUsage(&opencodeTokens{Input: 10, Output: 5})
	if usage.CacheCreationReported {
		t.Error("cache creation must not be reported without a cache block")
	}
	if usage.ReasoningReported {
		t.Error("reasoning must not be reported when the shape omits it")
	}
	if usage.InputTokens != 10 || usage.OutputTokens != 5 {
		t.Errorf("input/output = %d/%d, want 10/5", usage.InputTokens, usage.OutputTokens)
	}
}

func TestAccumulateUsage(t *testing.T) {
	byMsg := map[string]TokenUsage{
		"a": {InputTokens: 10, OutputTokens: 5, Reported: true},
		"b": {InputTokens: 20, OutputTokens: 15, Reported: true},
	}
	total := accumulateUsage(byMsg)
	if total.InputTokens != 30 || total.OutputTokens != 20 {
		t.Errorf("total = %d/%d, want 30/20", total.InputTokens, total.OutputTokens)
	}
}

func frames(events ...string) string {
	var b strings.Builder
	for _, ev := range events {
		b.WriteString("data: ")
		b.WriteString(strings.ReplaceAll(ev, "{SID}", "s1"))
		b.WriteString("\n\n")
	}
	return b.String()
}

func parseFrames(t *testing.T, input string) (*opencodeStreamState, []string) {
	t.Helper()
	var chunks []string
	state := newOpencodeStreamState("s1", func(text string) { chunks = append(chunks, text) })
	if err := parseOpencodeSSE(strings.NewReader(input), state); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return state, chunks
}

func TestParseOpencodeSSE_TextDeltasStreamAndEndedAddsNothing(t *testing.T) {
	state, chunks := parseFrames(t, frames(
		ocTextDelta("m1", 0, "hello "),
		ocTextDelta("m1", 0, "world"),
		ocTextEnded("m1", 0, "hello world"),
		ocSucceeded(),
	))
	if strings.Join(chunks, "|") != "hello |world" {
		t.Errorf("chunks = %q, want the two deltas and no re-emission", chunks)
	}
	if state.outputText() != "hello world" {
		t.Errorf("outputText = %q, want %q", state.outputText(), "hello world")
	}
	if state.outcome != opencodeOutcomeSucceeded {
		t.Errorf("outcome = %q, want succeeded", state.outcome)
	}
}

func TestParseOpencodeSSE_TextEndedWithoutDeltasStreamsWhole(t *testing.T) {
	_, chunks := parseFrames(t, frames(ocTextEnded("m1", 0, "streamed text"), ocSucceeded()))
	if len(chunks) != 1 || chunks[0] != "streamed text" {
		t.Errorf("chunks = %q, want the whole text once", chunks)
	}
}

func TestParseOpencodeSSE_TextEndedNonPrefixSnapshotStreamsCorrectedText(t *testing.T) {
	state, chunks := parseFrames(t, frames(
		ocTextDelta("m1", 0, "helo"),
		ocTextEnded("m1", 0, "hello"),
		ocSucceeded(),
	))
	if strings.Join(chunks, "|") != "helo|hello" {
		t.Errorf("chunks = %q, want the correction emitted whole", chunks)
	}
	if state.outputText() != "hello" {
		t.Errorf("outputText = %q, want the corrected text", state.outputText())
	}
}

func TestParseOpencodeSSE_StepEndedRecordsUsagePerAssistantMessage(t *testing.T) {
	state, _ := parseFrames(t, frames(
		ocStepEnded("m1", 10, 5),
		ocStepEnded("m2", 20, 15),
		ocStepEnded("m2", 30, 15), // a later snapshot of the same step replaces, not adds
		ocSucceeded(),
	))
	if state.usage.InputTokens != 40 || state.usage.OutputTokens != 20 {
		t.Errorf("usage = %d/%d, want 40/20", state.usage.InputTokens, state.usage.OutputTokens)
	}
	if !state.usage.Reported {
		t.Error("usage should be reported")
	}
}

func TestParseOpencodeSSE_FiltersOtherSessions(t *testing.T) {
	other := strings.ReplaceAll(ocTextEnded("m9", 0, "not ours"), "{SID}", "s2")
	otherEnd := strings.ReplaceAll(ocSucceeded(), "{SID}", "s2")
	state, chunks := parseFrames(t, "data: "+other+"\n\ndata: "+otherEnd+"\n\n"+frames(ocTextEnded("m1", 0, "ours"), ocSucceeded()))
	if len(chunks) != 1 || chunks[0] != "ours" {
		t.Errorf("chunks = %q, want only this session's text", chunks)
	}
	if state.outputText() != "ours" {
		t.Errorf("outputText = %q", state.outputText())
	}
}

func TestParseOpencodeSSE_SessionEventWithoutSessionIDIsNotOurs(t *testing.T) {
	state, _ := parseFrames(t, frames(
		ocEvent("session.execution.succeeded", `{}`),
		ocTextEnded("m1", 0, "ours"),
		ocSucceeded(),
	))
	if state.outputText() != "ours" {
		t.Error("an unattributed execution end must not close this session's turn")
	}
}

func TestParseOpencodeSSE_SeparatesTextAcrossSteps(t *testing.T) {
	_, chunks := parseFrames(t, frames(
		ocTextEnded("m1", 0, "first"),
		ocStepEnded("m1", 10, 5),
		ocToolCalled("m1"),
		ocTextEnded("m2", 0, "second"),
		ocSucceeded(),
	))
	if strings.Join(chunks, "|") != "first|\n\n|second" {
		t.Errorf("chunks = %q, want text, separator, text", chunks)
	}
}

func TestParseOpencodeSSE_DoesNotSeparateWhenToolStepPrecedesFirstText(t *testing.T) {
	_, chunks := parseFrames(t, frames(
		ocToolCalled("m1"),
		ocStepEnded("m1", 10, 5),
		ocTextDelta("m2", 0, "hello"),
		ocTextEnded("m2", 0, "hello world"),
		ocSucceeded(),
	))
	if strings.Join(chunks, "|") != "hello| world" {
		t.Errorf("chunks = %q, want no leading separator", chunks)
	}
}

func TestParseOpencodeSSE_ToolEventsMarkToolActivityAndStepEndAloneDoesNot(t *testing.T) {
	state, _ := parseFrames(t, frames(ocStepEnded("m1", 1, 1), ocSucceeded()))
	if state.toolInvoked {
		t.Fatal("a step end alone is not tool activity: every turn emits one")
	}
	for _, ev := range []string{
		ocToolCalled("m1"),
		ocEvent("session.tool.success", `{"sessionID":"{SID}","assistantMessageID":"m1","id":"t1","content":[{"type":"text","text":"ok"}],"executed":false}`),
		ocEvent("session.tool.failed", `{"sessionID":"{SID}","assistantMessageID":"m1","id":"t1","error":{"type":"tool.execution","message":"boom"},"executed":false}`),
	} {
		state, _ := parseFrames(t, frames(ev, ocSucceeded()))
		if !state.toolInvoked {
			t.Errorf("%s must count as tool activity", ev)
		}
	}
}

func TestParseOpencodeSSE_ExecutionOutcomes(t *testing.T) {
	failed, _ := parseFrames(t, frames(ocFailed(`{"type":"provider.rate-limit","message":"slow down","status":429}`)))
	if failed.outcome != opencodeOutcomeFailed || failed.failure == nil || failed.failure.Type != "provider.rate-limit" || failed.failure.Status != 429 {
		t.Errorf("failed outcome = %q failure = %+v", failed.outcome, failed.failure)
	}

	interrupted, _ := parseFrames(t, frames(ocInterrupted("shutdown")))
	if interrupted.outcome != opencodeOutcomeInterrupted || interrupted.interruptReason != "shutdown" {
		t.Errorf("interrupted outcome = %q reason = %q", interrupted.outcome, interrupted.interruptReason)
	}

	// step.failed carries the error too; execution.failed's error wins when both arrive.
	stepFailed, _ := parseFrames(t, frames(
		ocEvent("session.step.failed", `{"sessionID":"{SID}","assistantMessageID":"m1","error":{"type":"provider.auth","message":"bad key","status":401}}`),
		ocFailed(`{"type":"provider.auth","message":"bad key (execution)","status":401}`),
	))
	if stepFailed.failure == nil || stepFailed.failure.Message != "bad key (execution)" {
		t.Errorf("failure = %+v, want the execution-level error", stepFailed.failure)
	}
}

func TestParseOpencodeSSE_StopsReadingAtExecutionEnd(t *testing.T) {
	state, chunks := parseFrames(t, frames(
		ocTextEnded("m1", 0, "done"),
		ocSucceeded(),
		ocTextEnded("m2", 0, "late"),
	))
	if len(chunks) != 1 || state.outputText() != "done" {
		t.Errorf("events after the execution end must not be read: chunks=%q output=%q", chunks, state.outputText())
	}
}

func TestParseOpencodeSSE_MalformedAndEmptyEventsAreSkipped(t *testing.T) {
	input := "data: not json at all\n\ndata: \n\n" +
		"data: {\"id\":\"e\",\"type\":\"session.text.ended\",\"data\":\"not an object\"}\n\n" +
		frames(ocTextEnded("m1", 0, "ok"), ocSucceeded())
	state, _ := parseFrames(t, input)
	if state.outputText() != "ok" {
		t.Errorf("outputText = %q, want %q", state.outputText(), "ok")
	}
}

func TestParseOpencodeSSE_IgnoresNonSessionAndV1Frames(t *testing.T) {
	v1 := `{"payload":{"type":"session.idle","properties":{"sessionID":"s1"}}}`
	input := "data: " + v1 + "\n\n" +
		"data: " + ocEvent("project.updated", `{"id":"p"}`) + "\n\n" +
		frames(ocTextEnded("m1", 0, "ok"))
	state, _ := parseFrames(t, input)
	if state.outcome != "" {
		t.Errorf("a v1 idle frame must not end a v2 turn, outcome = %q", state.outcome)
	}
	if state.outputText() != "ok" {
		t.Errorf("outputText = %q", state.outputText())
	}
}

func TestParseOpencodeSSE_StreamEndWithoutExecutionEndLeavesOutcomeEmpty(t *testing.T) {
	state, _ := parseFrames(t, frames(ocTextEnded("m1", 0, "partial")))
	if state.outcome != "" {
		t.Errorf("outcome = %q, want empty for a stream that ended early", state.outcome)
	}
	if state.outputText() != "partial" {
		t.Errorf("outputText = %q", state.outputText())
	}
}
