package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// opencodeErrorServer serves the minimal session lifecycle with a
// caller-supplied POST /session/{id}/message body, and reports how many
// message requests it answered. The SSE stream carries no text, which is
// what real opencode emits for a turn that failed before the model replied.
func opencodeErrorServer(t *testing.T, bodies ...string) (*httptest.Server, *int) {
	t.Helper()
	return opencodeErrorServerWithEvents(t, "", bodies...)
}

// opencodeErrorServerWithEvents is opencodeErrorServer with caller-supplied
// SSE events replayed before session.idle, so a test can put the tool
// activity that a failed turn left behind on the stream.
func opencodeErrorServerWithEvents(t *testing.T, events string, bodies ...string) (*httptest.Server, *int) {
	t.Helper()
	sent := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"s1"}`)

		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if events != "" {
				fmt.Fprint(w, events)
			}
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")

		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			body := bodies[len(bodies)-1]
			if sent < len(bodies) {
				body = bodies[sent]
			}
			sent++
			fmt.Fprint(w, body)

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(server.Close)
	return server, &sent
}

func runOpencodeAgainst(t *testing.T, server *httptest.Server) (*Result, error) {
	t.Helper()
	a := &opencodeAgent{
		bin:    "opencode",
		server: &managedServer{port: mustParsePort(server.URL)},
	}
	return a.Run(context.Background(), RunOpts{
		Prompt:     "review this code",
		CWD:        t.TempDir(),
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`),
	})
}

// providerRejectedBody is the response real opencode returns when the
// provider rejects the forced tool_choice that a json_schema request
// carries: HTTP 200, no parts, zero tokens, and the whole cause nested
// under info.error.data.
const providerRejectedBody = `{"info":{"id":"msg1","role":"assistant","tokens":{"input":0,"output":0},` +
	`"error":{"name":"APIError","data":{"message":"Error from provider (Console Go): Upstream request failed: ` +
	`[invalid_request_error] Thinking mode does not support this tool_choice","statusCode":400,"isRetryable":false}}},"parts":[]}`

// TestOpencodeAgent_FailedTurnSurfacesProviderErrorInsteadOfEmptyOutput is
// the regression for a review step that failed in seconds with zero tokens
// and reported only "opencode returned no text output". opencode reports a
// failed turn on info.error with HTTP 200, so every variant other than
// StructuredOutputError used to be dropped and the run fell through to the
// empty-text fallback, hiding an actionable provider rejection.
func TestOpencodeAgent_FailedTurnSurfacesProviderErrorInsteadOfEmptyOutput(t *testing.T) {
	server, sent := opencodeErrorServer(t, providerRejectedBody)

	result, err := runOpencodeAgainst(t, server)
	if err == nil {
		t.Fatalf("expected error, got result %+v", result)
	}
	if result != nil {
		t.Fatalf("expected nil result on error, got %+v", result)
	}
	msg := err.Error()
	if strings.Contains(msg, "no text output") {
		t.Errorf("failed turn must not be reported as empty output, got %q", msg)
	}
	for _, want := range []string{"APIError", "400", "Thinking mode does not support this tool_choice"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error to carry %q, got %q", want, msg)
		}
	}
	// A provider that rejected the request as invalid will reject it again,
	// so the step must fail on the first attempt rather than spending the
	// retry budget.
	if *sent != 1 {
		t.Errorf("expected 1 message request for a non-retryable error, got %d", *sent)
	}
}

// TestOpencodeAgent_StructuredOutputErrorReadsNestedErrorData pins the wire
// shape: opencode serializes every named error as {"name":..,"data":{..}},
// so reading message/retries from the top level left the user-facing error
// blank ("failed after 0 internal retries: ").
func TestOpencodeAgent_StructuredOutputErrorReadsNestedErrorData(t *testing.T) {
	server, _ := opencodeErrorServer(t, `{"info":{"id":"msg1","role":"assistant","error":{"name":"StructuredOutputError",`+
		`"data":{"message":"Model did not produce structured output","retries":2}}},"parts":[]}`)

	_, err := runOpencodeAgainst(t, server)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "2 internal retries") {
		t.Errorf("expected nested retry count in error, got %q", msg)
	}
	if !strings.Contains(msg, "Model did not produce structured output") {
		t.Errorf("expected nested message in error, got %q", msg)
	}
}

// TestOpencodeAgent_RetriesRetryableProviderErrorThenSucceeds covers the
// other half: a provider blip opencode itself marks retryable costs a retry,
// not the whole pipeline round.
func TestOpencodeAgent_RetriesRetryableProviderErrorThenSucceeds(t *testing.T) {
	defer withFastBackoff(t)()

	server, sent := opencodeErrorServer(t,
		`{"info":{"id":"msg1","role":"assistant","error":{"name":"APIError","data":{"message":"Service Unavailable","statusCode":503,"isRetryable":true}}},"parts":[]}`,
		`{"info":{"id":"msg2","role":"assistant","structured":{"summary":"all good"},"tokens":{"input":10,"output":5}},"parts":[{"type":"text","text":"{\"summary\":\"all good\"}"}]}`,
	)

	result, err := runOpencodeAgainst(t, server)
	if err != nil {
		t.Fatalf("expected retry to succeed, got %v", err)
	}
	if result == nil || result.Output == nil {
		t.Fatalf("expected structured output, got %+v", result)
	}
	if *sent != 2 {
		t.Errorf("expected exactly one retry, got %d message requests", *sent)
	}
}

func TestClassifyOpencodeTransient(t *testing.T) {
	retryable := true
	notRetryable := false

	cases := []struct {
		name         string
		err          *opencodeMessageError
		toolActivity bool
		wantRetry    bool
	}{
		{
			name:      "provider marks the call retryable",
			err:       &opencodeMessageError{Name: "APIError", Data: &opencodeMessageErrorData{StatusCode: 503, IsRetryable: &retryable}},
			wantRetry: true,
		},
		{
			name:      "provider marks the call non-retryable",
			err:       &opencodeMessageError{Name: "APIError", Data: &opencodeMessageErrorData{StatusCode: 400, IsRetryable: &notRetryable}},
			wantRetry: false,
		},
		{
			// The flag wins over the status text: a rejection quoting the
			// provider's own rate-limit prose must not read as transient.
			name: "non-retryable body quoting a rate limit",
			err: &opencodeMessageError{Name: "APIError", Data: &opencodeMessageErrorData{
				Message: "rate_limit_error: upstream said 429", StatusCode: 400, IsRetryable: &notRetryable,
			}},
			wantRetry: false,
		},
		{
			name: "retryable quota exhaustion",
			err: &opencodeMessageError{Name: "APIError", Data: &opencodeMessageErrorData{
				Message: "insufficient_quota", StatusCode: 429, IsRetryable: &retryable,
			}},
			wantRetry: false,
		},
		{
			name:      "no flag falls back to the status class",
			err:       &opencodeMessageError{Name: "APIError", Data: &opencodeMessageErrorData{StatusCode: 502}},
			wantRetry: true,
		},
		{
			name:      "no flag and a client status",
			err:       &opencodeMessageError{Name: "APIError", Data: &opencodeMessageErrorData{StatusCode: 401}},
			wantRetry: false,
		},
		{
			// opencode already spent its internal retries on this one.
			name:      "structured output error",
			err:       &opencodeMessageError{Name: "StructuredOutputError", Data: &opencodeMessageErrorData{Retries: new(int)}},
			wantRetry: false,
		},
		{
			// A retry starts a fresh session, so repeating a turn that
			// already ran tools re-executes their side effects.
			name:         "retryable but the turn already ran tools",
			err:          &opencodeMessageError{Name: "APIError", Data: &opencodeMessageErrorData{StatusCode: 503, IsRetryable: &retryable}},
			toolActivity: true,
			wantRetry:    false,
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

	// Errors from outside a message response keep the shared classification.
	if _, retry := classifyOpencodeTransient(fmt.Errorf("opencode server: connection refused")); !retry {
		t.Error("expected shared transient classification to still apply")
	}
}

// toolPartEvent is a tool part on the SSE stream: opencode reports every tool
// invocation as a part of type "tool" on the assistant message.
const toolPartEvent = `data: {"payload":{"type":"message.part.updated","properties":{"sessionID":"s1",` +
	`"part":{"id":"prt1","messageID":"msg1","type":"tool"}}}}` + "\n\n"

// stepFinishEvent ends a model step. Every turn emits one, including a turn
// that only produced text, so it must never by itself count as tool activity.
const stepFinishEvent = `data: {"payload":{"type":"message.part.updated","properties":{"sessionID":"s1",` +
	`"part":{"id":"step1","messageID":"msg1","type":"step-finish","tokens":{"input":10,"output":5}}}}}` + "\n\n"

// retryableProviderBlipBody is a failure opencode itself marks retryable.
const retryableProviderBlipBody = `{"info":{"id":"msg1","role":"assistant","error":{"name":"APIError",` +
	`"data":{"message":"Service Unavailable","statusCode":503,"isRetryable":true}}},"parts":[]}`

const structuredSuccessBody = `{"info":{"id":"msg2","role":"assistant","structured":{"summary":"all good"},` +
	`"tokens":{"input":10,"output":5}},"parts":[{"type":"text","text":"{\"summary\":\"all good\"}"}]}`

// TestOpencodeAgent_RetryableFailureAfterToolActivityIsNotRetried is the
// regression for the retry replaying side effects. runOnce creates a fresh
// session per attempt, so a retry replays the whole prompt with no memory of
// the tools the failed attempt already executed - a second branch push, a
// second file write, a second PR comment. When the failed turn ran any tool
// the retry is refused and the provider error is reported as-is.
func TestOpencodeAgent_RetryableFailureAfterToolActivityIsNotRetried(t *testing.T) {
	defer withFastBackoff(t)()

	server, sent := opencodeErrorServerWithEvents(t, toolPartEvent,
		retryableProviderBlipBody,
		structuredSuccessBody,
	)

	result, err := runOpencodeAgainst(t, server)
	if err == nil {
		t.Fatalf("expected the failed turn to fail closed, got result %+v", result)
	}
	if *sent != 1 {
		t.Fatalf("expected 1 message request after tool activity, got %d", *sent)
	}
	msg := err.Error()
	// The provider's own cause must survive the refusal.
	for _, want := range []string{"APIError", "503", "Service Unavailable"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error to carry %q, got %q", want, msg)
		}
	}
	if !strings.Contains(msg, "already ran tools") {
		t.Errorf("expected the error to explain why it was not retried, got %q", msg)
	}
}

// TestOpencodeAgent_RetryableFailureWithoutToolActivityStillRetries pins the
// other side of the gate: the motivating failure mode is a provider blip that
// kills the turn before any tool runs, and that must still cost only a retry.
// A step-finish part alone is not tool activity - every turn emits one.
func TestOpencodeAgent_RetryableFailureWithoutToolActivityStillRetries(t *testing.T) {
	defer withFastBackoff(t)()

	server, sent := opencodeErrorServerWithEvents(t, stepFinishEvent,
		retryableProviderBlipBody,
		structuredSuccessBody,
	)

	result, err := runOpencodeAgainst(t, server)
	if err != nil {
		t.Fatalf("expected retry to succeed, got %v", err)
	}
	if result == nil || result.Output == nil {
		t.Fatalf("expected structured output, got %+v", result)
	}
	if *sent != 2 {
		t.Errorf("expected exactly one retry, got %d message requests", *sent)
	}
}

// TestOpencodeAgent_ToolPartInTheMessageResponseAlsoBlocksRetry covers the
// tool part that arrives only on the message response body: the SSE stream
// can be cut short, so the response is the second place tool activity shows.
func TestOpencodeAgent_ToolPartInTheMessageResponseAlsoBlocksRetry(t *testing.T) {
	defer withFastBackoff(t)()

	server, sent := opencodeErrorServer(t,
		`{"info":{"id":"msg1","role":"assistant","error":{"name":"APIError",`+
			`"data":{"message":"Service Unavailable","statusCode":503,"isRetryable":true}}},`+
			`"parts":[{"type":"tool"}]}`,
		structuredSuccessBody,
	)

	if _, err := runOpencodeAgainst(t, server); err == nil {
		t.Fatal("expected the failed turn to fail closed")
	}
	if *sent != 1 {
		t.Errorf("expected 1 message request after tool activity, got %d", *sent)
	}
}

// TestOpencodeAgent_ThinkingConflictAfterToolActivityDoesNotFallBack applies
// the same gate to the prompt-only structured-output fallback. That fallback
// is also a second attempt in a fresh session, and a session.error can arrive
// at any point in a turn, so a conflict reported after the model already ran
// a tool would replay that tool's side effects.
func TestOpencodeAgent_ThinkingConflictAfterToolActivityDoesNotFallBack(t *testing.T) {
	var sessions atomic.Int32
	var eventStreams atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			id := sessions.Add(1)
			fmt.Fprintf(w, `{"id":"s%d"}`, id)
		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			if eventStreams.Add(1) == 1 {
				fmt.Fprint(w, toolPartEvent)
				fmt.Fprint(w, `data: {"payload":{"type":"session.error","properties":{"sessionID":"s1","error":{"name":"APIError","data":{"message":"tool_choice 'required' is incompatible with thinking enabled"}}}}}`+"\n\n")
				return
			}
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")
		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant"}}`)
		case r.URL.Path == "/session/s2/message" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"info":{"id":"msg2","role":"assistant"},"parts":[{"type":"text","text":"{\"summary\":\"fallback ran anyway\"}"}]}`)
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{bin: "opencode", server: &managedServer{port: mustParsePort(server.URL)}}
	result, err := a.Run(context.Background(), RunOpts{
		Prompt:     "review the changes",
		CWD:        t.TempDir(),
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`),
	})
	if err == nil {
		t.Fatalf("expected the conflict to fail closed, got result %+v", result)
	}
	if got := sessions.Load(); got != 1 {
		t.Fatalf("sessions = %d, want no fallback session after tool activity", got)
	}
	if !strings.Contains(err.Error(), "already ran tools") {
		t.Errorf("expected the error to explain the withheld fallback, got %q", err)
	}
}

// killResponseStream ends an in-flight response the way a dropped connection
// does: the chunked body stops without its terminating chunk, so the client's
// next read fails with an unexpected EOF rather than a clean end of stream.
func killResponseStream(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	flusher, ok := w.(http.Flusher)
	if !ok {
		t.Fatal("response writer does not support flushing")
	}
	flusher.Flush()
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		t.Fatal("response writer does not support hijacking")
	}
	conn, _, err := hijacker.Hijack()
	if err != nil {
		t.Fatalf("hijack: %v", err)
	}
	conn.Close()
}

// opencodeStreamDeathServer serves a first turn whose SSE stream dies
// mid-flight after replaying events, and a healthy turn on every later
// attempt. It reports how many sessions were created, which is the fact the
// gate is about: a retry is a fresh session that replays the whole prompt.
func opencodeStreamDeathServer(t *testing.T, events string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var sessions, streams atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprintf(w, `{"id":"s%d"}`, sessions.Add(1))

		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if streams.Add(1) == 1 {
				fmt.Fprint(w, events)
				killResponseStream(t, w)
				return
			}
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")

		case strings.HasSuffix(r.URL.Path, "/message") && r.Method == http.MethodPost:
			fmt.Fprint(w, structuredSuccessBody)

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(server.Close)
	return server, &sessions
}

// TestOpencodeAgent_SSEFailureAfterToolActivityIsNotRetried is the same
// replay hole on the path that never reaches opencode's own retryability
// verdict. A dropped event stream is reported as "opencode events:
// unexpected EOF", which the shared substring classifier reads as transient
// and retries in a FRESH session - replaying every tool the failed turn
// already ran. The tool part arrives on the stream before it dies, so the
// retry is refused.
func TestOpencodeAgent_SSEFailureAfterToolActivityIsNotRetried(t *testing.T) {
	defer withFastBackoff(t)()

	server, sessions := opencodeStreamDeathServer(t, toolPartEvent)

	result, err := runOpencodeAgainst(t, server)
	if err == nil {
		t.Fatalf("expected the dropped stream to fail closed, got result %+v", result)
	}
	if got := sessions.Load(); got != 1 {
		t.Fatalf("sessions = %d, want no replay session after tool activity", got)
	}
	msg := err.Error()
	if !strings.Contains(msg, "opencode events:") {
		t.Errorf("expected the stream failure to survive the refusal, got %q", msg)
	}
	if !strings.Contains(msg, "not retried: the failed turn already ran tools") {
		t.Errorf("expected the error to explain why it was not retried, got %q", msg)
	}
}

// TestOpencodeAgent_SSEFailureWithoutToolActivityStillRetries pins the other
// side: a stream that drops before the model acted is exactly the blip the
// retry exists for, and still costs one retry rather than the whole round.
func TestOpencodeAgent_SSEFailureWithoutToolActivityStillRetries(t *testing.T) {
	defer withFastBackoff(t)()

	server, sessions := opencodeStreamDeathServer(t, stepFinishEvent)

	result, err := runOpencodeAgainst(t, server)
	if err != nil {
		t.Fatalf("expected retry to succeed, got %v", err)
	}
	if result == nil || result.Output == nil {
		t.Fatalf("expected structured output, got %+v", result)
	}
	if got := sessions.Load(); got != 2 {
		t.Errorf("sessions = %d, want exactly one retry session", got)
	}
}

// TestOpencodeTurnFailure_GatesTheSharedClassifier covers the paths that
// carry no verdict from opencode. The classifier sees only text there, so
// the marker is what stands between a dropped stream and a replayed tool.
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

	// The thinking-conflict trigger reaches the retry loop by the same route,
	// and the prompt-only fallback reads the same markers.
	for _, evidence := range []opencodeToolEvidence{opencodeToolsRan, opencodeToolsUnknown} {
		conflict := thinkingConflict(evidence, fmt.Errorf("connection reset by peer"))
		if _, retry := classifyOpencodeTransient(conflict); retry {
			t.Errorf("a thinking conflict with evidence %v must not be retried", evidence)
		}
		if !opencodeReplayUnsafe(conflict) {
			t.Errorf("a thinking conflict with evidence %v must block the fresh-session fallback", evidence)
		}
	}
	if opencodeReplayUnsafe(thinkingConflict(opencodeToolsNone, nil)) {
		t.Error("a conflict proven to be before any tool ran must still reach the fallback")
	}
}

// TestResolveOpencodeToolEvidence is the three-valued question itself: a turn
// that ran a tool, a turn PROVEN to have run none, and a turn nothing
// available can answer for. The last one used to be read as the second.
func TestResolveOpencodeToolEvidence(t *testing.T) {
	respWithTool := &opencodeMessageResponse{Parts: []opencodeMessagePart{{Type: "tool"}}}
	respWithoutTool := &opencodeMessageResponse{Parts: []opencodeMessagePart{{Type: "text", Text: "done"}}}
	dialFailure := &url.Error{Op: "Post", URL: "http://127.0.0.1:1/session/s1/message",
		Err: &net.OpError{Op: "dial", Err: fmt.Errorf("connect: connection refused")}}
	midRequestFailure := &url.Error{Op: "Post", URL: "http://127.0.0.1:1/session/s1/message",
		Err: &net.OpError{Op: "read", Err: fmt.Errorf("connection reset by peer")}}

	cases := []struct {
		name           string
		state          *opencodeStreamState
		mr             opencodeMessageResult
		streamComplete bool
		want           opencodeToolEvidence
	}{
		{
			name:  "tool part on the stream",
			state: &opencodeStreamState{toolInvoked: true},
			want:  opencodeToolsRan,
		},
		{
			name:  "tool part only in the message response",
			state: &opencodeStreamState{},
			mr:    opencodeMessageResult{resp: respWithTool, settled: true},
			want:  opencodeToolsRan,
		},
		{
			// The response lists the turn's parts, so it can prove the
			// negative the stream no longer can.
			name:  "message response without a tool part",
			state: &opencodeStreamState{},
			mr:    opencodeMessageResult{resp: respWithoutTool, settled: true},
			want:  opencodeToolsNone,
		},
		{
			// Every tool part of the session crossed a stream that reached
			// session.idle, so the request's own outcome adds nothing.
			name:           "healthy stream and a failed message request",
			state:          &opencodeStreamState{},
			mr:             opencodeMessageResult{err: midRequestFailure, settled: true},
			streamComplete: true,
			want:           opencodeToolsNone,
		},
		{
			// The race: the stream died carrying no tool part, and the
			// response that would list one has not arrived.
			name:  "dead stream and a request still in flight",
			state: &opencodeStreamState{},
			mr:    opencodeMessageResult{},
			want:  opencodeToolsUnknown,
		},
		{
			// opencode never received the prompt, so this attempt ran
			// nothing - the retry (and the server restart behind it) stands.
			name:  "dead stream and a request that never connected",
			state: &opencodeStreamState{},
			mr:    opencodeMessageResult{err: dialFailure, settled: true},
			want:  opencodeToolsNone,
		},
		{
			// Connected means opencode held the prompt, and holding the
			// prompt is enough to have run a tool.
			name:  "dead stream and a request that failed after connecting",
			state: &opencodeStreamState{},
			mr:    opencodeMessageResult{err: midRequestFailure, settled: true},
			want:  opencodeToolsUnknown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveOpencodeToolEvidence(tc.state, tc.mr, tc.streamComplete); got != tc.want {
				t.Errorf("evidence = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAwaitOpencodeMessage covers the bounded wait itself: an answer that
// lands late still settles the evidence, and one that never lands leaves the
// result unsettled rather than empty-and-believed.
func TestAwaitOpencodeMessage(t *testing.T) {
	ch := make(chan opencodeMessageResult, 1)
	go func() {
		time.Sleep(5 * time.Millisecond)
		ch <- opencodeMessageResult{resp: &opencodeMessageResponse{}, settled: true}
	}()
	if mr := awaitOpencodeMessage(ch, time.Second); !mr.settled || mr.resp == nil {
		t.Errorf("expected the late answer to settle the result, got %+v", mr)
	}

	if mr := awaitOpencodeMessage(make(chan opencodeMessageResult), 5*time.Millisecond); mr.settled {
		t.Errorf("expected an unsettled result when nothing answers, got %+v", mr)
	}

	// The non-blocking peek is what the wait exists to back up.
	if mr := pollOpencodeMessage(make(chan opencodeMessageResult)); mr.settled {
		t.Errorf("expected the peek to report nothing yet, got %+v", mr)
	}
}

// opencodeSlowMessageServer serves a first turn whose SSE stream dies after
// the caller-supplied events and whose message request answers only once that
// stream is already gone - the interleaving the non-blocking receive cannot
// see through. An empty answer never arrives at all. Later attempts get a
// healthy stream and a successful turn, so a replay is visible as a second
// session.
func opencodeSlowMessageServer(t *testing.T, events, answer string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var sessions, streams atomic.Int32
	streamDead := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprintf(w, `{"id":"s%d"}`, sessions.Add(1))

		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if streams.Add(1) == 1 {
				fmt.Fprint(w, events)
				killResponseStream(t, w)
				close(streamDead)
				return
			}
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")

		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			select {
			case <-streamDead:
			case <-release:
			case <-r.Context().Done():
			}
			if answer == "" {
				// The turn's record never arrives, so the client's only
				// observation of the first attempt is the dead stream.
				select {
				case <-release:
				case <-r.Context().Done():
				}
				return
			}
			fmt.Fprint(w, answer)

		case strings.HasSuffix(r.URL.Path, "/message") && r.Method == http.MethodPost:
			fmt.Fprint(w, structuredSuccessBody)

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	// Registered after the close, so it runs before it: Close waits for a
	// handler still parked on an unanswered request.
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	return server, &sessions
}

// withFastEvidenceWait shortens the bounded wait for a failed turn's message
// response, so a test modelling a turn that never answers does not sit out
// the production budget. Returns a restore func, like withFastBackoff.
func withFastEvidenceWait(t *testing.T) func() {
	t.Helper()
	prev := opencodeEvidenceWait
	opencodeEvidenceWait = 20 * time.Millisecond
	return func() { opencodeEvidenceWait = prev }
}

// TestOpencodeAgent_InFlightMessageResponseWithholdsTheRetry is the race the
// tool-activity gate had left: a stream that dies after opencode ran a tool
// but before the tool event arrives leaves NO evidence at classification
// time - the stream carries no tool part, and the message response that
// would list one is still in flight. Reading that silence as "no tools ran"
// retries in a fresh session and replays the tool.
func TestOpencodeAgent_InFlightMessageResponseWithholdsTheRetry(t *testing.T) {
	defer withFastBackoff(t)()
	defer withFastEvidenceWait(t)()

	server, sessions := opencodeSlowMessageServer(t, stepFinishEvent, "")

	result, err := runOpencodeAgainst(t, server)
	if err == nil {
		t.Fatalf("expected the unverifiable turn to fail closed, got result %+v", result)
	}
	if got := sessions.Load(); got != 1 {
		t.Fatalf("sessions = %d, want no replay session while the turn is unverified", got)
	}
	msg := err.Error()
	if !strings.Contains(msg, "opencode events:") {
		t.Errorf("expected the stream failure to survive the refusal, got %q", msg)
	}
	if !strings.Contains(msg, "not retried: could not verify the failed turn ran no tools") {
		t.Errorf("expected the error to name the unverified turn, got %q", msg)
	}
}

// TestOpencodeAgent_LateMessageResponseWithoutToolsStillRetries is the other
// side of the same wait: the response arrives only after the stream is gone,
// and it is what PROVES the turn ran no tool. The blip the retry exists for
// still costs one retry rather than the whole round.
func TestOpencodeAgent_LateMessageResponseWithoutToolsStillRetries(t *testing.T) {
	defer withFastBackoff(t)()

	server, sessions := opencodeSlowMessageServer(t, stepFinishEvent, structuredSuccessBody)

	result, err := runOpencodeAgainst(t, server)
	if err != nil {
		t.Fatalf("expected retry to succeed, got %v", err)
	}
	if result == nil || result.Output == nil {
		t.Fatalf("expected structured output, got %+v", result)
	}
	if got := sessions.Load(); got != 2 {
		t.Errorf("sessions = %d, want exactly one retry session", got)
	}
}

// TestOpencodeAgent_LateMessageResponseWithToolPartWithholdsTheRetry is the
// case the race actually hid: the tool part exists, but only in a response
// that arrives after the stream died. Waiting for it turns an unverifiable
// turn into a known one, and the refusal says so.
func TestOpencodeAgent_LateMessageResponseWithToolPartWithholdsTheRetry(t *testing.T) {
	defer withFastBackoff(t)()

	server, sessions := opencodeSlowMessageServer(t, stepFinishEvent,
		`{"info":{"id":"msg1","role":"assistant"},"parts":[{"type":"tool"}]}`)

	result, err := runOpencodeAgainst(t, server)
	if err == nil {
		t.Fatalf("expected the turn that ran a tool to fail closed, got result %+v", result)
	}
	if got := sessions.Load(); got != 1 {
		t.Fatalf("sessions = %d, want no replay session after tool activity", got)
	}
	if msg := err.Error(); !strings.Contains(msg, "not retried: the failed turn already ran tools") {
		t.Errorf("expected the late tool part to be read as tool activity, got %q", msg)
	}
}
