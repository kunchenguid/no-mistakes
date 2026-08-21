// Package agentcfg is the harness-neutral agent tuning layer.
//
// Reasoning effort and model selection vary across the supported harnesses:
// codex takes `-m` plus a TOML config override, copilot and claude take
// `--effort`, pi calls the same idea `--thinking`, opencode has no launch flag
// and carries both in the session-message body, ACP targets are driven through
// acpx rather than the vendor CLI, and some harnesses expose neither knob.
// Before this package the only pipeline configuration lever was
// agent_args_override, so an operator had to know each harness's own flag, and
// the eval path carried a second, divergent copy of the model rule.
//
// A Profile states what the operator wants once; this package owns the single
// mapping down to what each harness can actually express. It is deliberately a
// normalization layer and nothing more: it never launches a process, never
// reads configuration files, and holds no state.
//
// Precedence is fixed and one-directional: a raw agent_args_override flag that
// already sets a knob natively always wins, and the Profile value for that knob
// is not emitted. That keeps every pre-existing configuration byte-identical
// and avoids handing a CLI the same knob twice, which different harnesses
// resolve differently (first-wins, last-wins, or hard error).
package agentcfg

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Effort is the harness-neutral reasoning-depth knob. The vocabulary is the
// union of what the supported harnesses accept; values pass through to the
// harness verbatim (codex wraps its value in the `-c` config form). Restricting
// it to this set catches typos at config-load time, while deliberately NOT
// gating per-harness value ranges: those drift with every CLI release, and a
// stale table that refuses a newly valid level is worse than the harness
// reporting the level it does not know. Per-harness ranges are documented in
// docs/src/content/docs/reference/global-config.md.
type Effort string

const (
	EffortMinimal Effort = "minimal"
	EffortLow     Effort = "low"
	EffortMedium  Effort = "medium"
	EffortHigh    Effort = "high"
	EffortXHigh   Effort = "xhigh"
	EffortMax     Effort = "max"
)

var efforts = []Effort{EffortMinimal, EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax}

// Efforts returns the canonical effort vocabulary in ascending depth order.
func Efforts() []Effort {
	out := make([]Effort, len(efforts))
	copy(out, efforts)
	return out
}

// EffortNames returns the canonical effort vocabulary as strings, for help and
// error text.
func EffortNames() []string {
	out := make([]string, 0, len(efforts))
	for _, e := range efforts {
		out = append(out, string(e))
	}
	return out
}

// ParseEffort validates a user-supplied effort spelling.
func ParseEffort(raw string) (Effort, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	for _, e := range efforts {
		if string(e) == value {
			return e, nil
		}
	}
	return "", fmt.Errorf("invalid effort %q (valid: %s)", raw, strings.Join(EffortNames(), ", "))
}

// Profile is the harness-neutral tuning surface: what the operator asked for,
// not how a given CLI spells it. The zero Profile means "leave the harness on
// its own defaults" and is what every configuration that predates this package
// resolves to.
type Profile struct {
	Model  string
	Effort Effort
}

// IsZero reports whether the profile asks for nothing.
func (p Profile) IsZero() bool { return p.Model == "" && p.Effort == "" }

// String renders the profile in the canonical comma-separated key=value form
// shared by eval candidates. It is empty for the zero profile.
func (p Profile) String() string {
	parts := make([]string, 0, 2)
	if p.Model != "" {
		parts = append(parts, "model="+p.Model)
	}
	if p.Effort != "" {
		parts = append(parts, "effort="+string(p.Effort))
	}
	return strings.Join(parts, ",")
}

// Knob names one common field.
type Knob string

const (
	KnobModel  Knob = "model"
	KnobEffort Knob = "effort"
)

// Mechanism describes how a harness expresses a knob.
type Mechanism string

const (
	// MechanismUnsupported means no-mistakes has no verified way to set the
	// knob for that harness. The raw agent_args_override escape hatch is
	// still available for operators whose build of the CLI accepts a flag we
	// could not verify.
	MechanismUnsupported Mechanism = "unsupported"
	// MechanismArgs means no-mistakes injects native CLI flags into the argv
	// it already builds for that harness.
	MechanismArgs Mechanism = "args"
	// MechanismRequest means the adapter carries the value in its own
	// protocol rather than in argv, because the launch command cannot express
	// it (opencode runs as `opencode serve`, which rejects model flags; the
	// knob belongs to the session message body).
	MechanismRequest Mechanism = "request"
)

// knob is one harness's mapping for one common field.
type knob struct {
	mechanism Mechanism
	// args renders the native argv fragment. Nil for non-args mechanisms.
	args func(value string) []string
	// pinned reports whether raw agent_args_override args already set this
	// knob through the mechanism this harness actually uses. Nil means a raw
	// override can never reach the knob, so there is nothing to defer to.
	pinned func(rawArgs []string) bool
	// validate rejects a value this harness structurally cannot accept.
	validate func(value string) error
}

type harness struct {
	model  knob
	effort knob
}

func flagArgs(flag string) func(string) []string {
	return func(value string) []string { return []string{flag, value} }
}

// flagPinned matches a flag in both its `--flag value` and `--flag=value`
// spellings, across every alias the harness accepts for the same knob.
func flagPinned(flags ...string) func([]string) bool {
	return func(rawArgs []string) bool {
		for _, arg := range rawArgs {
			for _, flag := range flags {
				if arg == flag || strings.HasPrefix(arg, flag+"=") {
					return true
				}
			}
		}
		return false
	}
}

// codexConfigArgs renders codex's `-c key="value"` config override. The quotes
// are part of the argument, matching the spelling codex documents for string
// config values.
func codexConfigArgs(key string) func(string) []string {
	return func(value string) []string { return []string{"-c", key + `="` + value + `"`} }
}

// codexConfigPinned reports whether a raw override already sets a codex config
// key, in either the split `-c key=value` form or a single token containing the
// config flag and assignment.
func codexConfigPinned(key string, flags ...string) func([]string) bool {
	flagMatch := flagPinned(flags...)
	assignmentMatches := func(arg string) bool {
		assignmentKey, _, ok := strings.Cut(strings.TrimSpace(arg), "=")
		return ok && strings.TrimSpace(assignmentKey) == key
	}
	return func(rawArgs []string) bool {
		if len(flags) > 0 && flagMatch(rawArgs) {
			return true
		}
		for i, arg := range rawArgs {
			if (arg == "-c" || arg == "--config") && i+1 < len(rawArgs) && assignmentMatches(rawArgs[i+1]) {
				return true
			}
			for _, prefix := range []string{"-c ", "--config ", "-c=", "--config="} {
				if strings.HasPrefix(arg, prefix) && assignmentMatches(strings.TrimPrefix(arg, prefix)) {
					return true
				}
			}
		}
		return false
	}
}

func unsupported() knob { return knob{mechanism: MechanismUnsupported} }

// requireProviderModel enforces opencode's provider/model spelling, which the
// session-message body requires as two separate fields. Catching it at
// validation time is what keeps a bare "gpt-5" from being silently dropped.
func requireProviderModel(value string) error {
	if _, _, ok := SplitProviderModel(value); !ok {
		return fmt.Errorf("model %q must use opencode's provider/model form (for example openai/gpt-5)", value)
	}
	return nil
}

// SplitProviderModel splits opencode's provider/model spelling into the
// providerID and modelID the session-message body requires.
func SplitProviderModel(model string) (provider, id string, ok bool) {
	provider, id, found := strings.Cut(strings.TrimSpace(model), "/")
	provider = strings.TrimSpace(provider)
	id = strings.TrimSpace(id)
	if !found || provider == "" || id == "" {
		return "", "", false
	}
	return provider, id, true
}

// harnesses is the whole mapping. Every native flag here was read off the
// harness's own `--help`, not inferred.
var harnesses = map[types.AgentName]harness{
	types.AgentClaude: {
		model:  knob{mechanism: MechanismArgs, args: flagArgs("--model"), pinned: flagPinned("--model")},
		effort: knob{mechanism: MechanismArgs, args: flagArgs("--effort"), pinned: flagPinned("--effort")},
	},
	types.AgentCodex: {
		// codex takes the model as a real flag but reasoning depth as a TOML
		// config override, so the two knobs use different shapes.
		model:  knob{mechanism: MechanismArgs, args: flagArgs("-m"), pinned: codexConfigPinned("model", "-m", "--model")},
		effort: knob{mechanism: MechanismArgs, args: codexConfigArgs("model_reasoning_effort"), pinned: codexConfigPinned("model_reasoning_effort")},
	},
	types.AgentGrok: {
		model:  knob{mechanism: MechanismArgs, args: flagArgs("--model"), pinned: flagPinned("--model", "-m")},
		effort: knob{mechanism: MechanismArgs, args: flagArgs("--reasoning-effort"), pinned: flagPinned("--reasoning-effort", "--effort")},
	},
	types.AgentCopilot: {
		model:  knob{mechanism: MechanismArgs, args: flagArgs("--model"), pinned: flagPinned("--model")},
		effort: knob{mechanism: MechanismArgs, args: flagArgs("--effort"), pinned: flagPinned("--effort", "--reasoning-effort")},
	},
	types.AgentPi: {
		model:  knob{mechanism: MechanismArgs, args: flagArgs("--model"), pinned: flagPinned("--model")},
		effort: knob{mechanism: MechanismArgs, args: flagArgs("--thinking"), pinned: flagPinned("--thinking")},
	},
	types.AgentOpenCode: {
		// `opencode serve` rejects model and variant flags outright, so raw
		// args can never reach either knob and there is nothing to defer to.
		// Both ride the session-message body instead.
		model:  knob{mechanism: MechanismRequest, validate: requireProviderModel},
		effort: knob{mechanism: MechanismRequest},
	},
	// rovodev is driven through `acli rovodev serve` plus a REST session API
	// that takes no model parameter, and antigravity's agy CLI parses flags
	// strictly. Neither has a mechanism no-mistakes can set, so both knobs are
	// refused here rather than emitted and silently ignored.
	types.AgentRovoDev:     {model: unsupported(), effort: unsupported()},
	types.AgentAntigravity: {model: unsupported(), effort: unsupported()},
}

// acpHarness covers every ACP-driven name: the first-class aliases (cursor) and
// explicit acp:<target> spellings. no-mistakes never speaks ACP itself; it
// shells out to acpx, whose own `--model <id>` is therefore the model
// mechanism. acpx exposes no reasoning-effort surface, so effort stays
// unmappable for ACP targets - deliberately refused rather than dropped.
var acpHarness = harness{
	model:  knob{mechanism: MechanismArgs, args: flagArgs("--model")},
	effort: unsupported(),
}

func lookup(name types.AgentName) (harness, bool) {
	if _, ok := types.ACPTargetFor(name); ok {
		return acpHarness, true
	}
	h, ok := harnesses[name]
	return h, ok
}

func (h harness) knob(k Knob) knob {
	if k == KnobEffort {
		return h.effort
	}
	return h.model
}

// Agents returns every agent name with an entry in the mapping, sorted. ACP
// targets are dynamic and therefore not listed; use MechanismFor for those.
func Agents() []types.AgentName {
	out := make([]types.AgentName, 0, len(harnesses))
	for name := range harnesses {
		out = append(out, name)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Known reports whether the mapping covers this agent name.
func Known(name types.AgentName) bool {
	_, ok := lookup(name)
	return ok
}

// MechanismFor reports how a harness expresses a knob.
func MechanismFor(name types.AgentName, k Knob) Mechanism {
	h, ok := lookup(name)
	if !ok {
		return MechanismUnsupported
	}
	return h.knob(k).mechanism
}

// Validate reports whether a harness can express every knob the profile sets,
// and whether each value has a shape that harness can accept. It is the single
// fail-closed check: an unmappable knob is refused at config-load and at agent
// construction rather than silently dropped, so a run never reports a model or
// effort it did not actually use.
func Validate(name types.AgentName, p Profile) error {
	if p.IsZero() {
		return nil
	}
	h, ok := lookup(name)
	if !ok {
		return fmt.Errorf("unknown agent %q", name)
	}
	if p.Effort != "" {
		if _, err := ParseEffort(string(p.Effort)); err != nil {
			return err
		}
	}
	for _, entry := range []struct {
		name  Knob
		value string
		knob  knob
	}{
		{KnobModel, p.Model, h.model},
		{KnobEffort, string(p.Effort), h.effort},
	} {
		if entry.value == "" {
			continue
		}
		if entry.knob.mechanism == MechanismUnsupported {
			return fmt.Errorf("agent %q cannot express %s; no-mistakes has no verified %s mechanism for it (%s)", name, entry.name, entry.name, escapeHatch(name))
		}
		if entry.knob.validate != nil {
			if err := entry.knob.validate(entry.value); err != nil {
				return fmt.Errorf("agent %q %s: %w", name, entry.name, err)
			}
		}
	}
	return nil
}

// escapeHatch names the surface that can still reach an unmappable knob. ACP
// names are not valid agent_args_override keys, so pointing an operator there
// would send them to a setting that refuses their agent; the raw ACP command is
// the only lever they have.
func escapeHatch(name types.AgentName) string {
	if target, ok := types.ACPTargetFor(name); ok {
		return "bake the flag into acp_registry_overrides." + target + " if the target's own command accepts one"
	}
	return "use agent_args_override." + string(name) + " if your build of the CLI accepts one"
}

// NativeArgs renders the argv fragment that expresses the profile for a harness
// whose knobs use MechanismArgs. rawArgs are the operator's agent_args_override
// flags: a knob those already pin natively is skipped, so the raw spelling
// keeps winning exactly as it did before this layer existed.
//
// Knobs on MechanismRequest and MechanismUnsupported contribute nothing here;
// request-carried knobs are read straight off the Profile by their adapter.
func NativeArgs(name types.AgentName, p Profile, rawArgs []string) []string {
	if p.IsZero() {
		return nil
	}
	h, ok := lookup(name)
	if !ok {
		return nil
	}
	var out []string
	for _, entry := range []struct {
		value string
		knob  knob
	}{
		{p.Model, h.model},
		{string(p.Effort), h.effort},
	} {
		if entry.value == "" || entry.knob.mechanism != MechanismArgs || entry.knob.args == nil {
			continue
		}
		if entry.knob.pinned != nil && entry.knob.pinned(rawArgs) {
			continue
		}
		out = append(out, entry.knob.args(entry.value)...)
	}
	return out
}
