package db

import (
	"database/sql"
	"fmt"
	"strings"
)

const (
	PublicationTargetNoAttempt = "no_attempt"
	PublicationTargetAttempted = "attempted"
	PublicationTargetPublished = "published"
	PublicationTargetAmbiguous = "ambiguous"
)

type PublicationTargetInput struct {
	TargetKind        string
	TargetFingerprint string
	Ref               string
	TargetVersion     int64
}

type RunPublicationTarget struct {
	RunID             string
	TargetKind        string
	TargetFingerprint string
	Ref               string
	TargetVersion     int64
	State             string
	RequestIdentity   string
	AttemptHeadSHA    string
	Generation        int64
	Provenance        string
}

func PublicationTargetFingerprint(raw string) string {
	return publicationTargetFingerprint(raw)
}

func PublicationTargetInputs(repo *Repo, branch string) []PublicationTargetInput {
	if repo == nil {
		return nil
	}
	ref := publicationRef(branch)
	seen := make(map[string]struct{})
	inputs := make([]PublicationTargetInput, 0, 2)
	for _, item := range []struct {
		kind string
		url  string
	}{
		{kind: "upstream", url: repo.UpstreamURL},
		{kind: "fork", url: repo.ForkURL},
	} {
		fingerprint := publicationTargetFingerprint(item.url)
		if strings.TrimSpace(item.url) == "" || fingerprint == "" {
			continue
		}
		if _, ok := seen[fingerprint]; ok {
			continue
		}
		seen[fingerprint] = struct{}{}
		inputs = append(inputs, PublicationTargetInput{
			TargetKind:        item.kind,
			TargetFingerprint: fingerprint,
			Ref:               ref,
			TargetVersion:     repo.URLVersion,
		})
	}
	return inputs
}

func seedRunPublicationTargetsTx(tx *sql.Tx, runID string, inputs []PublicationTargetInput, provenance string) error {
	if strings.TrimSpace(runID) == "" {
		return ErrRunPublicationCAS
	}
	for _, input := range inputs {
		kind := strings.TrimSpace(input.TargetKind)
		fingerprint := strings.TrimSpace(input.TargetFingerprint)
		ref := strings.TrimSpace(input.Ref)
		if kind == "" || fingerprint == "" || ref == "" {
			return ErrRunPublicationCAS
		}
		if _, err := tx.Exec(`INSERT INTO run_publication_targets (run_id, target_kind, target_fingerprint, ref, target_version, state, generation, provenance, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?, ?) ON CONFLICT(run_id, target_fingerprint) DO NOTHING`, runID, kind, fingerprint, ref, input.TargetVersion, PublicationTargetNoAttempt, provenance, now(), now()); err != nil {
			return fmt.Errorf("seed publication target: %w", err)
		}
	}
	return nil
}

func (d *DB) SeedRunPublicationTargets(runID string, inputs []PublicationTargetInput) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("begin seed publication targets: %w", err)
	}
	defer tx.Rollback()
	if err := seedRunPublicationTargetsTx(tx, runID, inputs, "submission"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit seed publication targets: %w", err)
	}
	return nil
}

func (d *DB) ListRunPublicationTargets(runID string) ([]RunPublicationTarget, error) {
	rows, err := d.sql.Query(`SELECT run_id, target_kind, target_fingerprint, ref, target_version, state, COALESCE(request_identity, ''), COALESCE(attempt_head_sha, ''), generation, provenance FROM run_publication_targets WHERE run_id = ? ORDER BY target_fingerprint`, strings.TrimSpace(runID))
	if err != nil {
		return nil, fmt.Errorf("list run publication targets: %w", err)
	}
	defer rows.Close()
	var targets []RunPublicationTarget
	for rows.Next() {
		var target RunPublicationTarget
		if err := rows.Scan(&target.RunID, &target.TargetKind, &target.TargetFingerprint, &target.Ref, &target.TargetVersion, &target.State, &target.RequestIdentity, &target.AttemptHeadSHA, &target.Generation, &target.Provenance); err != nil {
			return nil, fmt.Errorf("scan run publication target: %w", err)
		}
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list run publication targets: %w", err)
	}
	return targets, nil
}

func (d *DB) SetRunPublicationTargetIdentity(runID, fingerprint, identity string) error {
	runID = strings.TrimSpace(runID)
	fingerprint = strings.TrimSpace(fingerprint)
	identity = strings.TrimSpace(identity)
	if runID == "" || fingerprint == "" || identity == "" {
		return ErrRunPublicationCAS
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("begin set publication target identity: %w", err)
	}
	defer tx.Rollback()
	var state, current string
	if err := tx.QueryRow(`SELECT state, COALESCE(request_identity, '') FROM run_publication_targets WHERE run_id = ? AND target_fingerprint = ?`, runID, fingerprint).Scan(&state, &current); err == sql.ErrNoRows {
		return ErrRunPublicationCAS
	} else if err != nil {
		return fmt.Errorf("read publication target identity: %w", err)
	}
	if state == PublicationTargetAmbiguous || (current != "" && current != identity) {
		return ErrRunPublicationCAS
	}
	if current == identity {
		return tx.Commit()
	}
	if _, err := tx.Exec(`UPDATE run_publication_targets SET request_identity = ?, state = CASE WHEN state = ? THEN ? ELSE state END, generation = generation + 1, provenance = ?, updated_at = ? WHERE run_id = ? AND target_fingerprint = ? AND state != ? AND (request_identity IS NULL OR request_identity = '')`, identity, PublicationTargetNoAttempt, PublicationTargetAttempted, "submission-identity", now(), runID, fingerprint, PublicationTargetAmbiguous); err != nil {
		return fmt.Errorf("set publication target identity: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit publication target identity: %w", err)
	}
	return nil
}

func migrateRunPublicationTargets(sqlDB *sql.DB) error {
	rows, err := sqlDB.Query(`SELECT runs.id, runs.branch, runs.publication_attempt_head_sha, runs.publication_attempt_target_kind, runs.publication_attempt_target_fingerprint, runs.publication_attempt_ref, runs.last_pushed_sha, runs.push_target_kind, runs.push_target_fingerprint, runs.push_ref, repos.upstream_url, repos.fork_url, repos.url_version FROM runs JOIN repos ON repos.id = runs.repo_id WHERE NOT EXISTS (SELECT 1 FROM run_publication_targets WHERE run_publication_targets.run_id = runs.id)`)
	if err != nil {
		return err
	}
	type legacyRun struct {
		id, branch                     string
		attemptHead, attemptKind       sql.NullString
		attemptFingerprint, attemptRef sql.NullString
		pushedHead, pushedKind         sql.NullString
		pushedFingerprint, pushedRef   sql.NullString
		upstream, fork                 string
		version                        int64
	}
	var legacy []legacyRun
	for rows.Next() {
		var item legacyRun
		if err := rows.Scan(&item.id, &item.branch, &item.attemptHead, &item.attemptKind, &item.attemptFingerprint, &item.attemptRef, &item.pushedHead, &item.pushedKind, &item.pushedFingerprint, &item.pushedRef, &item.upstream, &item.fork, &item.version); err != nil {
			rows.Close()
			return err
		}
		legacy = append(legacy, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(legacy) == 0 {
		return nil
	}
	tx, err := sqlDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, item := range legacy {
		inputs := make([]PublicationTargetInput, 0, 2)
		seen := make(map[string]struct{})
		for _, target := range []struct {
			kind string
			url  string
		}{
			{kind: "upstream", url: item.upstream},
			{kind: "fork", url: item.fork},
		} {
			fingerprint := publicationTargetFingerprint(target.url)
			if strings.TrimSpace(target.url) == "" || fingerprint == "" {
				continue
			}
			if _, ok := seen[fingerprint]; ok {
				continue
			}
			seen[fingerprint] = struct{}{}
			inputs = append(inputs, PublicationTargetInput{TargetKind: target.kind, TargetFingerprint: fingerprint, Ref: publicationRef(item.branch), TargetVersion: item.version})
		}
		if err := seedRunPublicationTargetsTx(tx, item.id, inputs, "legacy-migration"); err != nil {
			return err
		}
		if anyLegacyPublicationField(item.attemptHead, item.attemptKind, item.attemptFingerprint, item.attemptRef) {
			if !allLegacyPublicationFields(item.attemptHead, item.attemptKind, item.attemptFingerprint, item.attemptRef) {
				if err := markPublicationTargetsAmbiguous(tx, item.id); err != nil {
					return err
				}
			} else if err := migrateLegacyPublicationState(tx, item.id, item.attemptFingerprint.String, item.attemptHead.String, item.attemptKind.String, item.attemptRef.String, PublicationTargetAttempted, "legacy-attempt"); err != nil {
				if err := markPublicationTargetsAmbiguous(tx, item.id); err != nil {
					return err
				}
			}
		}
		if anyLegacyPublicationField(item.pushedHead, item.pushedKind, item.pushedFingerprint, item.pushedRef) {
			if !allLegacyPublicationFields(item.pushedHead, item.pushedKind, item.pushedFingerprint, item.pushedRef) {
				if err := markPublicationTargetsAmbiguous(tx, item.id); err != nil {
					return err
				}
			} else if err := migrateLegacyPublicationState(tx, item.id, item.pushedFingerprint.String, item.pushedHead.String, item.pushedKind.String, item.pushedRef.String, PublicationTargetPublished, "legacy-published"); err != nil {
				if err := markPublicationTargetsAmbiguous(tx, item.id); err != nil {
					return err
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

func anyLegacyPublicationField(values ...sql.NullString) bool {
	for _, value := range values {
		if value.Valid {
			return true
		}
	}
	return false
}

func allLegacyPublicationFields(values ...sql.NullString) bool {
	for _, value := range values {
		if !value.Valid || strings.TrimSpace(value.String) == "" {
			return false
		}
	}
	return true
}

func markPublicationTargetsAmbiguous(tx *sql.Tx, runID string) error {
	_, err := tx.Exec(`UPDATE run_publication_targets SET state = ?, provenance = ?, generation = generation + 1, updated_at = ? WHERE run_id = ?`, PublicationTargetAmbiguous, "legacy-target-ambiguous", now(), runID)
	return err
}

func migrateLegacyPublicationState(tx *sql.Tx, runID, fingerprint, head, kind, ref, state, provenance string) error {
	result, err := tx.Exec(`UPDATE run_publication_targets SET state = ?, attempt_head_sha = ?, target_kind = ?, ref = ?, generation = generation + 1, provenance = ?, updated_at = ? WHERE run_id = ? AND target_fingerprint = ? AND state = ?`, state, head, kind, ref, now(), runID, strings.TrimSpace(fingerprint), PublicationTargetNoAttempt)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrRunPublicationCAS
	}
	return nil
}
