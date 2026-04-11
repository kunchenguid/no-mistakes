package cli

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/spf13/cobra"
)

func newInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Initialize no-mistakes gate for the current repository",
		Long: `Sets up a local bare repo as a gate, installs a post-receive hook,
adds a "no-mistakes" git remote, and records the repo in the database.

Run this from inside a git repository that has an "origin" remote.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
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
				if ejectErr := gate.Eject(cmd.Context(), d, p, "."); ejectErr != nil {
					return fmt.Errorf("start daemon: %w, rollback init: %v", err, ejectErr)
				}
				return fmt.Errorf("start daemon: %w", err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "initialized gate for %s\n", repo.WorkingPath)
			fmt.Fprintf(cmd.OutOrStdout(), "  remote: no-mistakes → %s\n", p.RepoDir(repo.ID))
			fmt.Fprintf(cmd.OutOrStdout(), "  upstream: %s\n", repo.UpstreamURL)
			fmt.Fprintf(cmd.OutOrStdout(), "\nPush through the gate with: git push no-mistakes <branch>\n")
			return nil
		},
	}
}
