package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
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
