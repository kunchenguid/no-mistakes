package db

import (
	"encoding/json"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

// BindRunCommitIdentity is create-only. A retry or a restart cannot change the
// provenance of a run that already created commits.
func (d *DB) BindRunCommitIdentity(runID string, identity git.CommitIdentity) error {
	if err := identity.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(identity)
	if err != nil {
		return err
	}
	_, err = d.sql.Exec(`INSERT INTO run_commit_identities (run_id, identity_json) VALUES (?, ?)`, runID, string(data))
	return err
}

func (d *DB) RunCommitIdentity(runID string) (git.CommitIdentity, error) {
	var identity git.CommitIdentity
	var data string
	if err := d.sql.QueryRow(`SELECT identity_json FROM run_commit_identities WHERE run_id = ?`, runID).Scan(&data); err != nil {
		return identity, fmt.Errorf("read run commit identity: %w", err)
	}
	if err := json.Unmarshal([]byte(data), &identity); err != nil {
		return identity, err
	}
	return identity, identity.Validate()
}
