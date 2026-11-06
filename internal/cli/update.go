package cli

import (
	"github.com/kunchenguid/no-mistakes/internal/update"
	"github.com/spf13/cobra"
)

func newUpdateCmd() *cobra.Command {
	var beta bool
	var yes bool
	var force bool
	var whenIdle bool
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update no-mistakes and reset the daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			logLifecycleInvocation("update", force)
			return trackCommand("update", func() error {
				return update.Run(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), update.RunOptions{Beta: beta, Yes: yes, Force: force, WhenIdle: whenIdle, Stdin: cmd.InOrStdin()})
			})
		},
	}
	cmd.Flags().BoolVar(&beta, "beta", false, "install the latest release including prereleases")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "answer yes to update safety prompts")
	cmd.Flags().BoolVar(&force, "force", false, "update and restart the daemon even when pipeline runs are active")
	cmd.Flags().BoolVar(&whenIdle, "when-idle", false, "let active runs finish on the current version and restart the daemon on the new one once no run is executing")
	return cmd
}
