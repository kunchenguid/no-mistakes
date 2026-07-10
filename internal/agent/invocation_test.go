package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

type invocationRecordingAgent struct {
	calls  int
	opts   RunOpts
	result *Result
	err    error
}

func (a *invocationRecordingAgent) Name() string { return "recording" }
func (a *invocationRecordingAgent) Close() error { return nil }
func (a *invocationRecordingAgent) Run(_ context.Context, opts RunOpts) (*Result, error) {
	a.calls++
	a.opts = opts
	return a.result, a.err
}

type memoryInvocationJournal struct {
	starts    []types.InvocationAttemptStart
	terminals []types.InvocationAttemptTerminal
}

func (j *memoryInvocationJournal) StartInvocationAttempt(start types.InvocationAttemptStart) (string, error) {
	j.starts = append(j.starts, start)
	return "attempt-1", nil
}

func (j *memoryInvocationJournal) FinishInvocationAttempt(_ string, terminal types.InvocationAttemptTerminal) error {
	j.terminals = append(j.terminals, terminal)
	return nil
}

func TestLegacyInvokerValidatesBeforeLaunchingAgent(t *testing.T) {
	base := &invocationRecordingAgent{}
	journal := &memoryInvocationJournal{}
	invoker := NewLegacyInvoker(base, journal)

	request := InvocationRequest{
		Purpose: types.Purpose("unknown-purpose"),
		Scope: types.InvocationScope{
			Kind:         types.InvocationScopePipeline,
			RunID:        "run-1",
			StepResultID: "step-1",
			StepRoundID:  "round-1",
		},
		Payload: RunOpts{Prompt: "must not launch"},
	}
	if _, err := invoker.Invoke(context.Background(), request); err == nil {
		t.Fatal("Invoke() error = nil, want unknown-purpose validation error")
	}
	if base.calls != 0 {
		t.Fatalf("agent calls = %d, want 0", base.calls)
	}
	if len(journal.starts) != 0 {
		t.Fatalf("journal starts = %d, want 0", len(journal.starts))
	}
}

func TestLegacyInvokerPersistsStartBeforeForwardingPayloadAndTerminal(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	chunks := ""
	base := &invocationRecordingAgent{result: &Result{
		Output: json.RawMessage(`{"ok":true}`),
		Usage:  TokenUsage{InputTokens: 11, OutputTokens: 7, CacheReadTokens: 3, CacheCreationTokens: 2},
	}}
	journal := &memoryInvocationJournal{}
	invoker := NewLegacyInvoker(base, journal)
	scope := types.InvocationScope{
		Kind:         types.InvocationScopePipeline,
		RunID:        "run-1",
		StepResultID: "step-1",
		StepRoundID:  "round-1",
	}
	payload := RunOpts{
		Prompt:     "review this",
		CWD:        "/work/repo",
		JSONSchema: schema,
		OnChunk: func(text string) {
			chunks += text
		},
	}

	if _, err := invoker.Invoke(context.Background(), InvocationRequest{
		Purpose: types.PurposeInitialReview,
		Scope:   scope,
		Payload: payload,
	}); err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}
	if base.calls != 1 {
		t.Fatalf("agent calls = %d, want 1", base.calls)
	}
	if base.opts.Prompt != payload.Prompt || base.opts.CWD != payload.CWD || string(base.opts.JSONSchema) != string(schema) {
		t.Fatalf("forwarded payload = %+v, want prompt/CWD/schema unchanged", base.opts)
	}
	base.opts.OnChunk("streamed")
	if chunks != "streamed" {
		t.Fatalf("stream callback produced %q, want streamed", chunks)
	}
	if len(journal.starts) != 1 {
		t.Fatalf("journal starts = %d, want 1", len(journal.starts))
	}
	start := journal.starts[0]
	if start.Purpose != types.PurposeInitialReview || start.Scope != scope || start.CandidateKey != types.LegacyCandidateKey {
		t.Fatalf("start = %+v, want registered purpose, stable scope, and legacy candidate", start)
	}
	if start.Role != types.InvocationRoleVerifier {
		t.Fatalf("start role = %q, want %q", start.Role, types.InvocationRoleVerifier)
	}
	if len(journal.terminals) != 1 {
		t.Fatalf("journal terminals = %d, want 1", len(journal.terminals))
	}
	terminal := journal.terminals[0]
	if terminal.Outcome != types.InvocationOutcomeSucceeded {
		t.Fatalf("terminal outcome = %q, want %q", terminal.Outcome, types.InvocationOutcomeSucceeded)
	}
	if terminal.InputTokens != 11 || terminal.OutputTokens != 7 || terminal.CacheReadTokens != 3 || terminal.CacheCreationTokens != 2 {
		t.Fatalf("terminal usage = %+v, want agent result usage", terminal)
	}
}

func TestLegacyInvokerRecordsCancellationWithoutRawError(t *testing.T) {
	base := &invocationRecordingAgent{err: context.Canceled}
	journal := &memoryInvocationJournal{}
	invoker := NewLegacyInvoker(base, journal)

	_, err := invoker.Invoke(context.Background(), InvocationRequest{
		Purpose: types.PurposeUnstructuredTestRepair,
		Scope: types.InvocationScope{
			Kind:           types.InvocationScopeUtility,
			UtilityScopeID: "utility-1",
		},
		Payload: RunOpts{Prompt: "repair"},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Invoke() error = %v, want context.Canceled", err)
	}
	if len(journal.terminals) != 1 {
		t.Fatalf("journal terminals = %d, want 1", len(journal.terminals))
	}
	if journal.terminals[0].Outcome != types.InvocationOutcomeCancelled {
		t.Fatalf("terminal = %+v, want cancelled", journal.terminals[0])
	}
}

func TestValidateInvocationRequestRejectsMixedOrIncompleteScopes(t *testing.T) {
	tests := []struct {
		name  string
		scope types.InvocationScope
	}{
		{name: "pipeline missing round", scope: types.InvocationScope{Kind: types.InvocationScopePipeline, RunID: "run", StepResultID: "step"}},
		{name: "utility missing ID", scope: types.InvocationScope{Kind: types.InvocationScopeUtility}},
		{name: "utility with pipeline ID", scope: types.InvocationScope{Kind: types.InvocationScopeUtility, UtilityScopeID: "utility", RunID: "run"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := InvocationRequest{Purpose: types.PurposeInitialReview, Scope: tt.scope}
			if err := ValidateInvocationRequest(request); err == nil {
				t.Fatal("ValidateInvocationRequest() error = nil")
			}
		})
	}
}
