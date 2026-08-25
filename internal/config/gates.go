package config

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	// MaxGates is the largest number of extra gates a repository may declare.
	MaxGates = 16
	// MaxGateNameLen bounds a gate name so the derived step name stays short
	// enough for the step tables, the PR body, and the attestation payload.
	MaxGateNameLen = 40
	// MaxGateInstructionsBytes bounds an agent gate's injected prompt for the
	// same reason MaxReviewPathInstructionsBytes bounds path_instructions: an
	// oversized prompt fails the agent invocation outright instead of degrading.
	MaxGateInstructionsBytes = 16384
)

// GateAnchors are the core steps an extra gate may be anchored to. The
// delivery tail (push, pr, ci) is deliberately excluded: a gate that ran after
// push would validate a branch the world can already see, which is the
// opposite of what a gate is for. intent is excluded because it establishes
// the acceptance criteria the later gates check against, so nothing can
// usefully run before it.
func GateAnchors() []types.StepName {
	return []types.StepName{types.StepRebase, types.StepReview, types.StepTest, types.StepDocument, types.StepLint}
}

// Gate is one repository-declared extra check that runs immediately after its
// anchor core step. A gate can only ADD a verdict to a run: it cannot skip,
// reorder, or replace a core step, and a failing gate fails the run closed.
type Gate struct {
	Name         string         `yaml:"name"`
	After        types.StepName `yaml:"after"`
	Command      string         `yaml:"command"`
	Instructions string         `yaml:"instructions"`
}

// IsAgent reports whether the gate is agent-driven rather than a command.
func (g Gate) IsAgent() bool { return strings.TrimSpace(g.Command) == "" }

// StepName is the gate's identity in the run's step sequence. The anchor is
// encoded into the name so types.StepName.Order can resolve a gate's execution
// order without reaching for the config that declared it.
func (g Gate) StepName() types.StepName {
	return types.CustomGateStepName(g.After, g.Name)
}

func validGateAnchor(name types.StepName) bool {
	for _, anchor := range GateAnchors() {
		if anchor == name {
			return true
		}
	}
	return false
}

func validGateName(name string) error {
	if name == "" {
		return fmt.Errorf("must not be empty")
	}
	if len(name) > MaxGateNameLen {
		return fmt.Errorf("is %d characters, at most %d are allowed", len(name), MaxGateNameLen)
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-' && i > 0 && i < len(name)-1:
		default:
			return fmt.Errorf("must be lowercase letters, digits, and inner hyphens only")
		}
	}
	return nil
}

// validateGates fails the config closed on a gates list the pipeline could not
// honor deterministically. Like validateReviewRaw this deliberately also runs
// on the PUSHED copy even though EffectiveRepoConfig discards a pushed gates
// block: the trusted-copy read aborts every run whose default-branch
// .no-mistakes.yaml fails these checks, so a branch carrying an invalid block
// has to fail here, before it merges, rather than brick the pipeline afterwards.
func validateGates(gates []Gate) error {
	if len(gates) > MaxGates {
		return fmt.Errorf("gates has %d entries, at most %d are allowed", len(gates), MaxGates)
	}
	seen := make(map[string]int, len(gates))
	for i, gate := range gates {
		name := strings.TrimSpace(gate.Name)
		if err := validGateName(name); err != nil {
			return fmt.Errorf("gates[%d].name %q %w", i, gate.Name, err)
		}
		if types.IsCoreStepName(types.StepName(name)) {
			return fmt.Errorf("gates[%d].name %q is a core step name; an extra gate must not shadow a core step", i, name)
		}
		if first, dup := seen[name]; dup {
			return fmt.Errorf("gates[%d].name %q duplicates gates[%d]; each gate needs its own name", i, name, first)
		}
		seen[name] = i

		if gate.After == "" {
			return fmt.Errorf("gates[%d] (%q).after must name the core step it runs after", i, name)
		}
		if !validGateAnchor(gate.After) {
			return fmt.Errorf("gates[%d] (%q).after %q is not an anchorable core step; valid: %s", i, name, gate.After, gateAnchorText())
		}

		hasCommand := strings.TrimSpace(gate.Command) != ""
		hasInstructions := strings.TrimSpace(gate.Instructions) != ""
		switch {
		case hasCommand && hasInstructions:
			return fmt.Errorf("gates[%d] (%q) sets both command and instructions; a gate is either a command or an agent review, not both", i, name)
		case !hasCommand && !hasInstructions:
			return fmt.Errorf("gates[%d] (%q) needs either a command to run or instructions for an agent review", i, name)
		}
		if hasInstructions {
			if RenderedInstructions(gate.Instructions) == "" {
				return fmt.Errorf("gates[%d] (%q).instructions is left empty once merge-conflict markers are removed; write the rule without <<<<<<<, =======, or >>>>>>>", i, name)
			}
			if size := len(gate.Instructions); size > MaxGateInstructionsBytes {
				return fmt.Errorf("gates[%d] (%q).instructions is %d bytes, at most %d are allowed so the prompt stays within budget", i, name, size, MaxGateInstructionsBytes)
			}
		}
	}
	return nil
}

func gateAnchorText() string {
	names := make([]string, 0, len(GateAnchors()))
	for _, anchor := range GateAnchors() {
		names = append(names, string(anchor))
	}
	return strings.Join(names, ", ")
}

func copyGates(gates []Gate) []Gate {
	if len(gates) == 0 {
		return nil
	}
	out := make([]Gate, len(gates))
	copy(out, gates)
	return out
}
