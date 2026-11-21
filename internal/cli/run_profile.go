package cli

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/spf13/cobra"
)

func bindRunProfileFlags(cmd *cobra.Command, model, effort *string) {
	cmd.Flags().Var(&runSelectionFlag{value: model}, "model", "exact omp provider/model ID to pin for a new run (overrides agent_config.omp.model)")
	cmd.Flags().Var(&runSelectionFlag{value: effort}, "effort", "omp reasoning effort to pin: minimal, low, medium, high, xhigh, max (overrides agent_config.omp.effort)")
}

// Do not silently choose the last of two explicit dispatch selections.
type runSelectionFlag struct {
	value *string
	set   bool
}

func (f *runSelectionFlag) String() string { return *f.value }
func (f *runSelectionFlag) Type() string   { return "string" }
func (f *runSelectionFlag) Set(value string) error {
	if f.set {
		return fmt.Errorf("run selection flag must not be repeated")
	}
	*f.value, f.set = value, true
	return nil
}

func runProfileFromFlags(cmd *cobra.Command, model, effort string) (*agentcfg.RunProfile, error) {
	if !cmd.Flags().Changed("model") && !cmd.Flags().Changed("effort") {
		return nil, nil
	}
	if cmd.Flags().Changed("model") && model == "" || cmd.Flags().Changed("effort") && effort == "" {
		return nil, fmt.Errorf("--model and --effort cannot be explicitly empty")
	}
	p := &agentcfg.RunProfile{Model: model, Effort: agentcfg.Effort(effort)}
	return p, p.ValidateRequest()
}

const runProfilePushOptionPrefix = "no-mistakes.run-profile="

func formatRunProfilePushOptions(p *agentcfg.RunProfile) []string {
	if p == nil {
		return nil
	}
	b, _ := json.Marshal(p)
	return []string{runProfilePushOptionPrefix + base64.RawURLEncoding.EncodeToString(b)}
}

func parseRunProfilePushOptions(options []string) (*agentcfg.RunProfile, error) {
	var profile *agentcfg.RunProfile
	for _, option := range options {
		raw, ok := strings.CutPrefix(option, runProfilePushOptionPrefix)
		if !ok {
			continue
		}
		if profile != nil {
			return nil, fmt.Errorf("duplicate run profile push option")
		}
		if len(raw) > 1024 {
			return nil, fmt.Errorf("invalid run profile push option")
		}
		b, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid run profile push option")
		}
		profile = &agentcfg.RunProfile{}
		if err := json.Unmarshal(b, profile); err != nil {
			return nil, fmt.Errorf("invalid run profile push option")
		}
		if err := profile.ValidateRequest(); err != nil {
			return nil, err
		}
	}
	return profile, nil
}
