package cli

import (
	"encoding/json"

	"github.com/spf13/cobra"
	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// newAxiContextCmd never opens writable resources or starts a daemon.
func newAxiContextCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "context",
		Short: "Identify this process's enclosing active custom gate (read-only)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			result, err := readEnclosingGateContext(cmd.Context())
			output := struct {
				SchemaVersion int    `json:"schema_version"`
				Available     bool   `json:"available"`
				Reason        string `json:"reason,omitempty"`
				*types.GateRunContext
			}{SchemaVersion: 1}
			if err != nil {
				output.Reason = "context_unavailable"
			} else if result.RunContext == nil {
				output.Reason = "no_enclosing_custom_gate"
			} else {
				output.Available = true
				output.GateRunContext = result.RunContext
			}
			if asJSON {
				if err := json.NewEncoder(cmd.OutOrStdout()).Encode(output); err != nil {
					return err
				}
			} else {
				fields := []toon.Field{
					{Key: "schema_version", Value: output.SchemaVersion},
					{Key: "available", Value: output.Available},
				}
				if output.Available {
					fields = append(fields,
						toon.Field{Key: "run_id", Value: output.RunID},
						toon.Field{Key: "repo_id", Value: output.RepoID},
						toon.Field{Key: "worktree", Value: output.Worktree},
						toon.Field{Key: "phase", Value: string(output.Phase)},
					)
				} else {
					fields = append(fields, toon.Field{Key: "reason", Value: output.Reason})
				}
				emitDoc(cmd, fields...)
			}
			if !output.Available {
				return &exitError{code: 1}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON instead of TOON")
	return cmd
}
