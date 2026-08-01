package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

type ManagedGateRef struct {
	RepoID   string
	GatePath string
	Ref      string
	Head     string
}

func NormalizeManagedGateHead(head string) string {
	head = strings.TrimSpace(head)
	if head != "" && strings.Trim(head, "0") == "" {
		return ""
	}
	return head
}

func (d *DB) GetManagedGateRef(repoID, gatePath, ref string) (*ManagedGateRef, error) {
	var managed ManagedGateRef
	err := d.sql.QueryRow(`SELECT repo_id, gate_path, ref, head FROM managed_gate_refs WHERE repo_id = ? AND gate_path = ? AND ref = ?`, strings.TrimSpace(repoID), strings.TrimSpace(gatePath), strings.TrimSpace(ref)).Scan(&managed.RepoID, &managed.GatePath, &managed.Ref, &managed.Head)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get managed gate ref: %w", err)
	}
	return &managed, nil
}

func (d *DB) SetManagedGateRefHead(repoID, gatePath, ref, head string) error {
	repoID = strings.TrimSpace(repoID)
	gatePath = strings.TrimSpace(gatePath)
	ref = strings.TrimSpace(ref)
	head = NormalizeManagedGateHead(head)
	if repoID == "" || gatePath == "" || ref == "" {
		return fmt.Errorf("set managed gate ref: exact identity is required")
	}
	_, err := d.sql.Exec(`INSERT INTO managed_gate_refs (repo_id, gate_path, ref, head, updated_at) VALUES (?, ?, ?, ?, ?) ON CONFLICT(repo_id, gate_path, ref) DO UPDATE SET head = excluded.head, updated_at = excluded.updated_at`, repoID, gatePath, ref, head, now())
	if err != nil {
		return fmt.Errorf("set managed gate ref: %w", err)
	}
	return nil
}
