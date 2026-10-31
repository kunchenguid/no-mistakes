package agentcfg

import "fmt"

// RunAgent is an explicit, immutable per-run native agent selection.
// Empty means the configured agent selection remains in force.
type RunAgent string

const RunAgentCodex RunAgent = "codex"

// LaunchOption keeps the existing optional Pi profile and a run agent choice
// together through the daemon's launch paths.
type LaunchOption interface{ launchOption() }

func (*PiProfile) launchOption() {}
func (RunAgent) launchOption()   {}

func ParseLaunchOptions(options []LaunchOption) (*PiProfile, RunAgent, error) {
	var profile *PiProfile
	var runAgent RunAgent
	for _, option := range options {
		switch value := option.(type) {
		case *PiProfile:
			if value != nil {
				profile = value
			}
		case RunAgent:
			runAgent = value
		}
	}
	if err := runAgent.Validate(); err != nil {
		return nil, "", err
	}
	if profile != nil && runAgent != "" {
		return nil, "", fmt.Errorf("per-run agent cannot be combined with a Pi profile")
	}
	return profile, runAgent, nil
}

func (a RunAgent) Validate() error {
	if a == "" || a == RunAgentCodex {
		return nil
	}
	return fmt.Errorf("unsupported per-run agent selection")
}
