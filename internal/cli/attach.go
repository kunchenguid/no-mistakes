package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/tui"
	"github.com/kunchenguid/no-mistakes/internal/update"
	"github.com/spf13/cobra"
)

// attachRun is the shared logic for attaching to a pipeline run. It's used by
// both the root command (bare `no-mistakes`) and the `attach` subcommand.
func attachRun(w io.Writer, runID string) error {
	p, d, err := openResources()
	if err != nil {
		return err
	}
	defer d.Close()

	// When no run ID is given, resolve the repo before starting the daemon
	// so we fail fast (and avoid orphan daemon processes) when not in a git repo.
	var repo *db.Repo
	if runID == "" {
		repo, err = findRepo(d)
		if err != nil {
			return err
		}
	}

	// Ensure daemon is running.
	if err := daemon.EnsureDaemon(p); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}

	// Connect to daemon.
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		return fmt.Errorf("connect to daemon: %w", err)
	}
	defer client.Close()

	var run *ipc.RunInfo
	var repoID string

	if runID != "" {
		// Fetch specific run.
		var result ipc.GetRunResult
		if err := client.Call(ipc.MethodGetRun, &ipc.GetRunParams{RunID: runID}, &result); err != nil {
			return fmt.Errorf("get run: %w", err)
		}
		run = result.Run
	} else {
		repoID = repo.ID

		// Detect current branch to prefer runs on the same branch.
		branch, _ := git.CurrentBranch(context.Background(), ".")

		// Get active run for this repo, preferring the current branch.
		var result ipc.GetActiveRunResult
		if err := client.Call(ipc.MethodGetActiveRun, &ipc.GetActiveRunParams{RepoID: repo.ID, Branch: branch}, &result); err != nil {
			return fmt.Errorf("get active run: %w", err)
		}
		run = result.Run
	}

	if run == nil {
		printNoActiveRun(w, d, repoID)
		return nil
	}

	return tui.Run(p.Socket(), client, run, update.CachedLatestVersion())
}

const recentRunsLimit = 5

func printNoActiveRun(w io.Writer, d *db.DB, repoID string) {
	if repoID != "" {
		runs, err := d.GetRunsByRepo(repoID)
		if err == nil && len(runs) > 0 {
			shown := runs
			if len(shown) > recentRunsLimit {
				shown = shown[:recentRunsLimit]
			}
			fmt.Fprintf(w, "  %s\n", sDim.Render("No active run."))
			fmt.Fprintln(w)
			fmt.Fprintf(w, "  %s\n", sCyan.Render("Recent runs"))
			for _, r := range shown {
				sha := r.HeadSHA
				if len(sha) > 8 {
					sha = sha[:8]
				}
				age := formatAge(r.CreatedAt)
				pr := ""
				if r.PRURL != nil {
					pr = fmt.Sprintf("  %s", *r.PRURL)
				}
				fmt.Fprintf(w, "  %-12s %-20s %s  %s%s\n", runStatusStyle(r.Status), r.Branch, sDim.Render(sha), sDim.Render(age), pr)
			}
			if len(runs) > recentRunsLimit {
				fmt.Fprintf(w, "  %s\n", sDim.Render(fmt.Sprintf("(%d more - run 'no-mistakes runs' to see all)", len(runs)-recentRunsLimit)))
			}
			fmt.Fprintln(w)
			fmt.Fprintf(w, "  %s\n", sDim.Render("Start a new pipeline:"))
			fmt.Fprintf(w, "  %s\n", sBold.Render("git push no-mistakes <branch>"))
			return
		}
	}
	fmt.Fprintf(w, "  %s\n", sDim.Render("No active run. Push through the gate to start a pipeline:"))
	fmt.Fprintf(w, "  %s\n", sBold.Render("git push no-mistakes <branch>"))
}

func formatAge(unixSec int64) string {
	d := time.Since(time.Unix(unixSec, 0))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		if m == 1 {
			return "1 min ago"
		}
		return fmt.Sprintf("%d mins ago", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", h)
	default:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1 day ago"
		}
		return fmt.Sprintf("%d days ago", days)
	}
}

func newAttachCmd() *cobra.Command {
	var runID string

	cmd := &cobra.Command{
		Use:   "attach",
		Short: "Attach to the active pipeline run",
		Long: `Opens the TUI to monitor and interact with a pipeline run.
If no run ID is specified, attaches to the active run for the current repo.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return attachRun(cmd.OutOrStdout(), runID)
		},
	}

	cmd.Flags().StringVar(&runID, "run", "", "attach to a specific run ID")
	return cmd
}
