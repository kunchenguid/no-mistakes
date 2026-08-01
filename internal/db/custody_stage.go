package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const (
	CustodyRefStagePrepared = "prepared"
	CustodyRefStageStaged   = "staged"
)

type CustodyRefStage struct {
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

func (d *DB) PrepareCustodyRefStage(stage CustodyRefStage) error {
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
		return fmt.Errorf("prepare custody ref stage: exact ownership metadata is required")
	}
	stamp := now()
	_, err := d.sql.Exec(`INSERT INTO custody_ref_stages (run_id, repo_id, gate_path, branch, ref, old_sha, new_sha, owner_generation, authority_endpoint, state, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(run_id) DO UPDATE SET repo_id = excluded.repo_id, gate_path = excluded.gate_path, branch = excluded.branch, ref = excluded.ref, old_sha = excluded.old_sha, new_sha = excluded.new_sha, owner_generation = excluded.owner_generation, authority_endpoint = excluded.authority_endpoint, state = excluded.state, updated_at = excluded.updated_at`, stage.RunID, stage.RepoID, stage.GatePath, stage.Branch, stage.Ref, stage.OldSHA, stage.NewSHA, stage.OwnerGeneration, stage.AuthorityEndpoint, CustodyRefStagePrepared, stamp, stamp)
	return err
}

func (d *DB) GetCustodyRefStage(runID string) (*CustodyRefStage, error) {
	var stage CustodyRefStage
	err := d.sql.QueryRow(`SELECT run_id, repo_id, gate_path, branch, ref, old_sha, new_sha, owner_generation, authority_endpoint, state FROM custody_ref_stages WHERE run_id = ?`, strings.TrimSpace(runID)).Scan(&stage.RunID, &stage.RepoID, &stage.GatePath, &stage.Branch, &stage.Ref, &stage.OldSHA, &stage.NewSHA, &stage.OwnerGeneration, &stage.AuthorityEndpoint, &stage.State)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get custody ref stage: %w", err)
	}
	return &stage, nil
}

func (d *DB) MarkCustodyRefStageStaged(runID, ownerGeneration string) error {
	result, err := d.sql.Exec(`UPDATE custody_ref_stages SET state = ?, updated_at = ? WHERE run_id = ? AND owner_generation = ? AND state = ?`, CustodyRefStageStaged, now(), strings.TrimSpace(runID), strings.TrimSpace(ownerGeneration), CustodyRefStagePrepared)
	if err != nil {
		return fmt.Errorf("mark custody ref stage: %w", err)
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return fmt.Errorf("mark custody ref stage: ownership journal is missing or changed")
	}
	return nil
}

func (d *DB) ConsumedInternalRefMutationExists(spec InternalRefMutationSpec, authorityEndpoint string) (bool, error) {
	var count int
	err := d.sql.QueryRow(`SELECT COUNT(*) FROM internal_ref_mutations WHERE repo_id = ? AND gate_path = ? AND branch = ? AND ref = ? AND old_sha = ? AND new_sha = ? AND operation = ? AND scope = ? AND authority_endpoint = ? AND state = ?`, strings.TrimSpace(spec.RepoID), strings.TrimSpace(spec.GatePath), strings.TrimSpace(spec.Branch), strings.TrimSpace(spec.Ref), strings.TrimSpace(spec.OldSHA), strings.TrimSpace(spec.NewSHA), strings.TrimSpace(spec.Operation), strings.TrimSpace(spec.Scope), strings.TrimSpace(authorityEndpoint), InternalRefMutationStateConsumed).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("find consumed internal ref mutation: %w", err)
	}
	return count == 1, nil
}
