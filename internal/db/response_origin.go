package db

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// SetStepResponseOrigin records caller-supplied provenance for a gate response
// stored on the step. Fix selections store their origin atomically on the
// answered round.
func (d *DB) SetStepResponseOrigin(stepID string, origin types.ResponseOrigin) error {
	result, err := d.sql.Exec(`UPDATE step_results SET response_origin = ? WHERE id = ?`, origin.OrUnknown(), stepID)
	if err != nil {
		return fmt.Errorf("record gate response origin: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("record gate response origin rows affected: %w", err)
	}
	if count != 1 {
		return fmt.Errorf("record gate response origin: updated %d rows", count)
	}
	return nil
}
