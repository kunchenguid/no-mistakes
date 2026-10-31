package cli

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

func bindReviewerFlags(cmd *cobra.Command) {
	for _, flag := range []struct{ name, help string }{
		{"reviewer", "explicit reviewer harness, pinned independently of the primary agent"},
		{"reviewer-model", "reviewer model (defaults to agent_config for its harness)"},
		{"reviewer-effort", "reviewer effort (defaults to agent_config for its harness)"},
	} {
		value := new(string)
		cmd.Flags().Var(&piSelectionFlag{value: value}, flag.name, flag.help)
	}
}

func reviewerFromFlags(cmd *cobra.Command) (*agentcfg.ReviewerProfile, error) {
	if cmd.Flags().Lookup("reviewer") == nil {
		return nil, nil
	}
	requested := false
	for _, name := range []string{"reviewer", "reviewer-model", "reviewer-effort"} {
		if cmd.Flags().Changed(name) {
			requested = true
			value, _ := cmd.Flags().GetString(name)
			if value == "" {
				return nil, fmt.Errorf("--%s cannot be explicitly empty", name)
			}
		}
	}
	if !requested {
		return nil, nil
	}
	name, _ := cmd.Flags().GetString("reviewer")
	model, _ := cmd.Flags().GetString("reviewer-model")
	effort, _ := cmd.Flags().GetString("reviewer-effort")
	p := &agentcfg.ReviewerProfile{Agent: types.AgentName(name), Model: model, Effort: agentcfg.Effort(effort)}
	return p, p.ValidateRequest()
}

func resolveReviewer(client *ipc.Client, repoID string, request *agentcfg.ReviewerProfile) (*agentcfg.ReviewerProfile, error) {
	if request == nil {
		return nil, nil
	}
	var resolved agentcfg.ReviewerProfile
	// Distinct RPC refuses old daemons before any push can take custody.
	if err := client.Call(ipc.MethodResolveReviewerProfile, &ipc.ResolveReviewerProfileParams{RepoID: repoID, ReviewerProfile: request}, &resolved); err != nil {
		return nil, fmt.Errorf("resolve reviewer selection: %w", err)
	}
	if err := resolved.Validate(); err != nil {
		return nil, err
	}
	if !resolved.Matches(request) {
		return nil, fmt.Errorf("daemon returned a different reviewer selection")
	}
	return &resolved, nil
}

const reviewerPushPrefix = "no-mistakes.reviewer-profile="

func formatReviewerPushOptions(p *agentcfg.ReviewerProfile) []string {
	if p == nil {
		return nil
	}
	b, _ := json.Marshal(p)
	return []string{reviewerPushPrefix + base64.RawURLEncoding.EncodeToString(b)}
}

func parseReviewerPushOptions(options []string) (*agentcfg.ReviewerProfile, error) {
	var p *agentcfg.ReviewerProfile
	for _, option := range options {
		raw, ok := strings.CutPrefix(option, reviewerPushPrefix)
		if !ok {
			continue
		}
		if p != nil || len(raw) > 1024 {
			return nil, fmt.Errorf("invalid or duplicate reviewer selection push option")
		}
		b, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid reviewer selection push option")
		}
		p = &agentcfg.ReviewerProfile{}
		if err := json.Unmarshal(b, p); err != nil {
			return nil, fmt.Errorf("invalid reviewer selection push option")
		}
		if err := p.Validate(); err != nil {
			return nil, err
		}
	}
	return p, nil
}
