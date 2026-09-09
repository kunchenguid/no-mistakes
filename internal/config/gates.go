package config

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
	"gopkg.in/yaml.v3"
)

const (
	// MaxGates is the largest number of extra gates a repository may declare.
	MaxGates = 16
	// MaxGateNameLen bounds a gate name so the derived step name stays short
	// enough for the step tables, the PR body, and the attestation payload.
	// types owns the bound because it also owns the step-name encoding the
	// bound exists to keep short.
	MaxGateNameLen = types.MaxCustomGateLabelLen
)

// Gate is one repository-declared extra check that runs immediately after its
// anchor core step. A gate can only add a verdict to a run: it cannot skip,
// reorder, or replace a core step, and a failing gate parks for an operator
// decision instead of weakening the core result.
type Gate struct {
	Name    string         `yaml:"name" json:"name"`
	After   types.StepName `yaml:"after" json:"after"`
	Command string         `yaml:"command" json:"command,omitempty"`
}

func (g *Gate) UnmarshalYAML(value *yaml.Node) error {
	var decoded struct {
		Name         string         `yaml:"name"`
		After        types.StepName `yaml:"after"`
		Command      string         `yaml:"command"`
		Instructions yaml.Node      `yaml:"instructions"`
	}
	if err := value.Decode(&decoded); err != nil {
		return err
	}
	if !decoded.Instructions.IsZero() {
		return fmt.Errorf("instructions: agent gates are not supported; use command")
	}
	*g = Gate{Name: decoded.Name, After: decoded.After, Command: decoded.Command}
	return nil
}

func (g *Gate) UnmarshalJSON(data []byte) error {
	type gatePlain Gate
	var decoded struct {
		gatePlain
		Instructions json.RawMessage `json:"instructions"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	if len(decoded.Instructions) != 0 && string(decoded.Instructions) != "null" {
		return fmt.Errorf("instructions: agent gates are not supported; use command")
	}
	*g = Gate(decoded.gatePlain)
	return nil
}

// StepName is the gate's identity in the run's step sequence. The anchor is
// encoded into the name so types.StepName.Order can resolve a gate's execution
// order without reaching for the config that declared it.
func (g Gate) StepName() types.StepName {
	return types.CustomGateStepName(g.After, g.Name)
}

func validGateName(name string) error {
	if name == "" {
		return fmt.Errorf("must not be empty")
	}
	if len(name) > MaxGateNameLen {
		return fmt.Errorf("is %d characters, at most %d are allowed", len(name), MaxGateNameLen)
	}
	// types.ValidCustomGateLabel is the single owner of the syntax, because the
	// same rule is what makes the derived step name safe to use as a filename.
	if !types.ValidCustomGateLabel(name) {
		return fmt.Errorf("must be lowercase letters, digits, and inner hyphens only")
	}
	return nil
}

// validateGates normalizes each gate in place and fails the config closed on a
// gates list the pipeline could not honor deterministically. Like
// validateReviewRaw this deliberately also runs on the PUSHED copy even though
// EffectiveRepoConfig discards a pushed gates block: the trusted-copy read
// aborts every run whose default-branch .no-mistakes.yaml fails these checks,
// so a branch carrying an invalid block has to fail here, before it merges,
// rather than brick the pipeline afterwards.
//
// Normalization is part of validation rather than a separate pass so the two
// can never disagree: a quoted `name: " arch "` used to validate as "arch"
// while Gate.StepName kept the raw spelling, putting a padded name into the
// step tables, the attestation, and every command an operator has to type.
func validateGates(gates []Gate) error {
	if len(gates) > MaxGates {
		return fmt.Errorf("gates has %d entries, at most %d are allowed", len(gates), MaxGates)
	}
	seen := make(map[string]int, len(gates))
	for i := range gates {
		raw := gates[i].Name
		gates[i].Name = strings.TrimSpace(raw)
		gate, name := gates[i], gates[i].Name
		if err := validGateName(name); err != nil {
			return fmt.Errorf("gates[%d].name %q %w", i, raw, err)
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
		if !gate.After.IsCustomGateAnchor() {
			return fmt.Errorf("gates[%d] (%q).after %q is not an anchorable core step; valid: %s", i, name, gate.After, gateAnchorText())
		}

		if strings.TrimSpace(gate.Command) == "" {
			return fmt.Errorf("gates[%d] (%q).command must not be empty", i, name)
		}
	}
	return nil
}

func gateAnchorText() string {
	anchors := types.CustomGateAnchors()
	names := make([]string, 0, len(anchors))
	for _, anchor := range anchors {
		names = append(names, string(anchor))
	}
	return strings.Join(names, ", ")
}

// MarshalGates encodes a run's resolved gate list so the run can carry it for
// its whole lifetime. Configuration decides a run's gates exactly once, at run
// creation, exactly as it decides worktree placement once (see
// worktrees.RecordedDir): the trusted default branch may gain or lose a gate
// while a run is parked, and re-resolving it later would hand recovery a step
// sequence the run never executed. An empty list encodes as the empty string,
// so a run that pinned no gates is indistinguishable from a run written before
// this was pinned at all - both mean the bare core pipeline.
func MarshalGates(gates []Gate) (string, error) {
	if len(gates) == 0 {
		return "", nil
	}
	data, err := json.Marshal(gates)
	if err != nil {
		return "", fmt.Errorf("encode gates: %w", err)
	}
	return string(data), nil
}

// ParseGates decodes a gate list pinned to a run. It revalidates the decoded
// gates through the same rules the config parser applies, so a payload that no
// longer describes a gate list this build can honor fails its reader closed
// with a reason instead of silently degrading the run to the core pipeline -
// the one thing an absent pin legitimately means.
func ParseGates(payload string) ([]Gate, error) {
	if strings.TrimSpace(payload) == "" {
		return nil, nil
	}
	var gates []Gate
	if err := json.Unmarshal([]byte(payload), &gates); err != nil {
		return nil, fmt.Errorf("decode gates: %w", err)
	}
	if err := validateGates(gates); err != nil {
		return nil, err
	}
	return gates, nil
}

func copyGates(gates []Gate) []Gate {
	if len(gates) == 0 {
		return nil
	}
	out := make([]Gate, len(gates))
	copy(out, gates)
	return out
}
