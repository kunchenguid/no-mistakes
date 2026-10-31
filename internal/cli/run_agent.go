package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/spf13/cobra"
)

const runAgentPushOptionPrefix = "no-mistakes.run-agent="

func runAgentFromFlags(cmd *cobra.Command) (agentcfg.RunAgent, error) {
	required, set := os.LookupEnv("NM_REQUIRED_RUN_AGENT")
	if set && required != string(agentcfg.RunAgentCodex) {
		return "", fmt.Errorf("NM_REQUIRED_RUN_AGENT requires codex")
	}
	selection := agentcfg.RunAgent(required)
	if cmd.Flags().Changed("agent") {
		value, _ := cmd.Flags().GetString("agent")
		if value == "" {
			return "", fmt.Errorf("--agent requires codex")
		}
		if selection != "" && value != required {
			return "", fmt.Errorf("--agent conflicts with NM_REQUIRED_RUN_AGENT")
		}
		selection = agentcfg.RunAgent(value)
	}
	if selection != "" {
		if err := selection.Validate(); err != nil {
			return "", err
		}
	}
	if selection != "" && (cmd.Flags().Changed("model") || cmd.Flags().Changed("effort")) {
		return "", fmt.Errorf("--agent cannot be combined with Pi --model or --effort")
	}
	return selection, nil
}

func formatRunAgentPushOptions(selection agentcfg.RunAgent) []string {
	if selection == "" {
		return nil
	}
	return []string{runAgentPushOptionPrefix + string(selection)}
}

func parseRunAgentPushOptions(options []string) (agentcfg.RunAgent, error) {
	var selection agentcfg.RunAgent
	seen := false
	for _, option := range options {
		value, ok := strings.CutPrefix(option, runAgentPushOptionPrefix)
		if !ok {
			continue
		}
		if seen {
			return "", fmt.Errorf("duplicate run agent push option")
		}
		seen = true
		selection = agentcfg.RunAgent(value)
		if selection == "" || selection.Validate() != nil {
			return "", fmt.Errorf("invalid run agent push option")
		}
	}
	return selection, nil
}
