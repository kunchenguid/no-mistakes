package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/tui"
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
		// Find repo from current directory.
		repo, err := findRepo(d)
		if err != nil {
			return err
		}
		repoID = repo.ID

		// Get active run for this repo.
		var result ipc.GetActiveRunResult
		if err := client.Call(ipc.MethodGetActiveRun, &ipc.GetActiveRunParams{RepoID: repo.ID}, &result); err != nil {
			return fmt.Errorf("get active run: %w", err)
		}
		run = result.Run
	}

	if run == nil {
		printNoActiveRun(w, d, repoID)
		return nil
	}

	return tui.Run(p.Socket(), client, run)
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
			fmt.Fprintln(w, "No active run.")
			fmt.Fprintln(w)
			fmt.Fprintln(w, "Recent runs:")
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
				fmt.Fprintf(w, "  %-12s %-20s %s  %s%s\n", r.Status, r.Branch, sha, age, pr)
			}
			if len(runs) > recentRunsLimit {
				fmt.Fprintf(w, "  (%d more — run 'no-mistakes runs' to see all)\n", len(runs)-recentRunsLimit)
			}
			fmt.Fprintln(w)
			fmt.Fprintln(w, "Start a new pipeline:")
			fmt.Fprintln(w, "  git push no-mistakes <branch>")
			return
		}
	}
	fmt.Fprintln(w, "No active run. Push through the gate to start a pipeline:")
	fmt.Fprintln(w, "  git push no-mistakes <branch>")
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
