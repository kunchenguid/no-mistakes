package cli

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/spf13/cobra"
)

func newEjectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "eject",
		Short: "Remove no-mistakes gate from the current repository",
		Long: `Removes the "no-mistakes" git remote, deletes the bare repo and worktrees,
and removes the repo record from the database.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommand("eject", func() error {
				p, d, err := openResources()
				if err != nil {
					return err
				}
				defer d.Close()

				repo, err := gate.Eject(cmd.Context(), d, p, ".")
				if err != nil {
					return fmt.Errorf("eject: %w", err)
				}

				w := cmd.OutOrStdout()
				fmt.Fprintf(w, "  %s Gate removed\n", sGreen.Render("✓"))
				fmt.Fprintln(w)
				fmt.Fprintf(w, "  %s  %s\n", sDim.Render("  repo"), repo.WorkingPath)
				fmt.Fprintf(w, "  %s  %s\n", sDim.Render("remote"), repo.UpstreamURL)
				return nil
			})
		},
	}
}
