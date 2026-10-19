package db

import "fmt"

// VerifyTerminalReword records preservation, not review approval. The caller
// has proved both exact histories and anchored them before this CAS.
func (d *DB) VerifyTerminalReword(id, submitted, recorded, rewritten string) (bool, error) {
	ts := now()
	result, err := d.sql.Exec(`UPDATE runs SET head_sha = ?, terminal_head_verified_at = ?, updated_at = ?
		WHERE id = ? AND status = 'failed' AND submitted_head_sha = ? AND head_sha = ?
		AND review_approved_head_sha IS NULL AND terminal_head_verified_at IS NULL
		AND custody_returned_at IS NULL AND COALESCE(push_generation, 0) = 0
		AND COALESCE(last_pushed_sha, '') = '' AND push_active = 0`,
		rewritten, ts, ts, id, submitted, recorded)
	if err != nil {
		return false, fmt.Errorf("verify terminal reword: %w", err)
	}
	n, err := result.RowsAffected()
	return n == 1, err
}
