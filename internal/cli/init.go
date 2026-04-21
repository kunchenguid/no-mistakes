package cli

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/spf13/cobra"
)

const banner = `_  _ ____    _  _ _ ____ ___ ____ _  _ ____ ____
|\ | |  |    |\/| | [__   |  |__| |_/  |___ [__
| \| |__|    |  | | ___]  |  |  | | \_ |___ ___]`

func newInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Initialize no-mistakes gate for the current repository",
		Long: `Sets up a local bare repo as a gate, installs a post-receive hook,
isolates the gate hook path from shared local git config writes, adds a
"no-mistakes" git remote, and records the repo in the database.

Run this from inside a git repository that has an "origin" remote.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommand("init", func() error {
				p, d, err := openResources()
				if err != nil {
					return err
				}
				defer d.Close()

				repo, err := gate.Init(cmd.Context(), d, p, ".")
				if err != nil {
					return fmt.Errorf("init: %w", err)
				}
				if err := daemon.EnsureDaemon(p); err != nil {
					if _, ejectErr := gate.Eject(cmd.Context(), d, p, "."); ejectErr != nil {
						return fmt.Errorf("start daemon: %w, rollback init: %v", err, ejectErr)
					}
					return fmt.Errorf("start daemon: %w", err)
				}

				w := cmd.OutOrStdout()
				fmt.Fprintln(w, sCyan.Render(banner))
				fmt.Fprintln(w)
				fmt.Fprintf(w, "  %s Gate initialized\n", sGreen.Render("✓"))
				fmt.Fprintln(w)
				fmt.Fprintf(w, "  %s  %s\n", sDim.Render("  repo"), repo.WorkingPath)
				fmt.Fprintf(w, "  %s  no-mistakes → %s\n", sDim.Render("  gate"), p.RepoDir(repo.ID))
				fmt.Fprintf(w, "  %s  %s\n", sDim.Render("remote"), repo.UpstreamURL)
				fmt.Fprintln(w)
				fmt.Fprintf(w, "  %s\n", sDim.Render("Push through the gate with:"))
				fmt.Fprintf(w, "  %s\n", sBold.Render("git push no-mistakes <branch>"))
				return nil
			})
		},
	}
}
