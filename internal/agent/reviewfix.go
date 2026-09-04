package agent

import (
	"context"
	"fmt"
)

// reviewFixSelection owns a pipeline's default agent and its optional,
// separately configured Review-fixer agent. Run always delegates to the
// default agent; the pipeline asks AgentForReviewFix explicitly at the one
// Review-remediation seam. Keeping the selection explicit prevents a purpose
// string from becoming an accidental general per-step router.
type reviewFixSelection struct {
	primary Agent
	fixer   Agent
}

// NewReviewFixSelection binds a separately configured agent to Review
// remediation turns. Nil or identical selections collapse to the primary
// agent, preserving the pre-override behavior and close ownership.
func NewReviewFixSelection(primary, fixer Agent) Agent {
	if primary == nil {
		return nil
	}
	if fixer == nil || primary == fixer {
		return primary
	}
	return &reviewFixSelection{primary: primary, fixer: fixer}
}

// AgentForReviewFix returns the agent selected for Review remediation, falling
// back to the ordinary pipeline agent when no override was configured.
func AgentForReviewFix(a Agent) Agent {
	if selected, ok := a.(*reviewFixSelection); ok {
		return selected.fixer
	}
	return a
}

// HasReviewFixSelection reports whether AgentForReviewFix selects a separately
// owned agent. Callers use it to avoid decorating the fallback agent twice.
func HasReviewFixSelection(a Agent) bool {
	_, ok := a.(*reviewFixSelection)
	return ok
}

func (a *reviewFixSelection) Name() string { return a.primary.Name() }

func (a *reviewFixSelection) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	return a.primary.Run(ctx, opts)
}

// Session capabilities intentionally describe the fixer: the only durable
// pipeline session is the Review-fixer session, and recovery validates its
// stored provider against the run-owned combined selection.
func (a *reviewFixSelection) SupportsSessionResume() bool {
	return SupportsSessionResume(a.fixer)
}

func (a *reviewFixSelection) SupportsSessionProvider(provider string) bool {
	return SupportsSessionProvider(a.fixer, provider)
}

func (a *reviewFixSelection) Close() error {
	primaryErr := a.primary.Close()
	fixerErr := a.fixer.Close()
	switch {
	case primaryErr != nil && fixerErr != nil:
		return fmt.Errorf("close pipeline agents: %s: %v; review fixer %s: %v", a.primary.Name(), primaryErr, a.fixer.Name(), fixerErr)
	case primaryErr != nil:
		return primaryErr
	case fixerErr != nil:
		return fmt.Errorf("close review fixer %s: %w", a.fixer.Name(), fixerErr)
	default:
		return nil
	}
}

// NeutralizesGateInstructions covers both possible subprocess selections. A
// trusted repository opt-out must fail closed if either role could still load
// project instructions.
func (a *reviewFixSelection) NeutralizesGateInstructions() bool {
	return NeutralizesGateInstructions(a.primary) && NeutralizesGateInstructions(a.fixer)
}
