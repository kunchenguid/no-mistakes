package cli

import (
	"fmt"
	"os"

	"github.com/kunchenguid/no-mistakes/internal/buildinfo"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/spf13/cobra"
)

// Execute runs the root CLI command.
func Execute() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "no-mistakes",
		Short:   "Local Git proxy that validates code before pushing upstream",
		Version: buildinfo.String(),
		// Silence cobra's default error/usage printing — we handle it ourselves.
		SilenceErrors: true,
		SilenceUsage:  true,
		// When run without a subcommand, default to attach behavior.
		RunE: func(cmd *cobra.Command, args []string) error {
			return attachRun(cmd.OutOrStdout(), "")
		},
	}

	cmd.AddCommand(newInitCmd())
	cmd.AddCommand(newEjectCmd())
	cmd.AddCommand(newUpdateCmd())
	cmd.AddCommand(newDaemonCmd())
	cmd.AddCommand(newAttachCmd())
	cmd.AddCommand(newRerunCmd())
	cmd.AddCommand(newStatusCmd())
	cmd.AddCommand(newRunsCmd())
	cmd.AddCommand(newDoctorCmd())

	return cmd
}

// openResources initializes paths, ensures directories exist, and opens the DB.
// Caller must close the returned DB.
func openResources() (*paths.Paths, *db.DB, error) {
	p, err := paths.New()
	if err != nil {
		return nil, nil, fmt.Errorf("resolve paths: %w", err)
	}
	if err := p.EnsureDirs(); err != nil {
		return nil, nil, fmt.Errorf("create directories: %w", err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		return nil, nil, fmt.Errorf("open database: %w", err)
	}
	return p, d, nil
}
