package cli

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

func bindRunAgentFlag(cmd *cobra.Command, name *string) {
	cmd.Flags().Var(&runAgentFlag{value: name}, "agent", "explicit backend for this run only; keeps global configuration unchanged")
}

type runAgentFlag struct {
	value *string
	set   bool
}

func (f *runAgentFlag) String() string { return *f.value }
func (f *runAgentFlag) Type() string   { return "string" }
func (f *runAgentFlag) Set(value string) error {
	if f.set || value == "" {
		return fmt.Errorf("--agent must be nonempty and supplied only once")
	}
	if err := agentcfg.ValidateRunAgent(types.AgentName(value)); err != nil {
		return err
	}
	*f.value, f.set = value, true
	return nil
}

const runAgentPushOptionPrefix = "no-mistakes.agent="

func formatRunAgentPushOptions(name types.AgentName) []string {
	if name == "" {
		return nil
	}
	return []string{runAgentPushOptionPrefix + string(name)}
}

func parseRunAgentPushOptions(options []string) (types.AgentName, error) {
	var name types.AgentName
	for _, option := range options {
		value, ok := strings.CutPrefix(option, runAgentPushOptionPrefix)
		if !ok {
			continue
		}
		if name != "" || value == "" {
			return "", fmt.Errorf("invalid or duplicate run agent push option")
		}
		name = types.AgentName(value)
		if err := agentcfg.ValidateRunAgent(name); err != nil {
			return "", err
		}
	}
	return name, nil
}

// An older daemon must refuse before a push, rather than ignore a selection
// field and spend quota on the globally configured backend.
func probeRunAgent(client *ipc.Client, name types.AgentName) error {
	if name == "" {
		return nil
	}
	var accepted types.AgentName
	if err := client.Call(ipc.MethodProbeRunAgent, name, &accepted); err != nil {
		return fmt.Errorf("daemon cannot honor --agent; update and restart it: %w", err)
	}
	if accepted != name {
		return fmt.Errorf("daemon did not confirm the requested agent")
	}
	return nil
}
