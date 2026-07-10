package agent

import (
	"context"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// InvocationRequest is the application-facing agent contract. Callers name a
// semantic Purpose and durable owner while RunOpts remains the unchanged native
// adapter payload.
type InvocationRequest struct {
	Purpose types.Purpose
	Scope   types.InvocationScope
	Payload RunOpts
	// Tier selects which Profile in the Purpose's Route to launch, so a repair
	// coordinator can escalate through the cascade. Zero is the first (and only)
	// tier for single-tier Routes.
	Tier int
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
