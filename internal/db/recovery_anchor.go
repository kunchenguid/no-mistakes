package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const (
	RecoveryAnchorStagePrepared = "prepared"
	RecoveryAnchorStageStaged   = "staged"
)

type RecoveryAnchorStage struct {
	RunID             string
	RepoID            string
	GatePath          string
	Branch            string
	Ref               string
	OldSHA            string
	NewSHA            string
	OwnerGeneration   string
	AuthorityEndpoint string
	State             string
}

func (d *DB) PrepareRecoveryAnchorStage(stage RecoveryAnchorStage) error {
	stage.RunID = strings.TrimSpace(stage.RunID)
	stage.RepoID = strings.TrimSpace(stage.RepoID)
	stage.GatePath = strings.TrimSpace(stage.GatePath)
	stage.Branch = strings.TrimSpace(stage.Branch)
	stage.Ref = strings.TrimSpace(stage.Ref)
	stage.OldSHA = strings.TrimSpace(stage.OldSHA)
	stage.NewSHA = strings.TrimSpace(stage.NewSHA)
	stage.OwnerGeneration = strings.TrimSpace(stage.OwnerGeneration)
	stage.AuthorityEndpoint = strings.TrimSpace(stage.AuthorityEndpoint)
	if stage.RunID == "" || stage.RepoID == "" || stage.GatePath == "" || stage.Branch == "" || stage.Ref == "" || stage.OldSHA == "" || stage.NewSHA == "" || stage.OwnerGeneration == "" || stage.AuthorityEndpoint == "" {
		return fmt.Errorf("prepare recovery anchor stage: exact ownership metadata is required")
	}
	stamp := now()
	_, err := d.sql.Exec(`INSERT INTO recovery_anchor_stages (run_id, repo_id, gate_path, branch, ref, old_sha, new_sha, owner_generation, authority_endpoint, state, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(run_id) DO UPDATE SET repo_id = excluded.repo_id, gate_path = excluded.gate_path, branch = excluded.branch, ref = excluded.ref, old_sha = excluded.old_sha, new_sha = excluded.new_sha, owner_generation = excluded.owner_generation, authority_endpoint = excluded.authority_endpoint, state = excluded.state, updated_at = excluded.updated_at`, stage.RunID, stage.RepoID, stage.GatePath, stage.Branch, stage.Ref, stage.OldSHA, stage.NewSHA, stage.OwnerGeneration, stage.AuthorityEndpoint, RecoveryAnchorStagePrepared, stamp, stamp)
	return err
}

func (d *DB) GetRecoveryAnchorStage(runID string) (*RecoveryAnchorStage, error) {
	var stage RecoveryAnchorStage
	err := d.sql.QueryRow(`SELECT run_id, repo_id, gate_path, branch, ref, old_sha, new_sha, owner_generation, authority_endpoint, state FROM recovery_anchor_stages WHERE run_id = ?`, strings.TrimSpace(runID)).Scan(&stage.RunID, &stage.RepoID, &stage.GatePath, &stage.Branch, &stage.Ref, &stage.OldSHA, &stage.NewSHA, &stage.OwnerGeneration, &stage.AuthorityEndpoint, &stage.State)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get recovery anchor stage: %w", err)
	}
	return &stage, nil
}

func (d *DB) MarkRecoveryAnchorStageStaged(runID, ownerGeneration string) error {
	result, err := d.sql.Exec(`UPDATE recovery_anchor_stages SET state = ?, updated_at = ? WHERE run_id = ? AND owner_generation = ? AND state = ?`, RecoveryAnchorStageStaged, now(), strings.TrimSpace(runID), strings.TrimSpace(ownerGeneration), RecoveryAnchorStagePrepared)
	if err != nil {
		return fmt.Errorf("mark recovery anchor stage: %w", err)
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("mark recovery anchor stage: ownership journal is missing or changed")
	}
	return nil
}
