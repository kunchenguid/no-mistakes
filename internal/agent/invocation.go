package agent

import (
	"context"
	"errors"
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

// ProfileUnavailableError means routing exhausted one Profile without a usable
// Candidate. Cause is the last executed Candidate's operational failure, or nil
// when every Candidate was skipped because its provider circuit was already
// open. An executed Candidate's non-operational bad result is returned directly
// and never classified as ProfileUnavailableError.
type ProfileUnavailableError struct {
	Profile string
	Cause   error
}

func (e *ProfileUnavailableError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("profile %q exhausted every candidate after operational failures: %v", e.Profile, e.Cause)
	}
	return fmt.Sprintf("profile %q has no available candidate: all provider circuits are open", e.Profile)
}

func (e *ProfileUnavailableError) Unwrap() error { return e.Cause }

// fatalInvocationError marks an invocation boundary that cannot safely be
// retried. Journal failures and failed candidate restoration are transaction
// failures, not resumability failures, so session fallback must preserve them.
type fatalInvocationError struct {
	err error
}

func (e *fatalInvocationError) Error() string { return e.err.Error() }
func (e *fatalInvocationError) Unwrap() error { return e.err }

// FatalInvocationError prevents a higher session layer from retrying an
// invocation whose terminal fact or isolated candidate could not be made safe.
func FatalInvocationError(err error) error {
	if err == nil || IsFatalInvocationError(err) {
		return err
	}
	return &fatalInvocationError{err: err}
}

// IsFatalInvocationError reports whether retrying the invocation could violate
// its journal or candidate-isolation transaction.
func IsFatalInvocationError(err error) bool {
	var fatal *fatalInvocationError
	return errors.As(err, &fatal)
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
