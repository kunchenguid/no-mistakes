package cli

import (
	"context"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/spf13/cobra"
)

func newRerunCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rerun",
		Short: "Rerun the pipeline for the current branch",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			p, d, err := openResources()
			if err != nil {
				return err
			}
			defer d.Close()

			gitRoot, err := git.FindGitRoot(".")
			if err != nil {
				return fmt.Errorf("not in a git repository")
			}

			repo, err := d.GetRepoByPath(gitRoot)
			if err != nil {
				return fmt.Errorf("get repo: %w", err)
			}
			if repo == nil {
				return fmt.Errorf("repo not initialized (run 'no-mistakes init' first)")
			}

			branch, err := git.CurrentBranch(context.Background(), gitRoot)
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
			if err := client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: repo.ID, Branch: branch}, &result); err != nil {
				return fmt.Errorf("rerun pipeline: %w", err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "rerun started for %s: %s\n", branch, result.RunID)
			return nil
		},
	}
}
