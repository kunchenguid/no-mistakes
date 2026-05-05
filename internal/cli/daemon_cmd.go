package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

var (
	daemonRun         = daemon.Run
	daemonStartFn     = daemon.Start
	daemonStopFn      = daemon.Stop
	daemonIsRunningFn = daemon.IsRunning
)

func newDaemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Manage the no-mistakes daemon",
	}

	cmd.AddCommand(newDaemonStartCmd())
	cmd.AddCommand(newDaemonStopCmd())
	cmd.AddCommand(newDaemonRestartCmd())
	cmd.AddCommand(newDaemonStatusCmd())
	cmd.AddCommand(newDaemonRunCmd())
	cmd.AddCommand(newDaemonNotifyPushCmd())

	return cmd
}

func newDaemonNotifyPushCmd() *cobra.Command {
	var gate string
	var ref string
	var oldSHA string
	var newSHA string
	var pushOptions []string

	cmd := &cobra.Command{
		Use:    "notify-push",
		Short:  "Notify daemon about a git push",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			skipSteps, err := parseSkipPushOptions(pushOptions)
			if err != nil {
				return err
			}

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
				Gate:      gate,
				Ref:       ref,
				Old:       oldSHA,
				New:       newSHA,
				SkipSteps: skipSteps,
			}, &result)
		},
	}

	cmd.Flags().StringVar(&gate, "gate", "", "bare repo path that received the push")
	cmd.Flags().StringVar(&ref, "ref", "", "git ref name")
	cmd.Flags().StringVar(&oldSHA, "old", "", "previous commit SHA")
	cmd.Flags().StringVar(&newSHA, "new", "", "new commit SHA")
	cmd.Flags().StringArrayVar(&pushOptions, "push-option", nil, "git push option")
	_ = cmd.MarkFlagRequired("gate")
	_ = cmd.MarkFlagRequired("ref")
	_ = cmd.MarkFlagRequired("old")
	_ = cmd.MarkFlagRequired("new")

	return cmd
}

func parseSkipPushOptions(options []string) ([]types.StepName, error) {
	var steps []types.StepName
	for _, option := range options {
		value, ok := strings.CutPrefix(option, "no-mistakes.skip=")
		if !ok {
			continue
		}
		parsed, err := parseSkipSteps(value)
		if err != nil {
			return nil, err
		}
		steps = append(steps, parsed...)
	}
	return dedupeSteps(steps), nil
}

func parseSkipSteps(value string) ([]types.StepName, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	var steps []types.StepName
	for _, part := range strings.Split(value, ",") {
		step := types.StepName(strings.TrimSpace(part))
		if !validStep(step) {
			return nil, fmt.Errorf("unknown step %q", step)
		}
		steps = append(steps, step)
	}
	return dedupeSteps(steps), nil
}

func formatSkipPushOptions(steps []types.StepName) []string {
	if len(steps) == 0 {
		return nil
	}
	parts := make([]string, 0, len(steps))
	for _, step := range dedupeSteps(steps) {
		parts = append(parts, string(step))
	}
	return []string{"no-mistakes.skip=" + strings.Join(parts, ",")}
}

func validStep(step types.StepName) bool {
	for _, known := range types.AllSteps() {
		if step == known {
			return true
		}
	}
	return false
}

func dedupeSteps(steps []types.StepName) []types.StepName {
	seen := make(map[types.StepName]bool, len(steps))
	out := make([]types.StepName, 0, len(steps))
	for _, step := range steps {
		if seen[step] {
			continue
		}
		seen[step] = true
		out = append(out, step)
	}
	return out
}

func newDaemonStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Install or refresh the managed daemon service and start it",
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommand("daemon.start", func() error {
				p, err := paths.New()
				if err != nil {
					return err
				}
				if err := p.EnsureDirs(); err != nil {
					return err
				}
				if err := daemonStartFn(p); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon started\n", sGreen.Render("✓"))
				return nil
			})
		},
	}
}

func newDaemonStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the running daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommand("daemon.stop", func() error {
				p, err := paths.New()
				if err != nil {
					return err
				}
				if err := daemonStopFn(p); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon stopped\n", sGreen.Render("✓"))
				return nil
			})
		},
	}
}

func newDaemonRestartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: "Restart the daemon (stop if running, then start)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommand("daemon.restart", func() error {
				p, err := paths.New()
				if err != nil {
					return err
				}
				if err := p.EnsureDirs(); err != nil {
					return err
				}
				if err := daemonStopFn(p); err != nil {
					return fmt.Errorf("stop daemon: %w", err)
				}
				if err := daemonStartFn(p); err != nil {
					return fmt.Errorf("start daemon: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon restarted\n", sGreen.Render("✓"))
				return nil
			})
		},
	}
}

func newDaemonStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Check if the daemon is running",
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommand("daemon.status", func() error {
				p, err := paths.New()
				if err != nil {
					return err
				}
				alive, err := daemonIsRunningFn(p)
				if err != nil {
					return err
				}
				if alive {
					pid, _ := daemon.ReadPID(p)
					if pid > 0 {
						fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon running %s\n", sGreen.Render("●"), sDim.Render(fmt.Sprintf("(pid %d)", pid)))
					} else {
						fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon running\n", sGreen.Render("●"))
					}
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon not running\n", sDim.Render("○"))
				}
				return nil
			})
		},
	}
}

func newDaemonRunCmd() *cobra.Command {
	var root string

	cmd := &cobra.Command{
		Use:    "run",
		Short:  "Run the daemon in the foreground",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if root != "" {
				if err := os.Setenv("NM_HOME", root); err != nil {
					return fmt.Errorf("set NM_HOME: %w", err)
				}
			}
			return daemonRun()
		},
	}

	cmd.Flags().StringVar(&root, "root", "", "override no-mistakes data directory")
	return cmd
}
