package cli

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/spf13/cobra"
)

func bindPiProfileFlags(cmd *cobra.Command, model, effort *string) {
	cmd.Flags().Var(&piSelectionFlag{value: model}, "model", "exact Pi provider/model ID to pin for a new run (overrides agent_config.pi.model)")
	cmd.Flags().Var(&piSelectionFlag{value: effort}, "effort", "Pi reasoning effort to pin: minimal, low, medium, high, xhigh, max (overrides agent_config.pi.effort)")
}

// Do not silently choose the last of two explicit dispatch selections.
type piSelectionFlag struct {
	value *string
	set   bool
}

func (f *piSelectionFlag) String() string { return *f.value }
func (f *piSelectionFlag) Type() string   { return "string" }
func (f *piSelectionFlag) Set(value string) error {
	if f.set {
		return fmt.Errorf("Pi selection flag must not be repeated")
	}
	*f.value, f.set = value, true
	return nil
}

func piProfileFromFlags(cmd *cobra.Command, model, effort string) (*agentcfg.PiProfile, error) {
	if !cmd.Flags().Changed("model") && !cmd.Flags().Changed("effort") {
		return nil, nil
	}
	if cmd.Flags().Changed("model") && model == "" || cmd.Flags().Changed("effort") && effort == "" {
		return nil, fmt.Errorf("--model and --effort cannot be explicitly empty")
	}
	p := &agentcfg.PiProfile{Model: model, Effort: agentcfg.Effort(effort)}
	return p, p.ValidateRequest()
}

const piProfilePushOptionPrefix = "no-mistakes.pi-profile="

func formatPiProfilePushOptions(p *agentcfg.PiProfile) []string {
	if p == nil {
		return nil
	}
	b, _ := json.Marshal(p)
	return []string{piProfilePushOptionPrefix + base64.RawURLEncoding.EncodeToString(b)}
}

func parsePiProfilePushOptions(options []string) (*agentcfg.PiProfile, error) {
	var profile *agentcfg.PiProfile
	for _, option := range options {
		raw, ok := strings.CutPrefix(option, piProfilePushOptionPrefix)
		if !ok {
			continue
		}
		if profile != nil {
			return nil, fmt.Errorf("duplicate Pi profile push option")
		}
		if len(raw) > 1024 {
			return nil, fmt.Errorf("invalid Pi profile push option")
		}
		b, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid Pi profile push option")
		}
		profile = &agentcfg.PiProfile{}
		if err := json.Unmarshal(b, profile); err != nil {
			return nil, fmt.Errorf("invalid Pi profile push option")
		}
		if err := profile.ValidateRequest(); err != nil {
			return nil, err
		}
	}
	return profile, nil
}
