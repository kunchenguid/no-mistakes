package agent

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Scripted failures in opencode 2.x's flat Session.StructuredError shape.
const (
	transportBlip  = `{"type":"provider.transport","message":"connection reset by upstream","status":503}`
	invalidRequest = `{"type":"provider.invalid-request","message":"rate_limit_error quoted by a 400","status":400}`
)

func TestOpencodeSessionErrorRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  *opencodeSessionError
		want bool
	}{
		{"rate limit type", &opencodeSessionError{Type: "provider.rate-limit", Status: 429}, true},
		{"transport type without a status", &opencodeSessionError{Type: "provider.transport"}, true},
		{"internal type", &opencodeSessionError{Type: "provider.internal", Status: 500}, true},
		{"timeout type", &opencodeSessionError{Type: "provider.timeout"}, true},
		// The typed classification wins over a status that would otherwise
		// read as transient.
		{"auth type with a 503", &opencodeSessionError{Type: "provider.auth", Status: 503}, false},
		{"invalid request quoting a rate limit", &opencodeSessionError{Type: "provider.invalid-request", Message: "rate_limit_error: 429 upstream", Status: 400}, false},
		{"quota exhaustion", &opencodeSessionError{Type: "provider.quota", Status: 429}, false},
		{"aborted", &opencodeSessionError{Type: "aborted"}, false},
		{"permission rejected", &opencodeSessionError{Type: "permission.rejected"}, false},
		// An unknown type falls back to the status class.
		{"unknown type with a 502", &opencodeSessionError{Type: "unknown", Status: 502}, true},
		{"unknown type with a 429", &opencodeSessionError{Type: "unknown", Status: 429}, true},
		{"unknown type with a 401", &opencodeSessionError{Type: "unknown", Status: 401}, false},
		{"unknown type without a status", &opencodeSessionError{Type: "unknown"}, false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.retryable(); got != tc.want {
				t.Errorf("retryable = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestClassifyOpencodeTransient(t *testing.T) {
	cases := []struct {
		name         string
		err          *opencodeSessionError
		toolActivity bool
		wantRetry    bool
	}{
		{name: "transient provider type", err: &opencodeSessionError{Type: "provider.transport", Status: 503}, wantRetry: true},
		{name: "rejected request", err: &opencodeSessionError{Type: "provider.invalid-request", Status: 400}, wantRetry: false},
		{name: "unknown type with a server status", err: &opencodeSessionError{Type: "unknown", Status: 502}, wantRetry: true},
		{name: "unknown type with a client status", err: &opencodeSessionError{Type: "unknown", Status: 401}, wantRetry: false},
		{
			// A retry starts a fresh session, so repeating a turn that
			// already ran tools re-executes their side effects.
			name:         "retryable but the turn already ran tools",
			err:          &opencodeSessionError{Type: "provider.transport", Status: 503},
			toolActivity: true,
			wantRetry:    false,
		},
		{
			// The terminal needles still apply to a typed failure.
			name:      "retryable type quoting an exhausted quota",
			err:       &opencodeSessionError{Type: "provider.rate-limit", Message: "insufficient_quota", Status: 429},
			wantRetry: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, retry := classifyOpencodeTransient(newOpencodeMessageFailure(tc.err, tc.toolActivity))
			if retry != tc.wantRetry {
				t.Errorf("retry = %v, want %v", retry, tc.wantRetry)
			}
		})
	}

	// Errors from outside a typed turn failure keep the shared classification.
	if _, retry := classifyOpencodeTransient(fmt.Errorf("opencode server: connection refused")); !retry {
		t.Error("expected shared transient classification to still apply")
	}
}

func TestOpencodeMessageFailure_Rendering(t *testing.T) {
	err := newOpencodeMessageFailure(&opencodeSessionError{Type: "provider.transport", Message: "reset", Status: 503}, true)
	msg := err.Error()
	for _, want := range []string{"opencode provider.transport", "status 503", "reset", "not retried: the failed turn already ran tools"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q should carry %q", msg, want)
		}
	}
	plain := newOpencodeMessageFailure(&opencodeSessionError{Type: "provider.auth", Message: "bad key"}, true).Error()
	if strings.Contains(plain, "not retried") {
		t.Errorf("a failure that was never retryable must not claim a withheld retry: %q", plain)
	}
	if newOpencodeMessageFailure(nil, false) != nil {
		t.Error("a nil error is no failure")
	}
}

// TestOpencodeAgent_FailedTurnSurfacesTheProviderError pins that a turn
// opencode ends with session.execution.failed reports the typed cause, not
// the undiagnosable "returned no text output".
func TestOpencodeAgent_FailedTurnSurfacesTheProviderError(t *testing.T) {
	f := newFakeOpencode(t, fakeOpencodeTurn{
		events:   []string{ocFailed(invalidRequest)},
		messages: ocMessages().user("p").assistantError("m1", invalidRequest).idle("failed").newestFirst(),
	})
	_, err := runFakeOpencode(t, f)
	if err == nil {
		t.Fatal("expected the failed turn to fail")
	}
	for _, want := range []string{"provider.invalid-request", "400", "rate_limit_error quoted by a 400"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should carry %q", err, want)
		}
	}
	if f.sessionCount() != 1 {
		t.Errorf("sessions = %d, want no retry of a rejected request", f.sessionCount())
	}
}

func TestOpencodeAgent_RetriesRetryableProviderErrorThenSucceeds(t *testing.T) {
	defer withFastBackoff(t)()
	f := newFakeOpencode(t,
		fakeOpencodeTurn{events: []string{ocStepEnded("m1", 1, 0), ocFailed(transportBlip)}},
		fakeOpencodeTurn{
			events:   ocTextTurn("m2", `{"summary":"all good"}`, 10, 5),
			messages: ocMessages().user("p").assistantText("m2", `{"summary":"all good"}`, 10, 5).idle("succeeded").newestFirst(),
		},
	)
	result, err := runFakeOpencode(t, f)
	if err != nil {
		t.Fatalf("expected the retry to succeed, got %v", err)
	}
	if result == nil || string(result.Output) != `{"summary":"all good"}` {
		t.Fatalf("result = %+v", result)
	}
	if f.sessionCount() != 2 {
		t.Errorf("sessions = %d, want exactly one retry in a fresh session", f.sessionCount())
	}
}

// TestOpencodeAgent_RetryableFailureAfterToolActivityIsNotRetried is the
// regression for the retry replaying side effects: runOnce creates a fresh
// session per attempt, so a retry replays the whole prompt with no memory of
// the tools the failed attempt already executed. When the failed turn ran
// any tool the retry is refused and the provider error is reported as-is.
func TestOpencodeAgent_RetryableFailureAfterToolActivityIsNotRetried(t *testing.T) {
	defer withFastBackoff(t)()
	f := newFakeOpencode(t,
		fakeOpencodeTurn{events: []string{ocToolCalled("m1"), ocFailed(transportBlip)}},
		fakeOpencodeTurn{events: ocTextTurn("m2", `{"summary":"all good"}`, 1, 1)},
	)
	result, err := runFakeOpencode(t, f)
	if err == nil {
		t.Fatalf("expected the failed turn to fail closed, got result %+v", result)
	}
	if f.sessionCount() != 1 {
		t.Fatalf("sessions = %d, want no replay after tool activity", f.sessionCount())
	}
	msg := err.Error()
	for _, want := range []string{"provider.transport", "503", "connection reset by upstream", "already ran tools"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q should carry %q", msg, want)
		}
	}
}

// TestOpencodeAgent_RetryableFailureWithoutToolActivityStillRetries pins the
// other side of the gate: a provider blip that kills the turn before any tool
// runs still costs only a retry. A step end alone is not tool activity.
func TestOpencodeAgent_RetryableFailureWithoutToolActivityStillRetries(t *testing.T) {
	defer withFastBackoff(t)()
	f := newFakeOpencode(t,
		fakeOpencodeTurn{events: []string{ocStepEnded("m1", 1, 0), ocFailed(transportBlip)}},
		fakeOpencodeTurn{
			events:   ocTextTurn("m2", `{"summary":"all good"}`, 1, 1),
			messages: ocMessages().user("p").assistantText("m2", `{"summary":"all good"}`, 1, 1).idle("succeeded").newestFirst(),
		},
	)
	if _, err := runFakeOpencode(t, f); err != nil {
		t.Fatalf("expected retry to succeed, got %v", err)
	}
	if f.sessionCount() != 2 {
		t.Errorf("sessions = %d, want exactly one retry", f.sessionCount())
	}
}

// TestOpencodeAgent_ToolContentInTheMessageListAlsoBlocksRetry covers tool
// activity that shows only in the record: the stream can miss the event, but
// the assistant message that ran the tool lists it.
func TestOpencodeAgent_ToolContentInTheMessageListAlsoBlocksRetry(t *testing.T) {
	defer withFastBackoff(t)()
	f := newFakeOpencode(t,
		fakeOpencodeTurn{
			events:   []string{ocFailed(transportBlip)},
			messages: ocMessages().user("p").assistantTool("m1").idle("failed").newestFirst(),
		},
		fakeOpencodeTurn{events: ocTextTurn("m2", `{"summary":"all good"}`, 1, 1)},
	)
	if _, err := runFakeOpencode(t, f); err == nil {
		t.Fatal("expected the failed turn to fail closed")
	}
	if f.sessionCount() != 1 {
		t.Errorf("sessions = %d, want no replay after tool activity", f.sessionCount())
	}
}

// TestOpencodeAgent_DroppedStreamAfterToolActivityIsNotRetried applies the
// gate to the stream failure path: the "unexpected EOF" the shared classifier
// would retry is refused once a tool event has crossed the stream.
func TestOpencodeAgent_DroppedStreamAfterToolActivityIsNotRetried(t *testing.T) {
	defer withFastBackoff(t)()
	defer withFastEvidenceWait(t)()
	f := newFakeOpencode(t,
		fakeOpencodeTurn{events: []string{ocToolCalled("m1")}, dropStream: true},
		fakeOpencodeTurn{events: ocTextTurn("m2", `{"summary":"all good"}`, 1, 1)},
	)
	_, err := runFakeOpencode(t, f)
	if err == nil {
		t.Fatal("expected the dropped stream to fail closed")
	}
	if f.sessionCount() != 1 {
		t.Errorf("sessions = %d, want no replay after tool activity", f.sessionCount())
	}
	if !strings.Contains(err.Error(), "opencode events:") || !strings.Contains(err.Error(), "already ran tools") {
		t.Errorf("error = %q", err)
	}
	if f.calledMatching("POST", "/api/session/ses_1/interrupt") == 0 {
		t.Error("a turn abandoned by its stream must be interrupted")
	}
}

// TestOpencodeAgent_DroppedStreamWithASettledRecordStillRetries is the blip
// the retry exists for: the stream died before any tool ran, the session
// settled to idle, and its record proves the turn ran nothing.
func TestOpencodeAgent_DroppedStreamWithASettledRecordStillRetries(t *testing.T) {
	defer withFastBackoff(t)()
	defer withFastEvidenceWait(t)()
	f := newFakeOpencode(t,
		fakeOpencodeTurn{
			events:     []string{ocStepEnded("m1", 1, 0)},
			dropStream: true,
			messages:   ocMessages().user("p").idle("interrupted").newestFirst(),
		},
		fakeOpencodeTurn{
			events:   ocTextTurn("m2", `{"summary":"all good"}`, 1, 1),
			messages: ocMessages().user("p").assistantText("m2", `{"summary":"all good"}`, 1, 1).idle("succeeded").newestFirst(),
		},
	)
	result, err := runFakeOpencode(t, f)
	if err != nil {
		t.Fatalf("expected the retry to succeed, got %v", err)
	}
	if result == nil || result.Output == nil {
		t.Fatalf("expected structured output, got %+v", result)
	}
	if f.sessionCount() != 2 {
		t.Errorf("sessions = %d, want exactly one retry", f.sessionCount())
	}
}

// TestOpencodeAgent_DroppedStreamWithAnUnsettledRecordWithholdsTheRetry is
// the window the gate exists for: the stream died carrying no tool event and
// the session never settled, so a tool can have run unrecorded. That turn is
// unverifiable and is not replayed.
func TestOpencodeAgent_DroppedStreamWithAnUnsettledRecordWithholdsTheRetry(t *testing.T) {
	defer withFastBackoff(t)()
	defer withFastEvidenceWait(t)()
	f := newFakeOpencode(t,
		fakeOpencodeTurn{
			events:     []string{ocStepEnded("m1", 1, 0)},
			dropStream: true,
			waitStatus: http.StatusServiceUnavailable,
			messages:   ocMessages().user("p").newestFirst(), // admitted, never idle
		},
		fakeOpencodeTurn{events: ocTextTurn("m2", `{"summary":"all good"}`, 1, 1)},
	)
	result, err := runFakeOpencode(t, f)
	if err == nil {
		t.Fatalf("expected the unverifiable turn to fail closed, got result %+v", result)
	}
	if f.sessionCount() != 1 {
		t.Fatalf("sessions = %d, want no replay while the turn is unverified", f.sessionCount())
	}
	if !strings.Contains(err.Error(), "not retried: could not verify the failed turn ran no tools") {
		t.Errorf("error = %q, want the unverified turn named", err)
	}
}

// TestOpencodeAgent_DroppedStreamWithAnUnreadableRecordWithholdsTheRetry: a
// server that is gone answers nothing, and nothing is not a proof.
func TestOpencodeAgent_DroppedStreamWithAnUnreadableRecordWithholdsTheRetry(t *testing.T) {
	defer withFastBackoff(t)()
	defer withFastEvidenceWait(t)()
	f := newFakeOpencode(t,
		fakeOpencodeTurn{events: []string{ocStepEnded("m1", 1, 0)}, dropStream: true, messagesStatus: http.StatusBadGateway},
		fakeOpencodeTurn{events: ocTextTurn("m2", `{"summary":"all good"}`, 1, 1)},
	)
	if _, err := runFakeOpencode(t, f); err == nil {
		t.Fatal("expected the unverifiable turn to fail closed")
	}
	if f.sessionCount() != 1 {
		t.Errorf("sessions = %d, want no replay while the turn is unverified", f.sessionCount())
	}
}

// TestOpencodeAgent_RejectedPromptWithNothingAdmittedStillRetries: a prompt
// the server refused never entered the session (its record holds no user
// message), so the transient it quoted keeps its retry.
func TestOpencodeAgent_RejectedPromptWithNothingAdmittedStillRetries(t *testing.T) {
	defer withFastBackoff(t)()
	defer withFastEvidenceWait(t)()
	f := newFakeOpencode(t,
		fakeOpencodeTurn{promptStatus: http.StatusServiceUnavailable, messages: "[]"},
		fakeOpencodeTurn{
			events:   ocTextTurn("m2", `{"summary":"all good"}`, 1, 1),
			messages: ocMessages().user("p").assistantText("m2", `{"summary":"all good"}`, 1, 1).idle("succeeded").newestFirst(),
		},
	)
	if _, err := runFakeOpencode(t, f); err != nil {
		t.Fatalf("expected the retry to succeed, got %v", err)
	}
	if f.sessionCount() != 2 {
		t.Errorf("sessions = %d, want exactly one retry", f.sessionCount())
	}
}

func TestOpencodeTurnFailure_GatesTheSharedClassifier(t *testing.T) {
	streamDrop := fmt.Errorf("opencode events: unexpected EOF")

	if _, retry := classifyOpencodeTransient(opencodeTurnFailure(opencodeToolsRan, streamDrop)); retry {
		t.Error("a dropped stream after tool activity must not be retried")
	}
	if msg := opencodeTurnFailure(opencodeToolsRan, streamDrop).Error(); !strings.Contains(msg, "not retried: the failed turn already ran tools") {
		t.Errorf("expected the withheld retry to be named, got %q", msg)
	}

	// The blip the retry exists for is untouched by the gate.
	if _, retry := classifyOpencodeTransient(opencodeTurnFailure(opencodeToolsNone, streamDrop)); !retry {
		t.Error("a dropped stream proven to be before any tool ran must still be retried")
	}

	// An unverified turn is refused like one known to have run tools, and
	// says which of the two it was.
	unverified := opencodeTurnFailure(opencodeToolsUnknown, streamDrop)
	if _, retry := classifyOpencodeTransient(unverified); retry {
		t.Error("a turn whose tool activity is unknown must not be retried")
	}
	if msg := unverified.Error(); !strings.Contains(msg, "not retried: could not verify the failed turn ran no tools") {
		t.Errorf("expected the unverified turn to be named as such, got %q", msg)
	}

	// A failure that was never retryable keeps its wording, so the suffix
	// stays a reliable signal that a retry was withheld.
	parseFailure := fmt.Errorf("opencode output parse: invalid character 'N'")
	if msg := opencodeTurnFailure(opencodeToolsRan, parseFailure).Error(); msg != parseFailure.Error() {
		t.Errorf("expected a non-retryable failure to render unchanged, got %q", msg)
	}
}

// TestResolveOpencodeToolEvidence is the three-valued question itself: a turn
// that ran a tool, a turn PROVEN to have run none, and a turn nothing
// available can answer for.
func TestResolveOpencodeToolEvidence(t *testing.T) {
	fetched := func(list string) opencodeTurnRecord {
		var wire opencodeMessageList
		if err := jsonUnmarshalString(`{"data":`+list+`}`, &wire); err != nil {
			t.Fatalf("fixture: %v", err)
		}
		sortOpencodeMessages(wire.Data)
		return opencodeTurnRecord{messages: wire.Data, fetched: true}
	}

	cases := []struct {
		name           string
		state          *opencodeStreamState
		record         opencodeTurnRecord
		streamComplete bool
		want           opencodeToolEvidence
	}{
		{
			name:  "tool event on the stream",
			state: &opencodeStreamState{toolInvoked: true},
			want:  opencodeToolsRan,
		},
		{
			name:   "tool content only in the record",
			state:  &opencodeStreamState{},
			record: fetched(ocMessages().user("p").assistantTool("m1").newestFirst()),
			want:   opencodeToolsRan,
		},
		{
			// The record reached idle, so it lists every step and can
			// prove the negative the stream no longer can.
			name:   "settled record without a tool",
			state:  &opencodeStreamState{},
			record: fetched(ocMessages().user("p").assistantText("m1", "done", 1, 1).idle("succeeded").newestFirst()),
			want:   opencodeToolsNone,
		},
		{
			// Nothing was admitted, so nothing ran.
			name:   "record with no user message",
			state:  &opencodeStreamState{},
			record: fetched("[]"),
			want:   opencodeToolsNone,
		},
		{
			// Admitted but never settled: a tool can have run unrecorded.
			name:   "unsettled record",
			state:  &opencodeStreamState{},
			record: fetched(ocMessages().user("p").assistantText("m1", "working", 1, 1).newestFirst()),
			want:   opencodeToolsUnknown,
		},
		{
			// Every tool event of the session crossed a stream that saw the
			// execution end, so an unreadable record adds nothing.
			name:           "complete stream and an unreadable record",
			state:          &opencodeStreamState{},
			record:         opencodeTurnRecord{err: fmt.Errorf("502")},
			streamComplete: true,
			want:           opencodeToolsNone,
		},
		{
			name:   "dead stream and an unreadable record",
			state:  &opencodeStreamState{},
			record: opencodeTurnRecord{err: fmt.Errorf("connection refused")},
			want:   opencodeToolsUnknown,
		},
		{
			// An earlier idle belongs to an earlier turn; only one after the
			// last prompt settles this turn.
			name:   "idle before the prompt does not settle it",
			state:  &opencodeStreamState{},
			record: fetched(ocMessages().idle("succeeded").user("p").newestFirst()),
			want:   opencodeToolsUnknown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveOpencodeToolEvidence(tc.state, tc.record, tc.streamComplete); got != tc.want {
				t.Errorf("evidence = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRequestNeverReachedOpencode(t *testing.T) {
	dial := &url.Error{Op: "Post", URL: "http://127.0.0.1:1/api/session/s1/prompt",
		Err: &net.OpError{Op: "dial", Err: fmt.Errorf("connect: connection refused")}}
	read := &url.Error{Op: "Post", URL: "http://127.0.0.1:1/api/session/s1/prompt",
		Err: &net.OpError{Op: "read", Err: fmt.Errorf("connection reset by peer")}}
	if !requestNeverReachedOpencode(dial) {
		t.Error("a dial failure proves the prompt never arrived")
	}
	if requestNeverReachedOpencode(read) {
		t.Error("a failure after connecting means opencode may hold the prompt")
	}
	if requestNeverReachedOpencode(nil) {
		t.Error("nil is not a failure")
	}
}

func TestOpencodeTurnRecord_OutputTextAndUsage(t *testing.T) {
	var wire opencodeMessageList
	list := ocMessages().
		assistantText("old", "previous turn", 100, 100).idle("succeeded").
		user("p").
		assistantText("m1", "step one", 1, 2).
		assistantTool("m1b").
		assistantText("m2", "  ", 0, 0).
		assistantText("m3", "final", 3, 4).
		idle("succeeded").newestFirst()
	if err := jsonUnmarshalString(`{"data":`+list+`}`, &wire); err != nil {
		t.Fatal(err)
	}
	sortOpencodeMessages(wire.Data)
	record := opencodeTurnRecord{messages: wire.Data, fetched: true}

	if got := record.outputText(); got != "step one\n\nfinal" {
		t.Errorf("outputText = %q, want only this turn's non-blank text joined", got)
	}
	usage, ok := record.usage()
	if !ok {
		t.Fatal("usage should be reported")
	}
	// 1+2 (m1) + 2+9 (tool) + 0+0 (m2) + 3+4 (m3); the previous turn is excluded.
	if usage.InputTokens != 6 || usage.OutputTokens != 15 {
		t.Errorf("usage = %d/%d", usage.InputTokens, usage.OutputTokens)
	}
	if !record.complete() || !record.admitted() || !record.toolRan() {
		t.Error("record should be complete, admitted and show the tool")
	}
	if record.assistantError() != nil {
		t.Error("no assistant error recorded")
	}
	unfetched := opencodeTurnRecord{err: fmt.Errorf("x")}
	if unfetched.outputText() != "" || unfetched.complete() || unfetched.admitted() || unfetched.toolRan() {
		t.Error("an unfetched record proves nothing")
	}
	if _, ok := unfetched.usage(); ok {
		t.Error("an unfetched record reports no usage")
	}
}
