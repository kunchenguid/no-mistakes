package quota

import (
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// StepClass states the reasoning class a step's agent turn needs.
//
// This is the only piece of judgement in this package, and it is written down
// explicitly because no local evidence states it: quota-axi knows what each
// provider can serve, but only the pipeline knows what a step asks of it. The
// split is by consequence, not by step size. A step that judges, changes, or
// repairs code needs the strongest class the provider attests (review decides
// what must change, test writes the proof, rebase resolves two versions of the
// same lines, and a CI repair is a code change too). A step that records,
// reformats, or summarizes is medium: its work is corrected by a later pass or
// reviewed by a human, so a provider that tops out at medium is a fine choice
// there even when it is unusable above it. An unknown step - a custom gate, whose
// name this package has never seen - is high, because fail-closed beats routing
// a step nobody classified onto the cheapest provider available.
func StepClass(step types.StepName) Class {
	switch step {
	case types.StepDocument, types.StepLint, types.StepPR, types.StepPush, types.StepIntent:
		return ClassMedium
	default:
		return ClassHigh
	}
}

// Budgets are the per-step ceilings a run may spend on agent work, taken from
// the operator's own configuration.
//
// The feasibility gate compares a provider's projected runway against these
// rather than against a guess at how long a step "usually" takes: the configured
// timeout IS this pipeline's statement of how much agent time that step may
// consume in one invocation, and inventing a second, unpublished number would
// make the gate unauditable.
type Budgets struct {
	Default time.Duration
	Review  time.Duration
	Test    time.Duration
}

// For returns the ceiling that governs one step.
func (b Budgets) For(step types.StepName) time.Duration {
	switch step {
	case types.StepReview:
		if b.Review > 0 {
			return b.Review
		}
	case types.StepTest:
		if b.Test > 0 {
			return b.Test
		}
	}
	return b.Default
}
