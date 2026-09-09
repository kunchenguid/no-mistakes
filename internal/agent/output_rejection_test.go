package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestCopilotDoesNotReplaceRejectedFindingsWithEarlierCleanReport(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"findings":{"type":"array","items":{"type":"object","required":["severity"]}}},"required":["findings"]}`)
	result, err := finalizeCopilotResult([]string{`{"findings":[]}`, `{"findings":[{"title":"Keep this finding","body":"A result can be lost."}]}`}, schema, TokenUsage{})
	if result != nil || !IsStructuredOutputRejected(err) || !strings.Contains(string(RejectedStructuredOutput(err)), "Keep this finding") {
		t.Fatalf("newer finding lost: result=%+v err=%v", result, err)
	}
}

func TestStructuredOutputRejectionDoesNotReplayTask(t *testing.T) {
	err := rejectStructuredOutput(fmt.Errorf("output parse: finding describes HTTP 429, connection refused, and process exited: failure"))
	calls := 0
	_, got := runWithRetry(context.Background(), "pi", RunOpts{}, 3, classifyTransient, nil, func() (*Result, error) {
		calls++
		return nil, err
	})
	if calls != 1 || got != err {
		t.Fatalf("replayed rejected report: calls=%d err=%v", calls, got)
	}
	if isAgentUnavailableError(err) {
		t.Fatal("rejected report must not fall back to a new provider")
	}
}

func TestStructuredOutputRejectionPreservesOnlyUnambiguousPayload(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"findings":{"type":"array","items":{"type":"object","required":["severity"]}}},"required":["findings"]}`)
	payload := `{"findings":[{"title":"[P1] Preserve result","body":"` + strings.Repeat("synthetic observation ", 30) + `"}]}`
	for _, tc := range []struct {
		name, text  string
		recoverable bool
	}{
		{"bare", payload, true},
		{"prose and fence", "Review complete.\n```json\n" + payload + "\n```", true},
		{"two objects", payload + "\n" + payload, false},
		{"duplicate findings", `{"findings":[{"body":"Do not lose this"}],"findings":[]}`, false},
		{"case colliding findings", `{"findings":[{"body":"Do not lose this"}],"Findings":[]}`, false},
		{"duplicate nested body", `{"findings":[{"body":"first","body":"second"}]}`, false},
		{"duplicate then clean", `{"findings":[{"body":"first","body":"second"}]} {"findings":[]}`, false},
		{"malformed", `{"findings":[}`, false},
		{"invalid finding then clean", payload + ` {"findings":[]}`, false},
		{"invalid fence then clean", "```json\n" + payload + "\n```\n```json\n{\"findings\":[]}\n```", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := finalizeTextResult("pi", tc.text, schema, TokenUsage{})
			if err == nil || result != nil || !IsStructuredOutputRejected(err) {
				t.Fatalf("result=%+v error=%v; want rejection", result, err)
			}
			got := RejectedStructuredOutput(fmt.Errorf("wrapped: %w", err))
			if tc.recoverable && string(got) != payload {
				t.Fatalf("rejected payload lost or truncated: %s", got)
			}
			if !tc.recoverable && got != nil {
				t.Fatalf("ambiguous payload offered for correction: %s", got)
			}
		})
	}
}
