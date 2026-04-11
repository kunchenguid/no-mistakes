package cli

import (
	"fmt"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/spf13/cobra"
)

func newRunsCmd() *cobra.Command {
	var limit int

	cmd := &cobra.Command{
		Use:   "runs",
		Short: "List pipeline runs for the current repository",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, d, err := openResources()
			if err != nil {
				return err
			}
			defer d.Close()

			gitRoot, err := git.FindGitRoot(".")
			if err != nil {
				return fmt.Errorf("not in a git repository")
			}

			repo, err := d.GetRepoByPath(gitRoot)
			if err != nil || repo == nil {
				return fmt.Errorf("repo not initialized (run 'no-mistakes init' first)")
			}

			runs, err := d.GetRunsByRepo(repo.ID)
			if err != nil {
				return fmt.Errorf("list runs: %w", err)
			}

			w := cmd.OutOrStdout()

			if len(runs) == 0 {
				fmt.Fprintln(w, "no runs yet. Push through the gate to start a pipeline:")
				fmt.Fprintln(w, "  git push no-mistakes <branch>")
				return nil
			}

			// Apply limit.
			shown := runs
			if limit > 0 && len(shown) > limit {
				shown = shown[:limit]
			}

			for _, r := range shown {
				ts := time.Unix(r.CreatedAt, 0).Format("2006-01-02 15:04")
				sha := r.HeadSHA
				if len(sha) > 8 {
					sha = sha[:8]
				}
				status := string(r.Status)
				pr := ""
				if r.PRURL != nil {
					pr = fmt.Sprintf("  %s", *r.PRURL)
				}
				fmt.Fprintf(w, "%-12s %-20s %s  %s%s\n", status, r.Branch, sha, ts, pr)
			}

			if len(runs) > len(shown) {
				fmt.Fprintf(w, "\n(%d more runs, use --limit to see more)\n", len(runs)-len(shown))
			}

			return nil
		},
	}

	cmd.Flags().IntVar(&limit, "limit", 10, "maximum number of runs to display")
	return cmd
}
