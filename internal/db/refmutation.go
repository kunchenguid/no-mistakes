package db

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const (
	InternalRefMutationScopeOrdinary = "ordinary"
	InternalRefMutationScopePrivate  = "private"
	InternalRefMutationStateIssued   = "issued"
	InternalRefMutationStatePrepared = "prepared"
	InternalRefMutationStateConsumed = "consumed"
)

var ErrInternalRefMutation = errors.New("internal ref mutation capability is invalid or already consumed")

type InternalRefMutationSpec struct {
	RepoID    string
	GatePath  string
	Branch    string
	Ref       string
	OldSHA    string
	NewSHA    string
	Operation string
	Scope     string
}

type InternalRefMutation struct {
	ID                string
	RepoID            string
	GatePath          string
	Branch            string
	Ref               string
	OldSHA            string
	NewSHA            string
	Operation         string
	Scope             string
	AuthorityEndpoint string
	State             string
	CreatedAt         int64
}

func (d *DB) IssueInternalRefMutation(spec InternalRefMutationSpec, authorityEndpoint string) (string, error) {
	spec.RepoID = strings.TrimSpace(spec.RepoID)
	spec.GatePath = strings.TrimSpace(spec.GatePath)
	spec.Branch = strings.TrimSpace(spec.Branch)
	spec.Ref = strings.TrimSpace(spec.Ref)
	spec.OldSHA = strings.TrimSpace(spec.OldSHA)
	spec.NewSHA = strings.TrimSpace(spec.NewSHA)
	spec.Operation = strings.TrimSpace(spec.Operation)
	spec.Scope = strings.TrimSpace(spec.Scope)
	authorityEndpoint = strings.TrimSpace(authorityEndpoint)
	if spec.RepoID == "" || spec.GatePath == "" || spec.Branch == "" || spec.Ref == "" || spec.OldSHA == "" || spec.NewSHA == "" || spec.Operation == "" || authorityEndpoint == "" {
		return "", fmt.Errorf("issue internal ref mutation: exact identity and active lock proof are required")
	}
	if spec.Scope != InternalRefMutationScopeOrdinary && spec.Scope != InternalRefMutationScopePrivate {
		return "", fmt.Errorf("issue internal ref mutation: unsupported scope %q", spec.Scope)
	}
	if !strings.HasPrefix(spec.Ref, "refs/heads/") && !strings.HasPrefix(spec.Ref, "refs/no-mistakes/") {
		return "", fmt.Errorf("issue internal ref mutation: unsupported managed ref %q", spec.Ref)
	}
	if strings.HasPrefix(spec.Ref, "refs/heads/") && spec.Scope != InternalRefMutationScopeOrdinary {
		return "", fmt.Errorf("issue internal ref mutation: ordinary refs require ordinary scope")
	}
	if strings.HasPrefix(spec.Ref, "refs/no-mistakes/") && spec.Scope != InternalRefMutationScopePrivate {
		return "", fmt.Errorf("issue internal ref mutation: private refs require private scope")
	}
	capability, err := randomCapability()
	if err != nil {
		return "", fmt.Errorf("issue internal ref mutation: generate capability: %w", err)
	}
	stamp := now()
	_, err = d.sql.Exec(`INSERT INTO internal_ref_mutations (id, repo_id, gate_path, branch, ref, old_sha, new_sha, operation, scope, capability_hash, authority_endpoint, state, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		newID(), spec.RepoID, spec.GatePath, spec.Branch, spec.Ref, spec.OldSHA, spec.NewSHA, spec.Operation, spec.Scope, capabilityHash(capability), authorityEndpoint, InternalRefMutationStateIssued, stamp, stamp)
	if err != nil {
		return "", fmt.Errorf("issue internal ref mutation: %w", err)
	}
	return capability, nil
}

func (d *DB) GetInternalRefMutation(capability string) (*InternalRefMutation, error) {
	capability = strings.TrimSpace(capability)
	if capability == "" {
		return nil, ErrInternalRefMutation
	}
	var mutation InternalRefMutation
	err := d.sql.QueryRow(`SELECT id, repo_id, gate_path, branch, ref, old_sha, new_sha, operation, scope, authority_endpoint, state, created_at FROM internal_ref_mutations WHERE capability_hash = ?`, capabilityHash(capability)).Scan(
		&mutation.ID, &mutation.RepoID, &mutation.GatePath, &mutation.Branch, &mutation.Ref, &mutation.OldSHA, &mutation.NewSHA, &mutation.Operation, &mutation.Scope, &mutation.AuthorityEndpoint, &mutation.State, &mutation.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInternalRefMutation
	}
	if err != nil {
		return nil, fmt.Errorf("get internal ref mutation: %w", err)
	}
	return &mutation, nil
}

func (d *DB) InvalidateInternalRefMutations(authorityEndpoint string) error {
	_, err := d.sql.Exec(`UPDATE internal_ref_mutations SET state = ?, updated_at = ? WHERE authority_endpoint = ? AND state IN (?, ?)`, InternalRefMutationStateConsumed, now(), strings.TrimSpace(authorityEndpoint), InternalRefMutationStateIssued, InternalRefMutationStatePrepared)
	return err
}

func (d *DB) InvalidateInternalRefMutationsPrefix(authorityPrefix string) error {
	prefix := strings.TrimSpace(authorityPrefix)
	_, err := d.sql.Exec(`UPDATE internal_ref_mutations SET state = ?, updated_at = ? WHERE (authority_endpoint = ? OR (authority_endpoint >= ? AND authority_endpoint < ?)) AND state IN (?, ?)`, InternalRefMutationStateConsumed, now(), prefix, prefix+"-", prefix+".", InternalRefMutationStateIssued, InternalRefMutationStatePrepared)
	return err
}

func (d *DB) AdvanceInternalRefMutation(authorityEndpoint, phase, gatePath, branch, ref, oldSHA, newSHA, operation, scope, capability string) error {
	if phase != "prepared" && phase != "committed" && phase != "aborted" {
		return fmt.Errorf("advance internal ref mutation: unsupported phase %q", phase)
	}
	mutation, err := d.GetInternalRefMutation(capability)
	if err != nil {
		return err
	}
	if mutation.AuthorityEndpoint != strings.TrimSpace(authorityEndpoint) || mutation.GatePath != strings.TrimSpace(gatePath) || mutation.Branch != strings.TrimSpace(branch) || mutation.Ref != strings.TrimSpace(ref) || mutation.OldSHA != strings.TrimSpace(oldSHA) || mutation.NewSHA != strings.TrimSpace(newSHA) || mutation.Operation != strings.TrimSpace(operation) || mutation.Scope != strings.TrimSpace(scope) {
		return ErrInternalRefMutation
	}
	from := InternalRefMutationStateIssued
	to := InternalRefMutationStatePrepared
	if phase == "committed" || phase == "aborted" {
		from = InternalRefMutationStatePrepared
		to = InternalRefMutationStateConsumed
		if phase == "aborted" && mutation.State == InternalRefMutationStateIssued {
			from = InternalRefMutationStateIssued
		}
	}
	if mutation.State != from {
		return ErrInternalRefMutation
	}
	result, err := d.sql.Exec(`UPDATE internal_ref_mutations SET state = ?, updated_at = ? WHERE id = ? AND state = ?`, to, now(), mutation.ID, from)
	if err != nil {
		return fmt.Errorf("advance internal ref mutation: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return ErrInternalRefMutation
	}
	return nil
}

func randomCapability() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func capabilityHash(value string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(value)))
	return hex.EncodeToString(sum[:])
}
