package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/spf13/cobra"
)

func newExportCmd() *cobra.Command {
	var runID string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "export --run <id> --json",
		Short: "Export stored run facts as read-only, versioned JSON",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(runID) == "" || !asJSON {
				return fmt.Errorf("export requires --run <id> and --json")
			}
			p, err := paths.New()
			if err != nil {
				return fmt.Errorf("resolve export paths: %w", err)
			}
			// Do not use openResources: export must not create directories,
			// migrate a database, start a daemon or emit remote telemetry.
			database, err := db.OpenReadOnly(p.DB())
			if err != nil {
				return fmt.Errorf("open export database: %w", err)
			}
			defer database.Close()
			record, err := database.ExportRunFacts(cmd.Context(), runID)
			if err != nil {
				return err
			}
			encoder := json.NewEncoder(cmd.OutOrStdout())
			encoder.SetIndent("", "  ")
			return encoder.Encode(record)
		},
	}
	cmd.Flags().StringVar(&runID, "run", "", "exact run ID to export (required)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit schema version 1 JSON (required)")
	return cmd
}
