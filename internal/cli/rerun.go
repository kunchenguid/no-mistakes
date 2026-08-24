package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

func newRerunCmd() *cobra.Command {
	var intent string
	var agentName string
	var model string
	cmd := &cobra.Command{
		Use:   "rerun",
		Short: "Rerun the pipeline for the current branch",
		Long: "Rerun the pipeline for the current branch. By default, an explicit intent from the selected prior run is inherited; otherwise intent is inferred afresh. Use --intent to replace either with a new explicit intent. " +
			"Agent/model selection is not inherited: omission resolves the current configured defaults for the new run; use --agent and optionally --model for a new operator override.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("intent") && strings.TrimSpace(intent) == "" {
				return fmt.Errorf("--intent must not be empty")
			}
			if cmd.Flags().Changed("agent") && strings.TrimSpace(agentName) == "" {
				return fmt.Errorf("--agent must not be empty")
			}
			if cmd.Flags().Changed("model") && strings.TrimSpace(model) == "" {
				return fmt.Errorf("--model must not be empty")
			}
			selectionAgent := types.AgentName(agentName)
			if err := agentcfg.ValidateRunOverride(selectionAgent, model); err != nil {
				return err
			}
			return trackCommand("rerun", func() error {
				p, d, err := openResources()
				if err != nil {
					return err
				}
				defer d.Close()

				repo, err := findRepo(d)
				if err != nil {
					return err
				}

				branch, err := git.CurrentBranch(context.Background(), ".")
				if err != nil {
					return fmt.Errorf("get current branch: %w", err)
				}
				if branch == "HEAD" {
					return fmt.Errorf("not on a branch")
				}

				if err := daemon.EnsureDaemon(p); err != nil {
					return fmt.Errorf("start daemon: %w", err)
				}

				client, err := ipc.Dial(p.Socket())
				if err != nil {
					return fmt.Errorf("connect to daemon: %w", err)
				}
				defer client.Close()

				var result ipc.RerunResult
				if err := client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: repo.ID, Branch: branch, Intent: intent, Agent: selectionAgent, Model: model}, &result); err != nil {
					return fmt.Errorf("rerun pipeline: %w", err)
				}

				fmt.Fprintf(cmd.OutOrStdout(), "  %s Rerun started for %s %s\n", sGreen.Render("✓"), branch, sDim.Render(result.RunID))
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&intent, "intent", "", "explicit intent for this rerun (overrides inherited intent or fresh inference)")
	cmd.Flags().StringVar(&agentName, "agent", "", "pipeline agent for the new rerun (omitted: resolve current configured defaults)")
	cmd.Flags().StringVar(&model, "model", "", "model for this rerun's --agent (only where the selected agent supports model pinning)")
	return cmd
}
