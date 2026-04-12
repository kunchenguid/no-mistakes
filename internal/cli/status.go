package cli

import (
	"fmt"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/spf13/cobra"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show status of the current repository",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			p, d, err := openResources()
			if err != nil {
				return err
			}
			defer d.Close()

			w := cmd.OutOrStdout()

			// Look up repo from current directory.
			repo, err := findRepo(d)
			if err != nil {
				fmt.Fprintln(w, err)
				return nil
			}

			fmt.Fprintf(w, "repo:     %s\n", repo.WorkingPath)
			fmt.Fprintf(w, "upstream: %s\n", repo.UpstreamURL)
			fmt.Fprintf(w, "gate:     %s\n", p.RepoDir(repo.ID))

			// Check daemon status.
			alive, _ := daemon.IsRunning(p)
			if alive {
				fmt.Fprintf(w, "daemon:   running\n")
			} else {
				fmt.Fprintf(w, "daemon:   stopped\n")
			}

			// Check for active run.
			activeRun, err := d.GetActiveRun(repo.ID)
			if err != nil {
				return fmt.Errorf("check active run: %w", err)
			}
			if activeRun != nil {
				fmt.Fprintf(w, "\nactive run:\n")
				fmt.Fprintf(w, "  id:     %s\n", activeRun.ID)
				fmt.Fprintf(w, "  branch: %s\n", activeRun.Branch)
				fmt.Fprintf(w, "  status: %s\n", activeRun.Status)
				fmt.Fprintf(w, "  head:   %s\n", activeRun.HeadSHA[:minLen(len(activeRun.HeadSHA), 8)])
				fmt.Fprintf(w, "  started: %s\n", time.Unix(activeRun.CreatedAt, 0).Format(time.DateTime))
			} else {
				fmt.Fprintf(w, "\nno active run\n")
			}

			return nil
		},
	}
}

func minLen(a, b int) int {
	if a < b {
		return a
	}
	return b
}
