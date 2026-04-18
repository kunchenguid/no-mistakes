package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/spf13/cobra"
)

func newRunsCmd() *cobra.Command {
	var limit int

	cmd := &cobra.Command{
		Use:   "runs",
		Short: "List pipeline runs for the current repository",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommand("runs", func() error {
				_, d, err := openResources()
				if err != nil {
					return err
				}
				defer d.Close()

				repo, err := findRepo(d)
				if err != nil {
					return err
				}

				runs, err := d.GetRunsByRepo(repo.ID)
				if err != nil {
					return fmt.Errorf("list runs: %w", err)
				}

				w := cmd.OutOrStdout()

				if len(runs) == 0 {
					fmt.Fprintf(w, "  %s\n", sDim.Render("no runs yet. Push through the gate to start a pipeline:"))
					fmt.Fprintf(w, "  %s\n", sBold.Render("git push no-mistakes <branch>"))
					return nil
				}

				// Apply limit.
				shown := runs
				if limit > 0 && len(shown) > limit {
					shown = shown[:limit]
				}

				for _, r := range shown {
					printRunLine(w, r)
				}

				if len(runs) > len(shown) {
					fmt.Fprintf(w, "\n  %s\n", sDim.Render(fmt.Sprintf("(%d more runs, use --limit to see more)", len(runs)-len(shown))))
				}

				return nil
			})
		},
	}

	cmd.Flags().IntVar(&limit, "limit", 10, "maximum number of runs to display")
	return cmd
}

func printRunLine(w io.Writer, r *db.Run) {
	ts := time.Unix(r.CreatedAt, 0).Format("2006-01-02 15:04")
	sha := r.HeadSHA
	if len(sha) > 8 {
		sha = sha[:8]
	}
	pr := ""
	if r.PRURL != nil {
		pr = fmt.Sprintf("  %s", *r.PRURL)
	}
	fmt.Fprintf(w, "  %-12s %-20s %s  %s%s\n", runStatusStyle(r.Status), r.Branch, sDim.Render(sha), sDim.Render(ts), pr)
}
