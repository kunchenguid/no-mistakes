// Package agentcfg is the harness-neutral agent tuning layer.
//
// This build supports exactly one harness, the native omp (Oh My Pi) CLI.
// A Profile states the model and reasoning effort once; NativeArgs maps it to
// omp's `--model` and `--thinking` flags. The layer stays separate from the
// adapter so config validation and eval replay share one mapping.
//
// Precedence is fixed: a raw agent_args_override flag that already pins a knob
// natively wins, and the mapped value for that knob is not emitted. This keeps
// every pre-existing configuration working unchanged and avoids handing omp the
// same knob twice.
package agentcfg

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Effort is the harness-neutral reasoning-depth knob. Values pass through to
// omp verbatim.
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

// EffortNames returns the canonical effort vocabulary as strings.
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

// Profile is the harness-neutral tuning surface. The zero Profile leaves omp on
// its own defaults.
type Profile struct {
	Model  string
	Effort Effort
}

// IsZero reports whether the profile asks for nothing.
func (p Profile) IsZero() bool { return p.Model == "" && p.Effort == "" }

// String renders the profile in the canonical comma-separated key=value form
// shared by eval candidates.
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
	// MechanismArgs means no-mistakes injects native CLI flags.
	MechanismArgs Mechanism = "args"
	// MechanismUnsupported means the harness cannot express the knob.
	MechanismUnsupported Mechanism = "unsupported"
)

// Agents returns every supported agent name. This build supports omp only.
func Agents() []types.AgentName {
	return []types.AgentName{types.AgentOMP}
}

// Known reports whether the mapping covers this agent name.
func Known(name types.AgentName) bool {
	return name == types.AgentOMP
}

// MechanismFor reports how omp expresses a knob.
func MechanismFor(name types.AgentName, k Knob) Mechanism {
	if !Known(name) {
		return MechanismUnsupported
	}
	return MechanismArgs
}

// Validate reports whether the supported harness can express every knob the
// profile sets. An unsupported agent or malformed effort is refused at
// config-load and at agent construction rather than silently dropped.
func Validate(name types.AgentName, p Profile) error {
	if p.IsZero() {
		return nil
	}
	if !Known(name) {
		if name == "" {
			return fmt.Errorf("agent is not configured; this build supports only %q", types.AgentOMP)
		}
		return fmt.Errorf("unsupported agent %q; this build supports only %q", name, types.AgentOMP)
	}
	if p.Effort != "" {
		if _, err := ParseEffort(string(p.Effort)); err != nil {
			return err
		}
	}
	return nil
}

// NativeArgs renders the argv fragment that expresses the profile for omp.
// rawArgs are the operator's agent_args_override flags: a knob those already
// pin natively is skipped, so the raw spelling keeps winning.
func NativeArgs(name types.AgentName, p Profile, rawArgs []string) []string {
	if p.IsZero() || !Known(name) {
		return nil
	}
	var out []string
	if p.Model != "" {
		if _, pinned := flagPinned("--model")(rawArgs); !pinned {
			out = append(out, "--model", p.Model)
		}
	}
	if p.Effort != "" {
		if _, pinned := flagPinned("--thinking")(rawArgs); !pinned {
			out = append(out, "--thinking", string(p.Effort))
		}
	}
	return out
}

// EffectiveModel reports the model omp will actually use, honoring a raw
// `--model` override over the configured profile.
func EffectiveModel(name types.AgentName, p Profile, rawArgs []string) string {
	if !Known(name) {
		return ""
	}
	if value, pinned := flagPinned("--model")(rawArgs); pinned {
		return value
	}
	return p.Model
}

// SplitProviderModel splits a provider/model spelling into its two parts.
func SplitProviderModel(model string) (provider, id string, ok bool) {
	provider, id, found := strings.Cut(strings.TrimSpace(model), "/")
	provider = strings.TrimSpace(provider)
	id = strings.TrimSpace(id)
	if !found || provider == "" || id == "" {
		return "", "", false
	}
	return provider, id, true
}

// ServedMatchesRequested reports whether the served model is the same model
// identity the operator requested. Identity is the final path segment of the
// model id; provider metadata is never compared.
func ServedMatchesRequested(requested, served, servedProvider string) bool {
	modelName := func(model string) string {
		model = strings.TrimSpace(model)
		if slash := strings.LastIndexByte(model, '/'); slash >= 0 {
			model = strings.TrimSpace(model[slash+1:])
		}
		return model
	}
	requestedName := modelName(requested)
	servedName := modelName(served)
	return requestedName != "" && requestedName == servedName
}

// flagPinned matches a flag in both `--flag value` and `--flag=value` forms.
func flagPinned(flags ...string) func([]string) (string, bool) {
	return func(rawArgs []string) (string, bool) {
		for i := len(rawArgs) - 1; i >= 0; i-- {
			arg := rawArgs[i]
			for _, flag := range flags {
				if arg == flag {
					if i+1 < len(rawArgs) {
						return rawArgs[i+1], true
					}
					return "", true
				}
				if value, found := strings.CutPrefix(arg, flag+"="); found {
					return value, true
				}
			}
		}
		return "", false
	}
}
