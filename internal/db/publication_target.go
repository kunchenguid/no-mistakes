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

	PublicationTargetPRNoAttempt                    = "no_attempt"
	PublicationTargetPRPrepared                     = "prepared"
	PublicationTargetPROpened                       = "opened"
	PublicationTargetPRAmbiguous                    = "ambiguous"
	PublicationTargetPRProvenanceMigrationPending   = "migration-pending"
	PublicationTargetPRProvenanceVerifiedNoAttempt  = "verified-no-attempt"
	PublicationTargetRequestLineageMigrationPending = "migration-pending"

	PublicationTargetSetComplete  = "complete"
	PublicationTargetSetAmbiguous = "ambiguous"
)

type PublicationTargetInput struct {
	TargetKind        string
	TargetFingerprint string
	Ref               string
	TargetVersion     int64
	RequestLineage    string
}

type RunPublicationTarget struct {
	RunID             string
	TargetKind        string
	TargetFingerprint string
	Ref               string
	TargetVersion     int64
	RequestLineage    string
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
	RunID              string
	TargetCount        int
	TargetSetHash      string
	State              string
	Generation         int64
	Provenance         string
	EvidenceHash       string
	EvidenceCursor     string
	EvidenceGeneration int64
	EvidenceProvenance string
}

type PublicationEvidenceInput struct {
	TargetFingerprint string
	Ref               string
	TargetVersion     int64
	RemoteHash        string
	ProviderHash      string
	Cursor            string
	Since             int64
	Until             int64
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
		input.RequestLineage = strings.TrimSpace(input.RequestLineage)
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
		parts = append(parts, strings.Join([]string{input.TargetKind, input.TargetFingerprint, input.Ref, fmt.Sprintf("%d", input.TargetVersion), input.RequestLineage}, "\x00"))
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
			RequestLineage:    target.RequestLineage,
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
	setErr := tx.QueryRow(`SELECT run_id, target_count, target_set_hash, state, generation, provenance, COALESCE(evidence_hash, ''), COALESCE(evidence_cursor, ''), COALESCE(evidence_generation, 0), COALESCE(evidence_provenance, '') FROM run_publication_target_sets WHERE run_id = ?`, runID).Scan(&existingSet.RunID, &existingSet.TargetCount, &existingSet.TargetSetHash, &existingSet.State, &existingSet.Generation, &existingSet.Provenance, &existingSet.EvidenceHash, &existingSet.EvidenceCursor, &existingSet.EvidenceGeneration, &existingSet.EvidenceProvenance)
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
		if _, err := tx.Exec(`INSERT INTO run_publication_targets (run_id, target_kind, target_fingerprint, ref, target_version, request_lineage, state, generation, provenance, pr_state, pr_generation, pr_provenance, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?, 0, ?, ?, ?)`, runID, input.TargetKind, input.TargetFingerprint, input.Ref, input.TargetVersion, input.RequestLineage, PublicationTargetNoAttempt, provenance, PublicationTargetPRNoAttempt, provenance, ts, ts); err != nil {
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
	rows, err := d.sql.Query(`SELECT run_id, target_kind, target_fingerprint, ref, target_version, COALESCE(request_lineage, ''), state, COALESCE(request_identity, ''), COALESCE(attempt_head_sha, ''), generation, provenance, pr_state, COALESCE(pr_request_identity, ''), pr_generation, pr_provenance FROM run_publication_targets WHERE run_id = ? ORDER BY target_fingerprint`, strings.TrimSpace(runID))
	if err != nil {
		return nil, fmt.Errorf("list run publication targets: %w", err)
	}
	defer rows.Close()
	var targets []RunPublicationTarget
	for rows.Next() {
		var target RunPublicationTarget
		if err := rows.Scan(&target.RunID, &target.TargetKind, &target.TargetFingerprint, &target.Ref, &target.TargetVersion, &target.RequestLineage, &target.State, &target.RequestIdentity, &target.AttemptHeadSHA, &target.Generation, &target.Provenance, &target.PRState, &target.PRRequestIdentity, &target.PRGeneration, &target.PRProvenance); err != nil {
			return nil, fmt.Errorf("scan run publication target: %w", err)
		}
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list run publication targets: %w", err)
	}
	return targets, nil
}

func (d *DB) ReconcileRunPublicationTargetLineage(runID, fingerprint, lineage string) error {
	runID = strings.TrimSpace(runID)
	fingerprint = strings.TrimSpace(fingerprint)
	lineage = strings.TrimSpace(lineage)
	if runID == "" || fingerprint == "" || lineage == "" {
		return ErrRunPublicationCAS
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("begin reconcile publication lineage: %w", err)
	}
	defer tx.Rollback()
	var custodyReturned sql.NullInt64
	var transitionToken, transitionPhase sql.NullString
	if err := tx.QueryRow(`SELECT custody_returned_at, custody_transition_token, custody_transition_phase FROM runs WHERE id = ?`, runID).Scan(&custodyReturned, &transitionToken, &transitionPhase); err != nil {
		return ErrRunCustodyCAS
	}
	if custodyReturned.Valid || transitionToken.Valid || transitionPhase.Valid {
		return ErrRunCustodyCAS
	}
	var current, state, prState string
	if err := tx.QueryRow(`SELECT COALESCE(request_lineage, ''), state, pr_state FROM run_publication_targets WHERE run_id = ? AND target_fingerprint = ?`, runID, fingerprint).Scan(&current, &state, &prState); err != nil {
		return ErrRunPublicationCAS
	}
	if current != "" && current != PublicationTargetRequestLineageMigrationPending {
		if current != lineage {
			return ErrRunPublicationCAS
		}
	}
	if state != PublicationTargetNoAttempt || prState != PublicationTargetPRNoAttempt {
		return ErrRunPublicationCAS
	}
	if current == "" || current == PublicationTargetRequestLineageMigrationPending {
		result, err := tx.Exec(`UPDATE run_publication_targets SET request_lineage = ?, generation = generation + 1, provenance = ?, updated_at = ? WHERE run_id = ? AND target_fingerprint = ? AND state = ? AND pr_state = ? AND (COALESCE(request_lineage, '') = '' OR request_lineage = ?)`, lineage, "legacy-lineage-reconciled", now(), runID, fingerprint, PublicationTargetNoAttempt, PublicationTargetPRNoAttempt, PublicationTargetRequestLineageMigrationPending)
		if err != nil {
			return fmt.Errorf("reconcile publication lineage: %w", err)
		}
		if rows, err := result.RowsAffected(); err != nil || rows != 1 {
			return ErrRunPublicationCAS
		}
	}
	targets, err := listPublicationTargetsTx(tx, runID)
	if err != nil {
		return err
	}
	var targetCount int
	var setState, currentHash string
	if err := tx.QueryRow(`SELECT target_count, state, target_set_hash FROM run_publication_target_sets WHERE run_id = ?`, runID).Scan(&targetCount, &setState, &currentHash); err != nil || setState != PublicationTargetSetComplete || targetCount != len(targets) || targetCount == 0 {
		return ErrRunPublicationCAS
	}
	newHash := PublicationTargetSetHash(targets)
	if currentHash == newHash {
		return tx.Commit()
	}
	if _, err := tx.Exec(`UPDATE run_publication_target_sets SET target_set_hash = ?, generation = generation + 1, updated_at = ? WHERE run_id = ? AND state = ? AND target_count = ?`, newHash, now(), runID, PublicationTargetSetComplete, targetCount); err != nil {
		return fmt.Errorf("update reconciled publication target set: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit reconciled publication lineage: %w", err)
	}
	return nil
}

func (d *DB) GetRunPublicationTargetSet(runID string) (*RunPublicationTargetSet, error) {
	var set RunPublicationTargetSet
	err := d.sql.QueryRow(`SELECT run_id, target_count, target_set_hash, state, generation, provenance, COALESCE(evidence_hash, ''), COALESCE(evidence_cursor, ''), COALESCE(evidence_generation, 0), COALESCE(evidence_provenance, '') FROM run_publication_target_sets WHERE run_id = ?`, strings.TrimSpace(runID)).Scan(&set.RunID, &set.TargetCount, &set.TargetSetHash, &set.State, &set.Generation, &set.Provenance, &set.EvidenceHash, &set.EvidenceCursor, &set.EvidenceGeneration, &set.EvidenceProvenance)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read run publication target set: %w", err)
	}
	return &set, nil
}

func validateRunPublicationTargetLedgerTx(tx *sql.Tx, runID string, requireVerified bool) error {
	var set RunPublicationTargetSet
	if err := tx.QueryRow(`SELECT run_id, target_count, target_set_hash, state, generation, provenance, COALESCE(evidence_hash, ''), COALESCE(evidence_cursor, ''), COALESCE(evidence_generation, 0), COALESCE(evidence_provenance, '') FROM run_publication_target_sets WHERE run_id = ?`, strings.TrimSpace(runID)).Scan(&set.RunID, &set.TargetCount, &set.TargetSetHash, &set.State, &set.Generation, &set.Provenance, &set.EvidenceHash, &set.EvidenceCursor, &set.EvidenceGeneration, &set.EvidenceProvenance); err != nil {
		return ErrRunPublicationCAS
	}
	rows, err := tx.Query(`SELECT run_id, target_kind, target_fingerprint, ref, target_version, COALESCE(request_lineage, ''), state, COALESCE(request_identity, ''), COALESCE(attempt_head_sha, ''), generation, provenance, pr_state, COALESCE(pr_request_identity, ''), pr_generation, pr_provenance FROM run_publication_targets WHERE run_id = ? ORDER BY target_fingerprint`, strings.TrimSpace(runID))
	if err != nil {
		return fmt.Errorf("read publication target ledger: %w", err)
	}
	defer rows.Close()
	var targets []RunPublicationTarget
	for rows.Next() {
		var target RunPublicationTarget
		if err := rows.Scan(&target.RunID, &target.TargetKind, &target.TargetFingerprint, &target.Ref, &target.TargetVersion, &target.RequestLineage, &target.State, &target.RequestIdentity, &target.AttemptHeadSHA, &target.Generation, &target.Provenance, &target.PRState, &target.PRRequestIdentity, &target.PRGeneration, &target.PRProvenance); err != nil {
			return fmt.Errorf("scan publication target ledger: %w", err)
		}
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read publication target ledger: %w", err)
	}
	if set.State != PublicationTargetSetComplete || set.TargetCount == 0 || set.TargetCount != len(targets) || set.Generation < 0 || strings.TrimSpace(set.Provenance) == "" || set.TargetSetHash != PublicationTargetSetHash(targets) {
		return ErrRunPublicationCAS
	}
	seen := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		if target.TargetKind == "" || target.TargetFingerprint == "" || target.Ref == "" || target.TargetVersion < 0 || target.Generation < 0 || strings.TrimSpace(target.Provenance) == "" || target.State != PublicationTargetNoAttempt || target.RequestIdentity != "" || target.AttemptHeadSHA != "" {
			return ErrRunPublicationCAS
		}
		if target.PRState != PublicationTargetPRNoAttempt || target.PRRequestIdentity != "" || target.PRGeneration < 0 || strings.TrimSpace(target.PRProvenance) == "" || requireVerified && target.PRProvenance == PublicationTargetPRProvenanceMigrationPending {
			return ErrRunPublicationCAS
		}
		if requireVerified && (strings.TrimSpace(target.RequestLineage) == "" || target.RequestLineage == PublicationTargetRequestLineageMigrationPending) {
			return ErrRunPublicationCAS
		}
		if _, ok := seen[target.TargetFingerprint]; ok {
			return ErrRunPublicationCAS
		}
		seen[target.TargetFingerprint] = struct{}{}
	}
	return nil
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
	var custodyReturned sql.NullInt64
	var transitionToken, transitionPhase sql.NullString
	if err := tx.QueryRow(`SELECT custody_returned_at, custody_transition_token, custody_transition_phase FROM runs WHERE id = ?`, runID).Scan(&custodyReturned, &transitionToken, &transitionPhase); err == sql.ErrNoRows {
		return ErrRunCustodyCAS
	} else if err != nil {
		return fmt.Errorf("read publication custody authority: %w", err)
	}
	if custodyReturned.Valid || transitionToken.Valid || transitionPhase.Valid {
		return ErrRunCustodyCAS
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
	if _, err := tx.Exec(`UPDATE run_publication_target_sets SET generation = generation + 1, updated_at = ? WHERE run_id = ?`, now(), runID); err != nil {
		return fmt.Errorf("advance publication target set generation: %w", err)
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
	var custodyReturned sql.NullInt64
	var transitionToken, transitionPhase sql.NullString
	if err := tx.QueryRow(`SELECT custody_returned_at, custody_transition_token, custody_transition_phase FROM runs WHERE id = ?`, runID).Scan(&custodyReturned, &transitionToken, &transitionPhase); err == sql.ErrNoRows {
		return ErrRunCustodyCAS
	} else if err != nil {
		return fmt.Errorf("read publication custody authority: %w", err)
	}
	if custodyReturned.Valid || transitionToken.Valid || transitionPhase.Valid {
		return ErrRunCustodyCAS
	}
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
	if _, err := tx.Exec(`UPDATE run_publication_target_sets SET generation = generation + 1, updated_at = ? WHERE run_id = ?`, now(), runID); err != nil {
		return fmt.Errorf("advance publication target set generation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit publication target identity: %w", err)
	}
	return nil
}

func (d *DB) MarkRunPublicationTargetsVerifiedNoAttempt(runID string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("begin verify publication target ledger: %w", err)
	}
	defer tx.Rollback()
	if err := validateRunPublicationTargetLedgerTx(tx, runID, false); err != nil {
		return err
	}
	result, err := tx.Exec(`UPDATE run_publication_targets SET pr_provenance = ?, pr_generation = pr_generation + 1, updated_at = ? WHERE run_id = ? AND state = ? AND pr_state = ? AND pr_provenance = ?`, PublicationTargetPRProvenanceVerifiedNoAttempt, now(), strings.TrimSpace(runID), PublicationTargetNoAttempt, PublicationTargetPRNoAttempt, PublicationTargetPRProvenanceMigrationPending)
	if err != nil {
		return fmt.Errorf("verify publication target ledger: %w", err)
	}
	if rows, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("verify publication target ledger: affected rows: %w", err)
	} else if rows > 0 {
		if _, err := tx.Exec(`UPDATE run_publication_target_sets SET generation = generation + 1, updated_at = ? WHERE run_id = ?`, now(), strings.TrimSpace(runID)); err != nil {
			return fmt.Errorf("advance verified publication target set generation: %w", err)
		}
	}
	if err := validateRunPublicationTargetLedgerTx(tx, runID, true); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit verified publication target ledger: %w", err)
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

func (d *DB) RecordRunPublicationEvidence(runID string, inputs []PublicationEvidenceInput) (*RunPublicationTargetSet, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" || len(inputs) == 0 {
		return nil, ErrRunPublicationCAS
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin record publication evidence: %w", err)
	}
	defer tx.Rollback()
	if err := validateRunPublicationTargetLedgerTx(tx, runID, true); err != nil {
		return nil, err
	}
	var set RunPublicationTargetSet
	if err := tx.QueryRow(`SELECT run_id, target_count, target_set_hash, state, generation, provenance, COALESCE(evidence_hash, ''), COALESCE(evidence_cursor, ''), COALESCE(evidence_generation, 0), COALESCE(evidence_provenance, '') FROM run_publication_target_sets WHERE run_id = ?`, runID).Scan(&set.RunID, &set.TargetCount, &set.TargetSetHash, &set.State, &set.Generation, &set.Provenance, &set.EvidenceHash, &set.EvidenceCursor, &set.EvidenceGeneration, &set.EvidenceProvenance); err != nil {
		return nil, ErrRunPublicationCAS
	}
	targets, err := listPublicationTargetsTx(tx, runID)
	if err != nil {
		return nil, err
	}
	byFingerprint := make(map[string]RunPublicationTarget, len(targets))
	for _, target := range targets {
		byFingerprint[target.TargetFingerprint] = target
	}
	if len(inputs) != len(targets) || set.EvidenceGeneration < 0 {
		return nil, ErrRunPublicationCAS
	}
	seen := make(map[string]struct{}, len(inputs))
	parts := make([]string, 0, len(inputs))
	cursors := make([]string, 0, len(inputs))
	generation := set.EvidenceGeneration + 1
	ts := now()
	for _, input := range inputs {
		input.TargetFingerprint = strings.TrimSpace(input.TargetFingerprint)
		input.Ref = strings.TrimSpace(input.Ref)
		input.RemoteHash = strings.TrimSpace(input.RemoteHash)
		input.ProviderHash = strings.TrimSpace(input.ProviderHash)
		input.Cursor = strings.TrimSpace(input.Cursor)
		if input.TargetFingerprint == "" || input.Ref == "" || input.TargetVersion < 0 || input.RemoteHash == "" || input.ProviderHash == "" || input.Cursor == "" || input.Since <= 0 || input.Until < input.Since {
			return nil, ErrRunPublicationCAS
		}
		target, ok := byFingerprint[input.TargetFingerprint]
		if !ok || target.Ref != input.Ref || target.TargetVersion != input.TargetVersion {
			return nil, ErrRunPublicationCAS
		}
		if _, ok := seen[input.TargetFingerprint]; ok {
			return nil, ErrRunPublicationCAS
		}
		seen[input.TargetFingerprint] = struct{}{}
		perTarget := sha256.Sum256([]byte(strings.Join([]string{input.TargetFingerprint, input.Ref, fmt.Sprintf("%d", input.TargetVersion), input.RemoteHash, input.ProviderHash, input.Cursor, fmt.Sprintf("%d", input.Since), fmt.Sprintf("%d", input.Until)}, "\x00")))
		perTargetHash := hex.EncodeToString(perTarget[:])
		parts = append(parts, strings.Join([]string{input.TargetFingerprint, input.Ref, perTargetHash}, "\x00"))
		cursors = append(cursors, input.TargetFingerprint+"="+input.Cursor)
		if _, err := tx.Exec(`INSERT INTO run_publication_evidence (run_id, target_fingerprint, ref, target_version, remote_hash, provider_hash, evidence_hash, cursor, since, until, generation, provenance, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(run_id, target_fingerprint) DO UPDATE SET ref = excluded.ref, target_version = excluded.target_version, remote_hash = excluded.remote_hash, provider_hash = excluded.provider_hash, evidence_hash = excluded.evidence_hash, cursor = excluded.cursor, since = excluded.since, until = excluded.until, generation = excluded.generation, provenance = excluded.provenance, updated_at = excluded.updated_at`, runID, input.TargetFingerprint, input.Ref, input.TargetVersion, input.RemoteHash, input.ProviderHash, perTargetHash, input.Cursor, input.Since, input.Until, generation, "target-scoped-complete-v1", ts, ts); err != nil {
			return nil, fmt.Errorf("record publication evidence: %w", err)
		}
	}
	sort.Strings(parts)
	sort.Strings(cursors)
	snapshot := sha256.Sum256([]byte(strings.Join(parts, "\x01")))
	set.EvidenceHash = hex.EncodeToString(snapshot[:])
	set.EvidenceCursor = strings.Join(cursors, ",")
	set.EvidenceGeneration = generation
	set.EvidenceProvenance = "target-scoped-complete-v1"
	if _, err := tx.Exec(`UPDATE run_publication_target_sets SET evidence_hash = ?, evidence_cursor = ?, evidence_generation = ?, evidence_provenance = ?, updated_at = ? WHERE run_id = ?`, set.EvidenceHash, set.EvidenceCursor, set.EvidenceGeneration, set.EvidenceProvenance, ts, runID); err != nil {
		return nil, fmt.Errorf("update publication evidence snapshot: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit publication evidence: %w", err)
	}
	return &set, nil
}

func listPublicationTargetsTx(tx *sql.Tx, runID string) ([]RunPublicationTarget, error) {
	rows, err := tx.Query(`SELECT run_id, target_kind, target_fingerprint, ref, target_version, COALESCE(request_lineage, ''), state, COALESCE(request_identity, ''), COALESCE(attempt_head_sha, ''), generation, provenance, pr_state, COALESCE(pr_request_identity, ''), pr_generation, pr_provenance FROM run_publication_targets WHERE run_id = ? ORDER BY target_fingerprint`, runID)
	if err != nil {
		return nil, fmt.Errorf("read publication targets: %w", err)
	}
	defer rows.Close()
	var targets []RunPublicationTarget
	for rows.Next() {
		var target RunPublicationTarget
		if err := rows.Scan(&target.RunID, &target.TargetKind, &target.TargetFingerprint, &target.Ref, &target.TargetVersion, &target.RequestLineage, &target.State, &target.RequestIdentity, &target.AttemptHeadSHA, &target.Generation, &target.Provenance, &target.PRState, &target.PRRequestIdentity, &target.PRGeneration, &target.PRProvenance); err != nil {
			return nil, fmt.Errorf("scan publication target: %w", err)
		}
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read publication targets: %w", err)
	}
	return targets, nil
}

func validateRunPublicationEvidenceTx(tx *sql.Tx, expected *Run) error {
	if expected == nil || strings.TrimSpace(expected.PublicationEvidenceHash) == "" || expected.PublicationEvidenceGeneration <= 0 {
		return ErrRunCustodyCAS
	}
	var hash, cursor, provenance string
	var generation int64
	var targetCount int
	if err := tx.QueryRow(`SELECT COALESCE(evidence_hash, ''), COALESCE(evidence_cursor, ''), COALESCE(evidence_generation, 0), COALESCE(evidence_provenance, ''), target_count FROM run_publication_target_sets WHERE run_id = ?`, expected.ID).Scan(&hash, &cursor, &generation, &provenance, &targetCount); err != nil {
		return ErrRunCustodyCAS
	}
	if hash == "" || cursor == "" || targetCount <= 0 || hash != expected.PublicationEvidenceHash || generation != expected.PublicationEvidenceGeneration || generation <= 0 || provenance != "target-scoped-complete-v1" {
		return ErrRunCustodyCAS
	}
	var count int
	if err := tx.QueryRow(`SELECT count(*) FROM run_publication_evidence WHERE run_id = ? AND generation = ? AND provenance = ?`, expected.ID, expected.PublicationEvidenceGeneration, "target-scoped-complete-v1").Scan(&count); err != nil {
		return ErrRunCustodyCAS
	}
	if count != targetCount || count == 0 {
		return ErrRunCustodyCAS
	}
	var invalid int
	if err := tx.QueryRow(`SELECT count(*) FROM run_publication_evidence AS evidence JOIN run_publication_targets AS target ON target.run_id = evidence.run_id AND target.target_fingerprint = evidence.target_fingerprint WHERE evidence.run_id = ? AND (evidence.ref <> target.ref OR evidence.target_version <> target.target_version OR evidence.evidence_hash = '' OR evidence.remote_hash = '' OR evidence.provider_hash = '' OR evidence.cursor = '' OR evidence.cursor NOT LIKE '%audit%' OR evidence.cursor NOT LIKE '%hasNextPage=false%' OR evidence.cursor NOT LIKE '%audit-cutoff=%' OR evidence.cursor NOT LIKE '%provider-date:%' OR evidence.since <= 0 OR evidence.until < evidence.since)`, expected.ID).Scan(&invalid); err != nil || invalid != 0 {
		return ErrRunCustodyCAS
	}
	return nil
}

func migrateRunPublicationTargets(sqlDB *sql.DB) error {
	if err := reconcileLegacyPublicationTargetMetadata(sqlDB); err != nil {
		return err
	}
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
		input := PublicationTargetInput{TargetKind: candidate.kind, TargetFingerprint: candidate.fingerprint, Ref: candidate.ref, TargetVersion: candidate.version, RequestLineage: PublicationTargetRequestLineageMigrationPending}
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

func reconcileLegacyPublicationTargetMetadata(sqlDB *sql.DB) error {
	tx, err := sqlDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE run_publication_targets SET pr_provenance = ? WHERE pr_state = ? AND pr_provenance = ''`, PublicationTargetPRProvenanceMigrationPending, PublicationTargetPRNoAttempt); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE run_publication_targets SET request_lineage = ? WHERE state = ? AND pr_state = ? AND COALESCE(request_lineage, '') = ''`, PublicationTargetRequestLineageMigrationPending, PublicationTargetNoAttempt, PublicationTargetPRNoAttempt); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT run_id FROM run_publication_target_sets WHERE state = ?`, PublicationTargetSetComplete)
	if err != nil {
		return err
	}
	var runIDs []string
	for rows.Next() {
		var runID string
		if err := rows.Scan(&runID); err != nil {
			rows.Close()
			return err
		}
		runIDs = append(runIDs, runID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, runID := range runIDs {
		targets, err := listPublicationTargetsTx(tx, runID)
		if err != nil {
			return err
		}
		var targetCount int
		var currentHash string
		if err := tx.QueryRow(`SELECT target_count, target_set_hash FROM run_publication_target_sets WHERE run_id = ?`, runID).Scan(&targetCount, &currentHash); err != nil {
			return err
		}
		newHash := PublicationTargetSetHash(targets)
		if targetCount != len(targets) || currentHash == newHash {
			continue
		}
		if _, err := tx.Exec(`UPDATE run_publication_target_sets SET target_set_hash = ?, generation = generation + 1, updated_at = ? WHERE run_id = ? AND state = ?`, newHash, now(), runID, PublicationTargetSetComplete); err != nil {
			return err
		}
	}
	return tx.Commit()
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
