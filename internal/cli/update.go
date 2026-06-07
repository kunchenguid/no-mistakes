package cli

import (
	"github.com/kunchenguid/no-mistakes/internal/update"
	"github.com/spf13/cobra"
)

func newUpdateCmd() *cobra.Command {
	var beta bool
	var yes bool
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update no-mistakes and reset the daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommand("update", func() error {
				return update.Run(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), update.RunOptions{Beta: beta, Yes: yes, Stdin: cmd.InOrStdin()})
			})
		},
	}
	cmd.Flags().BoolVar(&beta, "beta", false, "install the latest release including prereleases")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "answer yes to update safety prompts")
	return cmd
}
