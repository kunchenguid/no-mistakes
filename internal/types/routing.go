package types

import "fmt"

// Runner names a native agent backend that a routed Candidate executes through.
// It deliberately excludes the legacy agent selectors: only the two runners
// that translate a normalized model and effort to native arguments are valid.
type Runner string

const (
	RunnerCodex  Runner = "codex"
	RunnerClaude Runner = "claude"
)

// Validate reports whether the runner is one no-mistakes can launch.
func (r Runner) Validate() error {
	switch r {
	case RunnerCodex, RunnerClaude:
		return nil
	default:
		return fmt.Errorf("unsupported runner %q", r)
	}
}

// FailureDomain returns the provider family a runner belongs to, so a
// classified operational failure can open the right circuit.
func (r Runner) FailureDomain() (FailureDomain, error) {
	switch r {
	case RunnerCodex:
		return FailureDomainOpenAI, nil
	case RunnerClaude:
		return FailureDomainAnthropic, nil
	default:
		return "", fmt.Errorf("unsupported runner %q", r)
	}
}

// Effort is the normalized reasoning effort a Candidate requests. Adapters
// translate it to each runner's exact native argument.
type Effort string

const (
	EffortLow    Effort = "low"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
	EffortXHigh  Effort = "xhigh"
)

// Validate reports whether the effort is one of the four normalized levels.
func (e Effort) Validate() error {
	switch e {
	case EffortLow, EffortMedium, EffortHigh, EffortXHigh:
		return nil
	default:
		return fmt.Errorf("invalid effort %q", e)
	}
}
