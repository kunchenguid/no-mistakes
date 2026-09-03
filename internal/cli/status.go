package cli

import (
	"fmt"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/spf13/cobra"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show status of the current repository",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackReadSurface("status", nil, func() (string, string, error) {
				p, d, err := openResources()
				if err != nil {
					return "", "", err
				}
				defer d.Close()

				w := cmd.OutOrStdout()

				// Look up repo from current directory.
				repo, err := findRepo(d)
				if err != nil {
					fmt.Fprintln(w, err)
					return "uninitialized", "error", nil
				}

				fmt.Fprintf(w, "  %s  %s\n", sDim.Render("  repo:"), repo.WorkingPath)
				remoteURL := repo.UpstreamURL
				if repo.ForkURL != "" {
					remoteURL = safeurl.Redact(remoteURL)
				}
				fmt.Fprintf(w, "  %s  %s\n", sDim.Render("remote:"), remoteURL)
				if repo.ForkURL != "" {
					fmt.Fprintf(w, "  %s  %s\n", sDim.Render("  fork:"), safeurl.Redact(repo.ForkURL))
				}
				fmt.Fprintf(w, "  %s  %s\n", sDim.Render("  gate:"), p.RepoDir(repo.ID))

				// Check daemon status.
				alive, _ := daemon.IsRunning(p)
				daemonState := "stopped"
				if alive {
					daemonState = "running"
				}
				if alive {
					fmt.Fprintf(w, "  %s  %s %s\n", sDim.Render("daemon:"), sGreen.Render("●"), "running")
				} else {
					fmt.Fprintf(w, "  %s  %s %s\n", sDim.Render("daemon:"), sDim.Render("○"), "stopped")
				}

				// Check for active run.
				activeRun, err := d.GetActiveRun(repo.ID, "")
				if err != nil {
					return "", "", fmt.Errorf("check active run: %w", err)
				}
				syncState := (&branchsync.Service{DB: d, Repo: repo, WorkDir: "."}).InspectCached(cmd.Context())
				cachedSummary := cachedBranchSummary(syncState)
				fingerprint := statusFingerprint(repo.ID, daemonState, activeRun, cachedSummary)
				fmt.Fprintf(w, "\n  %s  %s\n", sDim.Render("local state:"), cachedSummary)
				if activeRun != nil {
					fmt.Fprintln(w)
					fmt.Fprintf(w, "  %s\n", sCyan.Render("Active run"))
					sha := activeRun.HeadSHA[:minLen(len(activeRun.HeadSHA), 8)]
					ts := time.Unix(activeRun.CreatedAt, 0).Format(time.DateTime)
					fmt.Fprintf(w, "  %s  %s\n", sDim.Render("     id:"), activeRun.ID)
					fmt.Fprintf(w, "  %s  %s\n", sDim.Render(" branch:"), activeRun.Branch)
					fmt.Fprintf(w, "  %s  %s\n", sDim.Render(" status:"), runStatusStyle(activeRun.Status))
					fmt.Fprintf(w, "  %s  %s\n", sDim.Render("   head:"), sDim.Render(sha))
					fmt.Fprintf(w, "  %s  %s\n", sDim.Render("started:"), sDim.Render(ts))
				} else {
					fmt.Fprintf(w, "\n  %s\n", sDim.Render("no active run"))
				}

				return fingerprint, "success", nil
			})
		},
	}
}

func statusFingerprint(repoID, daemonState string, activeRun *db.Run, cachedSummary string) string {
	base := repoID + "|" + daemonState + "|" + cachedSummary
	if activeRun == nil {
		return base + "|idle"
	}
	return fmt.Sprintf("%s|%s:%s:%s:%s", base, activeRun.ID, activeRun.Branch, activeRun.Status, activeRun.HeadSHA)
}

func cachedBranchSummary(state branchsync.State) string {
	summary := humanSyncSummary(state)
	if state.Local.Branch == "" || state.Local.Head == "" {
		return "cached: unavailable (" + summary + ")"
	}

	head := state.Local.Head[:minLen(len(state.Local.Head), 8)]
	if state.Local.Reason == "status_unavailable" {
		if state.State == branchsync.StateDirty {
			summary = "local branch status is unavailable"
		}
		return fmt.Sprintf("cached: %s %s (cleanliness unavailable: run `git status`; %s)", state.Local.Branch, head, summary)
	}
	cleanliness := "clean"
	if !state.Local.Clean {
		cleanliness = "dirty"
		if state.Local.Reason != "" {
			cleanliness += ": " + state.Local.Reason
		}
	}

	return fmt.Sprintf("cached: %s %s (%s; %s)", state.Local.Branch, head, cleanliness, summary)
}

func minLen(a, b int) int {
	if a < b {
		return a
	}
	return b
}
