package agentcfg

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"strings"
)

// RunSelection carries an opt-in launch selection. Only this package defines
// selections, so callers cannot inject an unrecognized profile kind.
type RunSelection interface{ runSelection() }

func (*RunProfile) runSelection() {}

// RunProfile is an opt-in per-run model/effort pin for omp. It is not a routing
// policy: every pipeline step still uses the same omp harness. A request may
// omit one field; a persisted profile must have both resolved.
type RunProfile struct {
	Model  string `json:"model"`
	Effort Effort `json:"effort"`
}

// ValidateRequest accepts only identifier-shaped, provider-qualified models.
func (p *RunProfile) ValidateRequest() error {
	if p == nil {
		return nil
	}
	if p.Model == "" && p.Effort == "" {
		return fmt.Errorf("run profile requires model or effort")
	}
	if p.Model != "" {
		parts := strings.Split(p.Model, "/")
		if len(parts) < 2 || len(p.Model) > 256 {
			return fmt.Errorf("run model must be an exact provider/model ID")
		}
		for _, part := range parts {
			if part == "" || part == "." || part == ".." || part[0] == '-' {
				return fmt.Errorf("run model must be an exact provider/model ID")
			}
			for _, c := range part {
				if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._-", c)) {
					return fmt.Errorf("run model must be an exact provider/model ID (no URL, pattern, or thinking suffix)")
				}
			}
		}
	}
	if p.Effort != "" {
		effort, err := ParseEffort(string(p.Effort))
		if err != nil || effort != p.Effort {
			return fmt.Errorf("invalid run effort (valid: %s)", strings.Join(EffortNames(), ", "))
		}
	}
	return nil
}

func (p *RunProfile) Validate() error {
	if p == nil {
		return nil
	}
	if err := p.ValidateRequest(); err != nil {
		return err
	}
	if p.Model == "" || p.Effort == "" {
		return fmt.Errorf("run pin requires both model and effort; supply flags or agent_config.omp defaults")
	}
	return nil
}

// Matches permits omission on reattachment, never replacement. Partial requests
// compare only their explicit knobs against the already-resolved run values.
func (p *RunProfile) Matches(request *RunProfile) bool {
	return request == nil || p != nil && (request.Model == "" || request.Model == p.Model) && (request.Effort == "" || request.Effort == p.Effort)
}

func (p RunProfile) Value() (driver.Value, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(p)
	return string(b), err
}

func (p *RunProfile) Scan(src any) error {
	var b []byte
	switch v := src.(type) {
	case string:
		b = []byte(v)
	case []byte:
		b = v
	default:
		return fmt.Errorf("invalid persisted run profile")
	}
	if err := json.Unmarshal(b, p); err != nil {
		return fmt.Errorf("invalid persisted run profile")
	}
	return p.Validate()
}

// OptionalRunProfile keeps existing internal launch callers source compatible.
func OptionalRunProfile(profiles []RunSelection) *RunProfile {
	for _, profile := range profiles {
		if p, ok := profile.(*RunProfile); ok && p != nil {
			return p
		}
	}
	return nil
}

// SelectionArgs strips only native selection knobs. Used when applying a
// durable pin so a later raw override cannot suppress NativeArgs on recovery.
func SelectionArgs(args []string) (remaining []string, found bool) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		flag, _, inline := strings.Cut(arg, "=")
		switch flag {
		case "--model", "--provider", "--thinking", "--models":
			found = true
			if !inline && i+1 < len(args) {
				i++
			}
		default:
			remaining = append(remaining, arg)
		}
	}
	return remaining, found
}
