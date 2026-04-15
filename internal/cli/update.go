package cli

import (
	"github.com/kunchenguid/no-mistakes/internal/update"
	"github.com/spf13/cobra"
)

func newUpdateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "update",
		Short: "Update no-mistakes and reset the daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return update.Run(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
}
