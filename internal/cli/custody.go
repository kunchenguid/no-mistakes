package cli

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/spf13/cobra"
	toon "github.com/toon-format/toon-go"
)

func newCustodyCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "custody", Short: "Release a published branch without losing owned heads"}
	for _, action := range []string{"release", "reconcile"} {
		sub := newCustodyOperationCmd(action)
		if action == "release" {
			sub.Short = "Archive owned heads and return custody at the exact existing PR head"
		} else {
			sub.Short = "Reconcile a historical daemon restart failure at the published PR head"
		}
		cmd.AddCommand(sub)
	}
	return cmd
}

func newPublicationCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "publication", Short: "Manage an existing-PR publication destination"}
	cmd.AddCommand(newCustodyOperationCmd("rebind"))
	return cmd
}

func newCustodyOperationCmd(action string) *cobra.Command {
	var runID, branch string
	cmd := &cobra.Command{
		Use: action, Args: cobra.NoArgs, SilenceErrors: true, SilenceUsage: true,
		Short: "Bind a parked live run to an existing PR branch, with append-only publication",
		Long: "Operate on the selected run from its registered checked-out custody branch.\n" +
			"--run is required to fence the operation to the intended generation. Release\n" +
			"and reconcile require a clean caller whose exact HEAD is already the open PR\n" +
			"head on the configured push target. They archive every available owned head\n" +
			"before restoring the gate lane; missing unpublished heads and dirty managed\n" +
			"worktrees refuse. Reconcile additionally requires a historical daemon lifecycle\n" +
			"failure. Rebind accepts an existing open PR branch whose head is an ancestor\n" +
			"of the managed head. The run must be live and parked at an approval gate. Rebound\n" +
			"publication never creates a branch or rewrites history. No operation pushes,\n" +
			"restarts the daemon, switches branches, or changes caller files.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(runID) == "" {
				return emitError(cmd, 2, "--run is required")
			}
			if action == "rebind" && strings.TrimSpace(branch) == "" {
				return emitError(cmd, 2, "--branch is required")
			}
			env, err := openAxiDaemonEnv()
			if err != nil {
				return emitError(cmd, 1, err.Error())
			}
			defer env.close()
			root, err := git.FindGitRoot(".")
			if err != nil {
				return emitError(cmd, 1, err.Error())
			}
			root, err = filepath.Abs(root)
			if err != nil {
				return emitError(cmd, 1, err.Error())
			}
			head, err := git.HeadSHA(cmd.Context(), root)
			if err != nil {
				return emitError(cmd, 1, err.Error())
			}
			var result ipc.CustodyOperationResult
			request := &ipc.CustodyOperationParams{Action: action, RepoID: env.repo.ID, RunID: runID, WorkDir: root, HeadSHA: head, PublicationBranch: branch}
			if err := env.client.CallWithContext(cmd.Context(), ipc.MethodCustodyOperation, request, &result, env.cfg.BranchSyncRemoteTimeout*8); err != nil {
				return emitError(cmd, 1, fmt.Sprintf("%s refused: %v", action, err))
			}
			emitDoc(cmd, toon.Field{Key: "custody_operation", Value: map[string]any{
				"run_id": result.RunID, "state": result.State, "branch": result.Branch, "head_sha": result.HeadSHA, "pr_url": result.PRURL,
			}})
			return nil
		},
	}
	cmd.Flags().StringVar(&runID, "run", "", "exact current custody run to operate on (required)")
	if action == "rebind" {
		cmd.Flags().StringVar(&branch, "branch", "", "existing open PR branch on the configured push target (required)")
	}
	return cmd
}
