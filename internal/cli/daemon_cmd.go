package cli

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
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
	cmd.AddCommand(newDaemonNotifyPushCmd())

	return cmd
}

func newDaemonNotifyPushCmd() *cobra.Command {
	var gate string
	var ref string
	var oldSHA string
	var newSHA string

	cmd := &cobra.Command{
		Use:    "notify-push",
		Short:  "Notify daemon about a git push",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := paths.New()
			if err != nil {
				return err
			}

			client, err := ipc.Dial(p.Socket())
			if err != nil {
				return fmt.Errorf("connect to daemon: %w", err)
			}
			defer client.Close()

			var result ipc.PushReceivedResult
			return client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
				Gate: gate,
				Ref:  ref,
				Old:  oldSHA,
				New:  newSHA,
			}, &result)
		},
	}

	cmd.Flags().StringVar(&gate, "gate", "", "bare repo path that received the push")
	cmd.Flags().StringVar(&ref, "ref", "", "git ref name")
	cmd.Flags().StringVar(&oldSHA, "old", "", "previous commit SHA")
	cmd.Flags().StringVar(&newSHA, "new", "", "new commit SHA")
	_ = cmd.MarkFlagRequired("gate")
	_ = cmd.MarkFlagRequired("ref")
	_ = cmd.MarkFlagRequired("old")
	_ = cmd.MarkFlagRequired("new")

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
