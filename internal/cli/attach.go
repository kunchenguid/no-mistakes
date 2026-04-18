package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/tui"
	"github.com/kunchenguid/no-mistakes/internal/update"
	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
)

// attachRun is the shared logic for attaching to a pipeline run. It's used by
// both the root command (bare `no-mistakes`) and the `attach` subcommand.
func attachRun(w io.Writer, runID string, rootDefault bool) error {
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
	var state *repoState

	if runID != "" {
		// Fetch specific run.
		var result ipc.GetRunResult
		if err := client.Call(ipc.MethodGetRun, &ipc.GetRunParams{RunID: runID}, &result); err != nil {
			return fmt.Errorf("get run: %w", err)
		}
		run = result.Run
	} else {
		repoID = repo.ID

		// Detect current state so we can decide between attach and wizard
		// consistently with what the wizard itself will see.
		state, err = detectRepoState(context.Background(), repo)
		if err != nil {
			return err
		}

		// Skip the active-run check entirely when the state clearly calls
		// for the wizard (detached HEAD, or default branch with pending
		// changes) — pipelines for the user's current situation don't
		// match any existing run.
		if !state.shouldRouteToWizard() {
			var result ipc.GetActiveRunResult
			if err := client.Call(ipc.MethodGetActiveRun, &ipc.GetActiveRunParams{RepoID: repo.ID, Branch: activeRunBranch(state, rootDefault)}, &result); err != nil {
				return fmt.Errorf("get active run: %w", err)
			}
			run = result.Run
		}
	}

	if run == nil {
		// No active run — if the user ran bare `no-mistakes` in their repo
		// from a TTY, offer the interactive setup wizard instead of just
		// dumping a hint. Skip the wizard in non-interactive contexts
		// (tests, CI, piped output) and fall back to the old behavior.
		if rootDefault && runID == "" && repo != nil && state != nil && isInteractive() {
			res, wErr := runWizard(context.Background(), p, state)
			if wErr != nil {
				return wErr
			}
			if res.Success {
				run, err = waitForActiveRun(client, repo.ID, res.TargetBranch, 5*time.Second)
				if err != nil {
					return fmt.Errorf("wait for active run: %w", err)
				}
			}
			if run == nil {
				if !res.Aborted {
					printNoActiveRun(w, d, repoID)
				}
				return nil
			}
		} else {
			printNoActiveRun(w, d, repoID)
			return nil
		}
	}

	return tui.Run(p.Socket(), client, run, update.CachedLatestVersion())
}

func activeRunBranch(state *repoState, rootDefault bool) string {
	if rootDefault {
		return state.currentBranch
	}
	return ""
}

// isInteractive reports whether stdin and stdout are both connected to a
// terminal. The wizard needs a real TTY to read keystrokes; in non-interactive
// contexts we fall back to printing hints.
func isInteractive() bool {
	return isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd())
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
			return trackCommand("attach", func() error {
				return attachRun(cmd.OutOrStdout(), runID, false)
			})
		},
	}

	cmd.Flags().StringVar(&runID, "run", "", "attach to a specific run ID")
	return cmd
}
