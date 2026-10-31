package agentcfg

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// RunSelection carries independent, opt-in launch selections. Only this package
// defines selections, so callers cannot inject an unrecognized profile kind.
type RunSelection interface{ runSelection() }

func (*PiProfile) runSelection()       {}
func (*ReviewerProfile) runSelection() {}

// inheritedReviewer can only be constructed by an internal continuation path,
// never by an IPC request. Its values already came from an immutable run row.
type inheritedReviewer struct{ profile *ReviewerProfile }

func (inheritedReviewer) runSelection() {}

func InheritReviewerProfile(profile *ReviewerProfile) RunSelection {
	return inheritedReviewer{profile: profile}
}

func ReviewerProfileInherited(profiles []RunSelection) bool {
	for _, profile := range profiles {
		if p, ok := profile.(inheritedReviewer); ok && p.profile != nil {
			return true
		}
	}
	return false
}

// ReviewerProfile pins the reviewer only. A request may inherit tuning from
// agent_config; persisted values always contain the resolved model and effort.
type ReviewerProfile struct {
	Agent  types.AgentName `json:"agent"`
	Model  string          `json:"model"`
	Effort Effort          `json:"effort"`
}

func (p *ReviewerProfile) ValidateRequest() error {
	if p == nil {
		return nil
	}
	if !Known(p.Agent) {
		return fmt.Errorf("reviewer requires an explicit supported harness")
	}
	if len(p.Model) > 256 {
		return fmt.Errorf("reviewer model must be an exact model identifier")
	}
	for _, c := range p.Model {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._-/", c)) {
			return fmt.Errorf("reviewer model must be an exact model identifier")
		}
	}
	if strings.HasPrefix(p.Model, "-") {
		return fmt.Errorf("reviewer model must be an exact model identifier")
	}
	if p.Effort != "" {
		effort, err := ParseEffort(string(p.Effort))
		if err != nil || effort != p.Effort {
			return fmt.Errorf("invalid reviewer effort")
		}
	}
	if p.Agent == types.AgentPi && (p.Model != "" || p.Effort != "") {
		if err := (&PiProfile{Model: p.Model, Effort: p.Effort}).ValidateRequest(); err != nil {
			return err
		}
	}
	return Validate(p.Agent, Profile{Model: p.Model, Effort: p.Effort})
}

func (p *ReviewerProfile) Validate() error {
	if p == nil {
		return nil
	}
	if err := p.ValidateRequest(); err != nil {
		return err
	}
	if p.Model == "" || p.Effort == "" {
		return fmt.Errorf("reviewer selection requires model and effort, explicitly or from agent_config")
	}
	return nil
}

func (p *ReviewerProfile) Matches(request *ReviewerProfile) bool {
	return request == nil || p != nil && p.Agent == request.Agent && (request.Model == "" || p.Model == request.Model) && (request.Effort == "" || p.Effort == request.Effort)
}

func (p ReviewerProfile) Value() (driver.Value, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(p)
	return string(b), err
}

func (p *ReviewerProfile) Scan(src any) error {
	var b []byte
	switch v := src.(type) {
	case string:
		b = []byte(v)
	case []byte:
		b = v
	default:
		return fmt.Errorf("invalid persisted reviewer selection")
	}
	if err := json.Unmarshal(b, p); err != nil {
		return fmt.Errorf("invalid persisted reviewer selection")
	}
	return p.Validate()
}

func OptionalReviewerProfile(profiles []RunSelection) *ReviewerProfile {
	for _, profile := range profiles {
		if p, ok := profile.(inheritedReviewer); ok && p.profile != nil {
			return p.profile
		}
		if p, ok := profile.(*ReviewerProfile); ok && p != nil {
			return p
		}
	}
	return nil
}
