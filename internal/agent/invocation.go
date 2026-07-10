package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// InvocationRequest is the application-facing agent contract. Callers name a
// semantic Purpose and durable owner while RunOpts remains the unchanged native
// adapter payload.
type InvocationRequest struct {
	Purpose types.Purpose
	Scope   types.InvocationScope
	Payload RunOpts
}

// ValidateInvocationRequest rejects invalid semantic ownership before any
// native agent process can launch.
func ValidateInvocationRequest(request InvocationRequest) error {
	if _, err := types.PurposeDefinitionFor(request.Purpose); err != nil {
		return err
	}
	if err := request.Scope.Validate(); err != nil {
		return fmt.Errorf("invocation scope: %w", err)
	}
	return nil
}

// InvocationJournal persists immutable start and terminal facts.
type InvocationJournal interface {
	StartInvocationAttempt(start types.InvocationAttemptStart) (string, error)
	FinishInvocationAttempt(attemptID string, terminal types.InvocationAttemptTerminal) error
}

// Invoker validates, records, and executes semantic invocation requests.
type Invoker interface {
	Invoke(ctx context.Context, request InvocationRequest) (*Result, error)
}

type legacyInvoker struct {
	agent   Agent
	journal InvocationJournal
}

// NewLegacyInvoker bridges semantic requests to the existing single-agent
// execution path without changing runner, fallback, retry, or model behavior.
func NewLegacyInvoker(agent Agent, journal InvocationJournal) Invoker {
	return &legacyInvoker{agent: agent, journal: journal}
}

func (invoker *legacyInvoker) Invoke(ctx context.Context, request InvocationRequest) (*Result, error) {
	if err := ValidateInvocationRequest(request); err != nil {
		return nil, err
	}
	if invoker == nil || invoker.agent == nil {
		return nil, fmt.Errorf("invocation agent is nil")
	}
	if invoker.journal == nil {
		return nil, fmt.Errorf("invocation journal is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	definition, _ := types.PurposeDefinitionFor(request.Purpose)
	attemptID, err := invoker.journal.StartInvocationAttempt(types.InvocationAttemptStart{
		Purpose:      request.Purpose,
		Role:         definition.Role,
		Scope:        request.Scope,
		CandidateKey: types.LegacyCandidateKey,
	})
	if err != nil {
		return nil, fmt.Errorf("record invocation start: %w", err)
	}

	startedAt := time.Now()
	result, runErr := invoker.agent.Run(ctx, request.Payload)
	terminal := types.InvocationAttemptTerminal{
		Outcome:    invocationOutcome(ctx, runErr),
		DurationMS: time.Since(startedAt).Milliseconds(),
	}
	if result != nil {
		terminal.InputTokens = int64(result.Usage.InputTokens)
		terminal.OutputTokens = int64(result.Usage.OutputTokens)
		terminal.CacheReadTokens = int64(result.Usage.CacheReadTokens)
		terminal.CacheCreationTokens = int64(result.Usage.CacheCreationTokens)
	}
	if journalErr := invoker.journal.FinishInvocationAttempt(attemptID, terminal); journalErr != nil {
		journalErr = fmt.Errorf("record invocation terminal: %w", journalErr)
		if runErr != nil {
			return result, errors.Join(runErr, journalErr)
		}
		return result, journalErr
	}
	return result, runErr
}

func invocationOutcome(ctx context.Context, err error) types.InvocationOutcome {
	if err == nil {
		return types.InvocationOutcomeSucceeded
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return types.InvocationOutcomeCancelled
	}
	return types.InvocationOutcomeFailed
}

// BoundAgent adapts a semantic Invoker back to the native Agent shape for
// helpers that own prompt construction but not invocation ownership.
type BoundAgent struct {
	invoker Invoker
	purpose types.Purpose
	scope   types.InvocationScope
}

// BindInvocation returns an Agent whose every Run uses the supplied semantic
// Purpose and scope. Close is intentionally a no-op; the owner closes the
// underlying native agent exactly once.
func BindInvocation(invoker Invoker, purpose types.Purpose, scope types.InvocationScope) Agent {
	return &BoundAgent{invoker: invoker, purpose: purpose, scope: scope}
}

func (agent *BoundAgent) Name() string { return "bound-invocation" }
func (agent *BoundAgent) Close() error { return nil }
func (agent *BoundAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	if agent == nil || agent.invoker == nil {
		return nil, fmt.Errorf("bound invocation is nil")
	}
	return agent.invoker.Invoke(ctx, InvocationRequest{Purpose: agent.purpose, Scope: agent.scope, Payload: opts})
}
