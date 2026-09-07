package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/spf13/cobra"
)

func newRerunCmd() *cobra.Command {
	var intent string
	var baseBranch string
	cmd := &cobra.Command{
		Use:   "rerun",
		Short: "Rerun the pipeline for the current branch",
		Long:  "Rerun the pipeline for the current branch. By default, an explicit intent from the selected prior run is inherited; otherwise intent is inferred afresh. Use --intent to replace either with a new explicit intent. A per-run PR base branch is inherited from the selected prior run unless --base-branch is set. The selected run's pull-request URL is inherited when that PR is not already merged or closed, so retarget can prove identity.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("intent") && strings.TrimSpace(intent) == "" {
				return fmt.Errorf("--intent must not be empty")
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

				callerHead, err := rerunCallerHead(cmd.Context())
				if err != nil {
					return err
				}
				var result ipc.RerunResult
				if err := client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: repo.ID, Branch: branch, Intent: intent, PRBaseBranch: baseBranch, CallerHeadSHA: callerHead}, &result); err != nil {
					return fmt.Errorf("rerun pipeline: %w", err)
				}

				fmt.Fprintf(cmd.OutOrStdout(), "  %s Rerun started for %s %s\n", sGreen.Render("✓"), branch, sDim.Render(result.RunID))
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&intent, "intent", "", "explicit intent for this rerun (overrides inherited intent or fresh inference)")
	cmd.Flags().StringVar(&baseBranch, "base-branch", "", "integration branch for the PR for this rerun only (overrides inherited per-run base branch)")
	return cmd
}

// rerunCallerHead captures clean-head evidence at the request boundary, after
// any daemon startup or post-push wait. Dirty callers retain the existing
// behavior by omitting that evidence.
func rerunCallerHead(ctx context.Context) (string, error) {
	// Read both facts from one status observation: a subsequent HEAD lookup
	// could name a different commit whose index or worktree was never clean.
	out, err := git.Run(ctx, ".", "status", "--porcelain=v2", "--branch", "-z")
	if err != nil {
		return "", fmt.Errorf("check working tree: %w", err)
	}
	var head string
	for _, entry := range strings.Split(out, "\x00") {
		if oid, ok := strings.CutPrefix(entry, "# branch.oid "); ok {
			head = oid
		} else if entry != "" && !strings.HasPrefix(entry, "# ") {
			return "", nil
		}
	}
	if head == "" || head == "(initial)" {
		return "", fmt.Errorf("get caller head: no HEAD commit")
	}
	return head, nil
}
