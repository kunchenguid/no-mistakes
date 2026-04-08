package cli

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/spf13/cobra"
)

func newDaemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Manage the no-mistakes daemon",
	}

	cmd.AddCommand(newDaemonStartCmd())
	cmd.AddCommand(newDaemonStopCmd())
	cmd.AddCommand(newDaemonStatusCmd())

	return cmd
}

func newDaemonStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the daemon in the background",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := paths.New()
			if err != nil {
				return err
			}
			if err := p.EnsureDirs(); err != nil {
				return err
			}
			if err := daemon.Start(p); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "daemon started")
			return nil
		},
	}
}

func newDaemonStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the running daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := paths.New()
			if err != nil {
				return err
			}
			if err := daemon.Stop(p); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "daemon stopped")
			return nil
		},
	}
}

func newDaemonStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Check if the daemon is running",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := paths.New()
			if err != nil {
				return err
			}
			alive, err := daemon.IsRunning(p)
			if err != nil {
				return err
			}
			if alive {
				pid, _ := daemon.ReadPID(p)
				if pid > 0 {
					fmt.Fprintf(cmd.OutOrStdout(), "daemon running (pid %d)\n", pid)
				} else {
					fmt.Fprintln(cmd.OutOrStdout(), "daemon running")
				}
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "daemon not running")
			}
			return nil
		},
	}
}
