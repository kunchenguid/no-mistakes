package cli

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
	toon "github.com/toon-format/toon-go"
)

func newAxiHandoffCmd() *cobra.Command {
	return &cobra.Command{
		Use:           "handoff",
		Short:         "End green CI monitoring and hand the open PR to human reviewers",
		Long:          "End the current branch's CI monitor after checks-passed, recording handed_off instead of cancelled.\nThe PR stays open: this neither merges nor closes it. Pending checks, failures and approval gates\nmust be resolved first with axi run. Use axi abort only to discard a run.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackAxiSurface("axi-handoff", "/axi/handoff", nil, func() error {
				env, err := openAxiDaemonEnv()
				if err != nil {
					return emitError(cmd, 1, err.Error(), repoInitHelp(err)...)
				}
				defer env.close()
				branch, err := git.CurrentBranch(cmd.Context(), ".")
				if err != nil {
					return emitError(cmd, 1, fmt.Sprintf("get current branch: %v", err))
				}
				var active ipc.GetActiveRunResult
				if err := env.client.Call(ipc.MethodGetActiveRun, activeRunLookupParams(env.repo.ID, branch), &active); err != nil {
					return emitError(cmd, 1, fmt.Sprintf("get active run: %v", err))
				}
				if active.Run == nil {
					return emitError(cmd, 1, "no active CI monitor on this branch; use axi status to inspect the run")
				}
				var result ipc.CancelRunResult
				if err := env.client.Call(ipc.MethodHandoffRun, &ipc.CancelRunParams{RunID: active.Run.ID}, &result); err != nil {
					return emitError(cmd, 1, fmt.Sprintf("handoff run: %v", err))
				}
				final, confirmed, reason := waitForTerminalRun(cmd.Context(), env.client, active.Run.ID, abortStateWaitTimeout)
				if !confirmed {
					return emitError(cmd, 1, "handoff requested but terminal state unconfirmed: "+reason, "Use axi status to inspect the run")
				}
				if final.Status != types.RunHandedOff {
					return emitError(cmd, 1, fmt.Sprintf("handoff not recorded: run ended as %s; use axi status to inspect it", final.Status))
				}
				return renderDriveResult(cmd, final, false, toon.Field{Key: "handed_off", Value: true})
			})
		},
	}
}
