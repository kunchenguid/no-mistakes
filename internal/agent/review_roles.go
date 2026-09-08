package agent

import (
	"context"
	"errors"
	"time"
)

// WithReviewAgents routes only review and review-fix invocations. Nil roles
// keep the default agent (including its fallback chain). Ownership of all
// supplied agents transfers to the wrapper; non-nil roles must be independent
// instances. Only the fixer resumes sessions; review turns remain fresh.
func WithReviewAgents(primary, reviewer, fixer Agent) Agent {
	if reviewer == nil && fixer == nil {
		return primary
	}
	return &reviewAgents{primary: primary, reviewer: reviewer, fixer: fixer}
}

type reviewAgents struct{ primary, reviewer, fixer Agent }

func (a *reviewAgents) Name() string { return a.primary.Name() }
func (a *reviewAgents) fixAgent() Agent {
	if a.fixer != nil {
		return a.fixer
	}
	return a.primary
}
func (a *reviewAgents) SupportsSessionResume() bool { return SupportsSessionResume(a.fixAgent()) }
func (a *reviewAgents) SupportsSessionProvider(provider string) bool {
	return SupportsSessionProvider(a.fixAgent(), provider)
}
func (a *reviewAgents) ReportsAgentAttempts() bool { return true }
func (a *reviewAgents) NeutralizesGateInstructions() bool {
	for _, current := range []Agent{a.primary, a.reviewer, a.fixer} {
		if current != nil && !NeutralizesGateInstructions(current) {
			return false
		}
	}
	return NeutralizesGateInstructions(a.primary)
}
func (a *reviewAgents) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	selected := a.primary
	switch opts.Purpose {
	case "review":
		if a.reviewer != nil {
			selected = a.reviewer
		}
		opts.Session = nil
	case "review-fix":
		selected = a.fixAgent()
	}
	started := time.Now()
	result, err := selected.Run(ctx, opts)
	if !ReportsAgentAttempts(selected) {
		emitAgentAttempt(opts, selected.Name(), result, err, started, time.Now())
	}
	if result != nil && result.Provider == "" {
		result.Provider = selected.Name()
	}
	return result, err
}
func (a *reviewAgents) Close() error {
	var errs []error
	for _, current := range []Agent{a.primary, a.reviewer, a.fixer} {
		if current != nil {
			errs = append(errs, current.Close())
		}
	}
	return errors.Join(errs...)
}
