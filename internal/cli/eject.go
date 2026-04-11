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
			p, d, err := openResources()
			if err != nil {
				return err
			}
			defer d.Close()

			if err := gate.Eject(cmd.Context(), d, p, "."); err != nil {
				return fmt.Errorf("eject: %w", err)
			}

			fmt.Fprintln(cmd.OutOrStdout(), "ejected no-mistakes gate")
			return nil
		},
	}
}
