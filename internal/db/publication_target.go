package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

const (
	PublicationTargetNoAttempt = "no_attempt"
	PublicationTargetAttempted = "attempted"
	PublicationTargetPublished = "published"
	PublicationTargetAmbiguous = "ambiguous"

	PublicationTargetPRNoAttempt = "no_attempt"
	PublicationTargetPRPrepared  = "prepared"
	PublicationTargetPROpened    = "opened"
	PublicationTargetPRAmbiguous = "ambiguous"

	PublicationTargetSetComplete  = "complete"
	PublicationTargetSetAmbiguous = "ambiguous"
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
	PRState           string
	PRRequestIdentity string
	PRGeneration      int64
	PRProvenance      string
}

type RunPublicationTargetSet struct {
	RunID         string
	TargetCount   int
	TargetSetHash string
	State         string
	Generation    int64
	Provenance    string
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

func normalizePublicationTargetInputs(inputs []PublicationTargetInput) ([]PublicationTargetInput, error) {
	byFingerprint := make(map[string]PublicationTargetInput, len(inputs))
	for _, input := range inputs {
		input.TargetKind = strings.TrimSpace(input.TargetKind)
		input.TargetFingerprint = strings.TrimSpace(input.TargetFingerprint)
		input.Ref = strings.TrimSpace(input.Ref)
		if input.TargetKind == "" || input.TargetFingerprint == "" || input.Ref == "" || input.TargetVersion < 0 {
			return nil, ErrRunPublicationCAS
		}
		if previous, ok := byFingerprint[input.TargetFingerprint]; ok {
			if previous != input {
				return nil, ErrRunPublicationCAS
			}
			continue
		}
		byFingerprint[input.TargetFingerprint] = input
	}
	normalized := make([]PublicationTargetInput, 0, len(byFingerprint))
	for _, input := range byFingerprint {
		normalized = append(normalized, input)
	}
	sort.Slice(normalized, func(i, j int) bool {
		return normalized[i].TargetFingerprint < normalized[j].TargetFingerprint
	})
	return normalized, nil
}

func publicationTargetSetHash(inputs []PublicationTargetInput) string {
	parts := make([]string, 0, len(inputs))
	for _, input := range inputs {
		parts = append(parts, strings.Join([]string{input.TargetKind, input.TargetFingerprint, input.Ref, fmt.Sprintf("%d", input.TargetVersion)}, "\x00"))
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x01")))
	return hex.EncodeToString(sum[:])
}

func PublicationTargetSetHash(targets []RunPublicationTarget) string {
	inputs := make([]PublicationTargetInput, 0, len(targets))
	for _, target := range targets {
		inputs = append(inputs, PublicationTargetInput{
			TargetKind:        target.TargetKind,
			TargetFingerprint: target.TargetFingerprint,
			Ref:               target.Ref,
			TargetVersion:     target.TargetVersion,
		})
	}
	normalized, err := normalizePublicationTargetInputs(inputs)
	if err != nil {
		return ""
	}
	return publicationTargetSetHash(normalized)
}

func insertPublicationTargetSetTx(tx *sql.Tx, runID string, count int, hash, state, provenance string) error {
	if strings.TrimSpace(runID) == "" || count < 0 || strings.TrimSpace(hash) == "" || strings.TrimSpace(state) == "" || strings.TrimSpace(provenance) == "" {
		return ErrRunPublicationCAS
	}
	ts := now()
	if _, err := tx.Exec(`INSERT INTO run_publication_target_sets (run_id, target_count, target_set_hash, state, generation, provenance, created_at, updated_at) VALUES (?, ?, ?, ?, 0, ?, ?, ?)`, runID, count, hash, state, provenance, ts, ts); err != nil {
		return fmt.Errorf("insert publication target set: %w", err)
	}
	return nil
}

func seedRunPublicationTargetsTx(tx *sql.Tx, runID string, inputs []PublicationTargetInput, provenance string) error {
	if strings.TrimSpace(runID) == "" {
		return ErrRunPublicationCAS
	}
	normalized, err := normalizePublicationTargetInputs(inputs)
	if err != nil {
		return err
	}
	var existingSet RunPublicationTargetSet
	setErr := tx.QueryRow(`SELECT run_id, target_count, target_set_hash, state, generation, provenance FROM run_publication_target_sets WHERE run_id = ?`, runID).Scan(&existingSet.RunID, &existingSet.TargetCount, &existingSet.TargetSetHash, &existingSet.State, &existingSet.Generation, &existingSet.Provenance)
	if setErr == nil {
		if existingSet.State != PublicationTargetSetComplete || existingSet.TargetCount != len(normalized) || existingSet.TargetSetHash != publicationTargetSetHash(normalized) {
			return ErrRunPublicationCAS
		}
		return nil
	}
	if setErr != sql.ErrNoRows {
		return fmt.Errorf("read publication target set: %w", setErr)
	}
	var existingCount int
	if err := tx.QueryRow(`SELECT count(*) FROM run_publication_targets WHERE run_id = ?`, runID).Scan(&existingCount); err != nil {
		return fmt.Errorf("count publication targets: %w", err)
	}
	if existingCount != 0 {
		return ErrRunPublicationCAS
	}
	ts := now()
	for _, input := range normalized {
		if _, err := tx.Exec(`INSERT INTO run_publication_targets (run_id, target_kind, target_fingerprint, ref, target_version, state, generation, provenance, pr_state, pr_generation, pr_provenance, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?, 0, ?, ?, ?)`, runID, input.TargetKind, input.TargetFingerprint, input.Ref, input.TargetVersion, PublicationTargetNoAttempt, provenance, PublicationTargetPRNoAttempt, provenance, ts, ts); err != nil {
			return fmt.Errorf("seed publication target: %w", err)
		}
	}
	return insertPublicationTargetSetTx(tx, runID, len(normalized), publicationTargetSetHash(normalized), PublicationTargetSetComplete, provenance)
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
	rows, err := d.sql.Query(`SELECT run_id, target_kind, target_fingerprint, ref, target_version, state, COALESCE(request_identity, ''), COALESCE(attempt_head_sha, ''), generation, provenance, pr_state, COALESCE(pr_request_identity, ''), pr_generation, pr_provenance FROM run_publication_targets WHERE run_id = ? ORDER BY target_fingerprint`, strings.TrimSpace(runID))
	if err != nil {
		return nil, fmt.Errorf("list run publication targets: %w", err)
	}
	defer rows.Close()
	var targets []RunPublicationTarget
	for rows.Next() {
		var target RunPublicationTarget
		if err := rows.Scan(&target.RunID, &target.TargetKind, &target.TargetFingerprint, &target.Ref, &target.TargetVersion, &target.State, &target.RequestIdentity, &target.AttemptHeadSHA, &target.Generation, &target.Provenance, &target.PRState, &target.PRRequestIdentity, &target.PRGeneration, &target.PRProvenance); err != nil {
			return nil, fmt.Errorf("scan run publication target: %w", err)
		}
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list run publication targets: %w", err)
	}
	return targets, nil
}

func (d *DB) GetRunPublicationTargetSet(runID string) (*RunPublicationTargetSet, error) {
	var set RunPublicationTargetSet
	err := d.sql.QueryRow(`SELECT run_id, target_count, target_set_hash, state, generation, provenance FROM run_publication_target_sets WHERE run_id = ?`, strings.TrimSpace(runID)).Scan(&set.RunID, &set.TargetCount, &set.TargetSetHash, &set.State, &set.Generation, &set.Provenance)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read run publication target set: %w", err)
	}
	return &set, nil
}

func (d *DB) PrepareRunPublicationTargetAttempt(runID, targetKind, targetFingerprint, ref string) error {
	runID = strings.TrimSpace(runID)
	targetKind = strings.TrimSpace(targetKind)
	targetFingerprint = strings.TrimSpace(targetFingerprint)
	ref = strings.TrimSpace(ref)
	if runID == "" || targetKind == "" || targetFingerprint == "" || ref == "" {
		return ErrRunPublicationCAS
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("begin prepare publication target: %w", err)
	}
	defer tx.Rollback()
	var setState string
	if err := tx.QueryRow(`SELECT state FROM run_publication_target_sets WHERE run_id = ?`, runID).Scan(&setState); err == sql.ErrNoRows {
		return ErrRunPublicationCAS
	} else if err != nil {
		return fmt.Errorf("read publication target set: %w", err)
	}
	if setState != PublicationTargetSetComplete {
		return ErrRunPublicationCAS
	}
	var prState, currentKind, currentRef string
	if err := tx.QueryRow(`SELECT pr_state, target_kind, ref FROM run_publication_targets WHERE run_id = ? AND target_fingerprint = ?`, runID, targetFingerprint).Scan(&prState, &currentKind, &currentRef); err == sql.ErrNoRows {
		return ErrRunPublicationCAS
	} else if err != nil {
		return fmt.Errorf("read publication target for attempt: %w", err)
	}
	if currentKind != targetKind || currentRef != ref || prState == PublicationTargetPRAmbiguous || prState == PublicationTargetPROpened {
		return ErrRunPublicationCAS
	}
	if prState == PublicationTargetPRPrepared {
		return tx.Commit()
	}
	result, err := tx.Exec(`UPDATE run_publication_targets SET pr_state = ?, pr_generation = pr_generation + 1, pr_provenance = ?, updated_at = ? WHERE run_id = ? AND target_fingerprint = ? AND pr_state = ?`, PublicationTargetPRPrepared, "pipeline-pr-attempt-prepared", now(), runID, targetFingerprint, PublicationTargetPRNoAttempt)
	if err != nil {
		return fmt.Errorf("prepare publication target attempt: %w", err)
	}
	if rows, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("prepare publication target attempt: affected rows: %w", err)
	} else if rows != 1 {
		return ErrRunPublicationCAS
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit prepare publication target: %w", err)
	}
	return nil
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
	if err := tx.QueryRow(`SELECT pr_state, COALESCE(pr_request_identity, '') FROM run_publication_targets WHERE run_id = ? AND target_fingerprint = ?`, runID, fingerprint).Scan(&state, &current); err == sql.ErrNoRows {
		return ErrRunPublicationCAS
	} else if err != nil {
		return fmt.Errorf("read publication target identity: %w", err)
	}
	if state != PublicationTargetPRNoAttempt && state != PublicationTargetPRPrepared && state != PublicationTargetPROpened || (current != "" && current != identity) {
		return ErrRunPublicationCAS
	}
	if current == identity && state == PublicationTargetPROpened {
		return tx.Commit()
	}
	if _, err := tx.Exec(`UPDATE run_publication_targets SET pr_request_identity = ?, pr_state = CASE WHEN pr_state IN (?, ?) THEN ? ELSE pr_state END, pr_generation = pr_generation + 1, pr_provenance = ?, updated_at = ? WHERE run_id = ? AND target_fingerprint = ? AND pr_state != ?`, identity, PublicationTargetPRNoAttempt, PublicationTargetPRPrepared, PublicationTargetPROpened, "submission-identity", now(), runID, fingerprint, PublicationTargetPRAmbiguous); err != nil {
		return fmt.Errorf("set publication target identity: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit publication target identity: %w", err)
	}
	return nil
}

func (d *DB) ValidateRunPublicationTargetLedger(runID string) error {
	run, err := d.GetRun(runID)
	if err != nil || run == nil {
		return ErrRunPublicationCAS
	}
	if run.PRURL != nil && strings.TrimSpace(*run.PRURL) != "" || run.LastPushedSHA != nil || run.PublicationAttemptHeadSHA != nil {
		return ErrRunPublicationCAS
	}
	targets, err := d.ListRunPublicationTargets(runID)
	if err != nil {
		return err
	}
	set, err := d.GetRunPublicationTargetSet(runID)
	if err != nil {
		return err
	}
	if set == nil || set.State != PublicationTargetSetComplete || set.TargetCount == 0 || set.TargetCount != len(targets) || set.Generation < 0 || strings.TrimSpace(set.Provenance) == "" || set.TargetSetHash != PublicationTargetSetHash(targets) {
		return ErrRunPublicationCAS
	}
	seen := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		if target.TargetKind == "" || target.TargetFingerprint == "" || target.Ref == "" || target.TargetVersion < 0 || target.Generation < 0 || strings.TrimSpace(target.Provenance) == "" || target.State != PublicationTargetNoAttempt || target.RequestIdentity != "" || target.AttemptHeadSHA != "" {
			return ErrRunPublicationCAS
		}
		if target.PRState != PublicationTargetPRNoAttempt || target.PRRequestIdentity != "" || target.PRGeneration < 0 || strings.TrimSpace(target.PRProvenance) == "" {
			return ErrRunPublicationCAS
		}
		if _, ok := seen[target.TargetFingerprint]; ok {
			return ErrRunPublicationCAS
		}
		seen[target.TargetFingerprint] = struct{}{}
	}
	return nil
}

func migrateRunPublicationTargets(sqlDB *sql.DB) error {
	rows, err := sqlDB.Query(`SELECT id, branch, publication_attempt_head_sha, publication_attempt_target_kind, publication_attempt_target_fingerprint, publication_attempt_ref, last_pushed_sha, push_target_kind, push_target_fingerprint, push_ref, publication_journal_target_version FROM runs WHERE NOT EXISTS (SELECT 1 FROM run_publication_target_sets WHERE run_publication_target_sets.run_id = runs.id)`)
	if err != nil {
		return err
	}
	var legacy []legacyPublicationRun
	for rows.Next() {
		var item legacyPublicationRun
		if err := rows.Scan(&item.id, &item.branch, &item.attemptHead, &item.attemptKind, &item.attemptFingerprint, &item.attemptRef, &item.pushedHead, &item.pushedKind, &item.pushedFingerprint, &item.pushedRef, &item.journalVersion); err != nil {
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
		var existingCount int
		if err := tx.QueryRow(`SELECT count(*) FROM run_publication_targets WHERE run_id = ?`, item.id).Scan(&existingCount); err != nil {
			return err
		}
		candidate, state, ok := legacySingletonTarget(item)
		if existingCount > 0 || !ok {
			if existingCount > 0 {
				if err := markPublicationTargetsAmbiguous(tx, item.id); err != nil {
					return err
				}
			}
			if err := insertPublicationTargetSetTx(tx, item.id, 0, publicationTargetSetHash(nil), PublicationTargetSetAmbiguous, "legacy-target-ambiguous"); err != nil {
				return err
			}
			continue
		}
		input := PublicationTargetInput{TargetKind: candidate.kind, TargetFingerprint: candidate.fingerprint, Ref: candidate.ref, TargetVersion: candidate.version}
		if err := seedRunPublicationTargetsTx(tx, item.id, []PublicationTargetInput{input}, "legacy-singleton"); err != nil {
			return err
		}
		if err := updateLegacySingletonState(tx, item.id, candidate, state); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

type legacyTargetCandidate struct {
	kind, fingerprint, ref, head string
	version                      int64
}

type legacyPublicationRun struct {
	id, branch                     string
	attemptHead, attemptKind       sql.NullString
	attemptFingerprint, attemptRef sql.NullString
	pushedHead, pushedKind         sql.NullString
	pushedFingerprint, pushedRef   sql.NullString
	journalVersion                 sql.NullInt64
}

func legacySingletonTarget(item legacyPublicationRun) (legacyTargetCandidate, string, bool) {
	attemptAny := anyLegacyPublicationField(item.attemptHead, item.attemptKind, item.attemptFingerprint, item.attemptRef)
	pushedAny := anyLegacyPublicationField(item.pushedHead, item.pushedKind, item.pushedFingerprint, item.pushedRef)
	attemptComplete := allLegacyPublicationFields(item.attemptHead, item.attemptKind, item.attemptFingerprint, item.attemptRef)
	pushedComplete := allLegacyPublicationFields(item.pushedHead, item.pushedKind, item.pushedFingerprint, item.pushedRef)
	if (attemptAny && !attemptComplete) || (pushedAny && !pushedComplete) || (!attemptComplete && !pushedComplete) {
		return legacyTargetCandidate{}, "", false
	}
	version := int64(0)
	if item.journalVersion.Valid {
		version = item.journalVersion.Int64
	}
	if attemptComplete && pushedComplete {
		if item.attemptKind.String != item.pushedKind.String || item.attemptFingerprint.String != item.pushedFingerprint.String || item.attemptRef.String != item.pushedRef.String {
			return legacyTargetCandidate{}, "", false
		}
		return legacyTargetCandidate{kind: item.pushedKind.String, fingerprint: item.pushedFingerprint.String, ref: item.pushedRef.String, head: item.pushedHead.String, version: version}, PublicationTargetPublished, true
	}
	if pushedComplete {
		return legacyTargetCandidate{kind: item.pushedKind.String, fingerprint: item.pushedFingerprint.String, ref: item.pushedRef.String, head: item.pushedHead.String, version: version}, PublicationTargetPublished, true
	}
	return legacyTargetCandidate{kind: item.attemptKind.String, fingerprint: item.attemptFingerprint.String, ref: item.attemptRef.String, head: item.attemptHead.String, version: version}, PublicationTargetAttempted, true
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

func updateLegacySingletonState(tx *sql.Tx, runID string, candidate legacyTargetCandidate, state string) error {
	result, err := tx.Exec(`UPDATE run_publication_targets SET state = ?, attempt_head_sha = ?, generation = generation + 1, provenance = ?, updated_at = ? WHERE run_id = ? AND target_fingerprint = ? AND state = ?`, state, candidate.head, "legacy-singleton", now(), runID, strings.TrimSpace(candidate.fingerprint), PublicationTargetNoAttempt)
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
