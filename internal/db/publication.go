package db

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/buildinfo"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const PublicationDenialErrorPrefix = "publication denied: "

var (
	ErrPublicationIDMismatch              = errors.New("publication id does not match canonical request")
	ErrPublicationCollision               = errors.New("publication id collision")
	ErrPublicationRunConflict             = errors.New("active run conflicts with publication")
	ErrPublicationEffectConflict          = errors.New("publication effect binding conflict")
	ErrPublicationAuthorizationRequired   = errors.New("publication effect authorization required")
	ErrPublicationAuthorizationNotAllowed = errors.New("publication effect authorization not allowed")
	ErrPublicationAuthorizationMismatch   = errors.New("publication effect authorization mismatch")
	ErrPublicationDecisionConsumed        = errors.New("publication effect decision already consumed")
	ErrPublicationEffectTransition        = errors.New("invalid publication effect transition")
)

// CreatePublicationInput is the immutable persistence projection of a
// canonical Factory publication request. CanonicalRequest is stored byte for
// byte; PublicationID must be its lowercase SHA-256.
type CreatePublicationInput struct {
	PublicationID    string
	CanonicalRequest []byte
	RepoID           string
	CandidateRef     string
	BaseRef          string
	HeadSHA          string
	BaseSHA          string
	TreeSHA          string
}

// Publication binds one content-addressed request to exactly one Run.
type Publication struct {
	PublicationID    string
	RunID            string
	CanonicalRequest []byte
	RepoID           string
	CandidateRef     string
	BaseRef          string
	HeadSHA          string
	BaseSHA          string
	TreeSHA          string
	CreatedAt        int64
	UpdatedAt        int64
}

const publicationColumns = `publication_id, run_id, canonical_request, repo_id, candidate_ref, base_ref, head_sha, base_sha, tree_sha, created_at, updated_at`

func scanPublication(row interface{ Scan(...any) error }, publication *Publication) error {
	return row.Scan(
		&publication.PublicationID, &publication.RunID, &publication.CanonicalRequest,
		&publication.RepoID, &publication.CandidateRef, &publication.BaseRef,
		&publication.HeadSHA, &publication.BaseSHA, &publication.TreeSHA,
		&publication.CreatedAt, &publication.UpdatedAt,
	)
}

func validatePublicationInput(input CreatePublicationInput) error {
	if len(input.CanonicalRequest) == 0 || input.RepoID == "" || input.CandidateRef == "" ||
		input.BaseRef == "" || input.HeadSHA == "" || input.BaseSHA == "" || input.TreeSHA == "" {
		return fmt.Errorf("invalid publication binding: %w", ErrPublicationCollision)
	}
	return nil
}

func publicationMatchesInput(publication *Publication, input CreatePublicationInput) bool {
	return publication != nil &&
		publication.PublicationID == input.PublicationID &&
		bytes.Equal(publication.CanonicalRequest, input.CanonicalRequest) &&
		publication.RepoID == input.RepoID &&
		publication.CandidateRef == input.CandidateRef &&
		publication.BaseRef == input.BaseRef &&
		publication.HeadSHA == input.HeadSHA &&
		publication.BaseSHA == input.BaseSHA &&
		publication.TreeSHA == input.TreeSHA
}

func equivalentRunBranches(candidateRef string) (string, string) {
	if short := strings.TrimPrefix(candidateRef, "refs/heads/"); short != candidateRef {
		return candidateRef, short
	}
	return candidateRef, "refs/heads/" + candidateRef
}

// CreateOrGetPublication atomically creates the Publication, its dedicated
// Run, and the existing executor's nine pending step rows. An identical retry
// reconciles the same rows; it never attaches an ordinary AXI Run.
func (d *DB) CreateOrGetPublication(input CreatePublicationInput) (*Publication, *Run, bool, error) {
	if err := validatePublicationInput(input); err != nil {
		return nil, nil, false, err
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, nil, false, fmt.Errorf("create publication: begin: %w", err)
	}
	defer tx.Rollback()

	existing := &Publication{}
	err = scanPublication(tx.QueryRow(`SELECT `+publicationColumns+` FROM publications WHERE publication_id = ?`, input.PublicationID), existing)
	if err == nil {
		if !publicationMatchesInput(existing, input) {
			return nil, nil, false, ErrPublicationCollision
		}
		run := &Run{}
		if err := scanRun(tx.QueryRow(`SELECT `+runColumns+` FROM runs WHERE id = ?`, existing.RunID), run); err != nil {
			return nil, nil, false, fmt.Errorf("create publication: read associated run: %w", err)
		}
		if run.Kind != RunKindFactoryPublicationV1 {
			return nil, nil, false, ErrPublicationCollision
		}
		if err := tx.Commit(); err != nil {
			return nil, nil, false, fmt.Errorf("create publication: commit reconciliation: %w", err)
		}
		existing.CanonicalRequest = append([]byte(nil), existing.CanonicalRequest...)
		return existing, run, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, false, fmt.Errorf("create publication: read existing: %w", err)
	}
	digest := sha256.Sum256(input.CanonicalRequest)
	if input.PublicationID != fmt.Sprintf("%x", digest) {
		return nil, nil, false, ErrPublicationIDMismatch
	}

	branchA, branchB := equivalentRunBranches(input.CandidateRef)
	var conflictingID string
	err = tx.QueryRow(
		`SELECT id FROM runs WHERE repo_id = ? AND branch IN (?, ?) AND status IN (?, ?) ORDER BY created_at DESC, id DESC LIMIT 1`,
		input.RepoID, branchA, branchB, types.RunPending, types.RunRunning,
	).Scan(&conflictingID)
	if err == nil {
		return nil, nil, false, fmt.Errorf("%w: run %s", ErrPublicationRunConflict, conflictingID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, false, fmt.Errorf("create publication: inspect active runs: %w", err)
	}

	ts := now()
	version := buildinfo.CurrentVersion()
	buildSHA := buildinfo.Commit
	run := &Run{
		ID:                 newID(),
		Kind:               RunKindFactoryPublicationV1,
		RepoID:             input.RepoID,
		Branch:             input.CandidateRef,
		HeadSHA:            input.HeadSHA,
		BaseSHA:            input.BaseSHA,
		SubmittedHeadSHA:   stringPtr(input.HeadSHA),
		NoMistakesVersion:  stringPtr(version),
		NoMistakesBuildSHA: stringPtr(buildSHA),
		Status:             types.RunPending,
		CreatedAt:          ts,
		UpdatedAt:          ts,
	}
	if _, err := tx.Exec(
		`INSERT INTO runs (id, run_kind, repo_id, branch, head_sha, base_sha, submitted_head_sha, no_mistakes_version, no_mistakes_build_sha, status, pr_state, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'none', ?, ?)`,
		run.ID, run.Kind, run.RepoID, run.Branch, run.HeadSHA, run.BaseSHA,
		input.HeadSHA, version, buildSHA, run.Status, ts, ts,
	); err != nil {
		return nil, nil, false, fmt.Errorf("create publication: insert run: %w", err)
	}

	publication := &Publication{
		PublicationID:    input.PublicationID,
		RunID:            run.ID,
		CanonicalRequest: append([]byte(nil), input.CanonicalRequest...),
		RepoID:           input.RepoID,
		CandidateRef:     input.CandidateRef,
		BaseRef:          input.BaseRef,
		HeadSHA:          input.HeadSHA,
		BaseSHA:          input.BaseSHA,
		TreeSHA:          input.TreeSHA,
		CreatedAt:        ts,
		UpdatedAt:        ts,
	}
	if _, err := tx.Exec(
		`INSERT INTO publications (`+publicationColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		publication.PublicationID, publication.RunID, publication.CanonicalRequest,
		publication.RepoID, publication.CandidateRef, publication.BaseRef,
		publication.HeadSHA, publication.BaseSHA, publication.TreeSHA,
		publication.CreatedAt, publication.UpdatedAt,
	); err != nil {
		return nil, nil, false, fmt.Errorf("create publication: insert binding: %w", err)
	}
	for _, stepName := range types.AllSteps() {
		if _, err := tx.Exec(
			`INSERT INTO step_results (id, run_id, step_name, step_order, status) VALUES (?, ?, ?, ?, ?)`,
			newID(), run.ID, stepName, stepName.Order(), types.StepStatusPending,
		); err != nil {
			return nil, nil, false, fmt.Errorf("create publication: seed %s step: %w", stepName, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, false, fmt.Errorf("create publication: commit: %w", err)
	}
	return publication, run, true, nil
}

func stringPtr(value string) *string { return &value }

// GetPublication returns the immutable publication binding by ID.
func (d *DB) GetPublication(publicationID string) (*Publication, error) {
	publication := &Publication{}
	err := scanPublication(d.sql.QueryRow(`SELECT `+publicationColumns+` FROM publications WHERE publication_id = ?`, publicationID), publication)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get publication: %w", err)
	}
	publication.CanonicalRequest = append([]byte(nil), publication.CanonicalRequest...)
	return publication, nil
}

// GetPublicationByRunID returns the one-to-one publication binding owned by a
// publication-profile Run. Startup recovery begins from durable Run rows, so
// this lookup avoids inferring a publication identity from mutable run data.
func (d *DB) GetPublicationByRunID(runID string) (*Publication, error) {
	publication := &Publication{}
	err := scanPublication(d.sql.QueryRow(`SELECT `+publicationColumns+` FROM publications WHERE run_id = ?`, runID), publication)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get publication by run id: %w", err)
	}
	publication.CanonicalRequest = append([]byte(nil), publication.CanonicalRequest...)
	return publication, nil
}

// ListRecoverablePublicationRuns returns only active publication-profile runs.
// Generic stale-run recovery deliberately excludes this set.
func (d *DB) ListRecoverablePublicationRuns() ([]*Run, error) {
	rows, err := d.sql.Query(
		`SELECT `+runColumns+` FROM runs
		 WHERE run_kind = ? AND status IN (?, ?)
		   AND EXISTS (SELECT 1 FROM publications WHERE publications.run_id = runs.id)
		 ORDER BY created_at DESC, id DESC`,
		RunKindFactoryPublicationV1, types.RunPending, types.RunRunning,
	)
	if err != nil {
		return nil, fmt.Errorf("list recoverable publication runs: %w", err)
	}
	defer rows.Close()
	var runs []*Run
	for rows.Next() {
		run := &Run{}
		if err := scanRun(rows, run); err != nil {
			return nil, fmt.Errorf("list recoverable publication runs: scan: %w", err)
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

type PublicationEffectKind string

const (
	PublicationEffectPush PublicationEffectKind = "push"
	PublicationEffectPR   PublicationEffectKind = "pr"
	PublicationEffectCI   PublicationEffectKind = "ci"
)

func (kind PublicationEffectKind) valid() bool {
	return kind == PublicationEffectPush || kind == PublicationEffectPR || kind == PublicationEffectCI
}

type PublicationEffectState string

const (
	PublicationEffectPlanned    PublicationEffectState = "planned"
	PublicationEffectAuthorized PublicationEffectState = "authorized"
	PublicationEffectObserved   PublicationEffectState = "observed"
	PublicationEffectUnknown    PublicationEffectState = "unknown"
	PublicationEffectFailed     PublicationEffectState = "failed"
)

func (state PublicationEffectState) terminal() bool {
	return state == PublicationEffectObserved || state == PublicationEffectUnknown || state == PublicationEffectFailed
}

// PublicationEffectBinding is the complete exact binding checked at every
// journal transition. Fields not applicable to an effect are the empty string.
type PublicationEffectBinding struct {
	CandidateSHA   string
	RemoteIdentity string
	DestinationRef string
	BaseRef        string
	HeadRef        string
	EffectDigest   string
	DraftDigest    string
}

type PublicationEffect struct {
	ID                 string
	PublicationID      string
	Kind               PublicationEffectKind
	State              PublicationEffectState
	Binding            PublicationEffectBinding
	PreparedPayload    []byte
	DecisionDigest     *string
	DecisionConsumedAt *int64
	EffectStartedAt    *int64
	Observation        []byte
	ObservedAt         *int64
	CreatedAt          int64
	UpdatedAt          int64
}

const publicationEffectColumns = `id, publication_id, effect_kind, effect_state, candidate_sha, remote_identity, destination_ref, base_ref, head_ref, effect_digest, draft_digest, prepared_payload, decision_digest, decision_consumed_at, effect_started_at, observation, observed_at, created_at, updated_at`

func scanPublicationEffect(row interface{ Scan(...any) error }, effect *PublicationEffect) error {
	return row.Scan(
		&effect.ID, &effect.PublicationID, &effect.Kind, &effect.State,
		&effect.Binding.CandidateSHA, &effect.Binding.RemoteIdentity,
		&effect.Binding.DestinationRef, &effect.Binding.BaseRef,
		&effect.Binding.HeadRef, &effect.Binding.EffectDigest,
		&effect.Binding.DraftDigest, &effect.PreparedPayload, &effect.DecisionDigest,
		&effect.DecisionConsumedAt, &effect.EffectStartedAt,
		&effect.Observation, &effect.ObservedAt, &effect.CreatedAt, &effect.UpdatedAt,
	)
}

type PlanPublicationEffectInput struct {
	PublicationID   string
	Kind            PublicationEffectKind
	Binding         PublicationEffectBinding
	PreparedPayload []byte
}

func (d *DB) PlanPublicationEffect(input PlanPublicationEffectInput) (*PublicationEffect, error) {
	if !input.Kind.valid() {
		return nil, ErrPublicationEffectConflict
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, fmt.Errorf("plan publication effect: begin: %w", err)
	}
	defer tx.Rollback()
	existing, err := getPublicationEffectTx(tx, input.PublicationID, input.Kind)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if existing.Binding != input.Binding || !bytes.Equal(existing.PreparedPayload, input.PreparedPayload) {
			return nil, ErrPublicationEffectConflict
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("plan publication effect: commit reconciliation: %w", err)
		}
		return existing, nil
	}
	ts := now()
	effect := &PublicationEffect{ID: newID(), PublicationID: input.PublicationID, Kind: input.Kind, State: PublicationEffectPlanned, Binding: input.Binding, PreparedPayload: append([]byte(nil), input.PreparedPayload...), CreatedAt: ts, UpdatedAt: ts}
	_, err = tx.Exec(
		`INSERT INTO publication_effects (`+publicationEffectColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, NULL, NULL, NULL, ?, ?)`,
		effect.ID, effect.PublicationID, effect.Kind, effect.State,
		effect.Binding.CandidateSHA, effect.Binding.RemoteIdentity, effect.Binding.DestinationRef,
		effect.Binding.BaseRef, effect.Binding.HeadRef, effect.Binding.EffectDigest,
		effect.Binding.DraftDigest, effect.PreparedPayload, ts, ts,
	)
	if err != nil {
		return nil, fmt.Errorf("plan publication effect: insert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("plan publication effect: commit: %w", err)
	}
	return effect, nil
}

func getPublicationEffectTx(tx *sql.Tx, publicationID string, kind PublicationEffectKind) (*PublicationEffect, error) {
	effect := &PublicationEffect{}
	err := scanPublicationEffect(tx.QueryRow(`SELECT `+publicationEffectColumns+` FROM publication_effects WHERE publication_id = ? AND effect_kind = ?`, publicationID, kind), effect)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get publication effect: %w", err)
	}
	effect.Observation = append([]byte(nil), effect.Observation...)
	effect.PreparedPayload = append([]byte(nil), effect.PreparedPayload...)
	return effect, nil
}

func (d *DB) GetPublicationEffect(publicationID string, kind PublicationEffectKind) (*PublicationEffect, error) {
	effect := &PublicationEffect{}
	err := scanPublicationEffect(d.sql.QueryRow(`SELECT `+publicationEffectColumns+` FROM publication_effects WHERE publication_id = ? AND effect_kind = ?`, publicationID, kind), effect)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get publication effect: %w", err)
	}
	effect.Observation = append([]byte(nil), effect.Observation...)
	effect.PreparedPayload = append([]byte(nil), effect.PreparedPayload...)
	return effect, nil
}

type AuthorizePublicationEffectInput struct {
	PublicationID  string
	Kind           PublicationEffectKind
	Binding        PublicationEffectBinding
	DecisionDigest string
}

// DenyPublicationEffectInput binds an Owner DENY to one exact planned Push or
// PR effect. The decision digest is the digest of the exact challenge carrying
// the DENY decision; it cannot be reused for another binding or effect kind.
type DenyPublicationEffectInput struct {
	PublicationID  string
	Kind           PublicationEffectKind
	Binding        PublicationEffectBinding
	DecisionDigest string
}

// DenyPublicationEffect atomically consumes an exact DENY without starting a
// provider effect and terminalizes the associated publication Run. An exact
// retry reconciles the same rows; GO and DENY states never transfer.
func (d *DB) DenyPublicationEffect(input DenyPublicationEffectInput) (*PublicationEffect, error) {
	if input.Kind != PublicationEffectPush && input.Kind != PublicationEffectPR {
		if input.Kind == PublicationEffectCI {
			return nil, ErrPublicationAuthorizationNotAllowed
		}
		return nil, ErrPublicationEffectConflict
	}
	if !validPublicationDecisionDigest(input.DecisionDigest) {
		return nil, ErrPublicationAuthorizationMismatch
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, fmt.Errorf("deny publication effect: begin: %w", err)
	}
	defer tx.Rollback()

	effect, err := getPublicationEffectTx(tx, input.PublicationID, input.Kind)
	if err != nil {
		return nil, err
	}
	if effect == nil || effect.Binding != input.Binding {
		return nil, ErrPublicationEffectConflict
	}

	var runID string
	var runKind RunKind
	var runStatus types.RunStatus
	var runError sql.NullString
	if err := tx.QueryRow(
		`SELECT publications.run_id, runs.run_kind, runs.status, runs.error
		 FROM publications JOIN runs ON runs.id = publications.run_id
		 WHERE publications.publication_id = ?`,
		input.PublicationID,
	).Scan(&runID, &runKind, &runStatus, &runError); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrPublicationEffectConflict
		}
		return nil, fmt.Errorf("deny publication effect: read Run: %w", err)
	}
	if runKind != RunKindFactoryPublicationV1 {
		return nil, ErrPublicationEffectConflict
	}
	denialMessage := PublicationDenialErrorPrefix + "Owner denied " + string(input.Kind) + " effect"

	if effect.State == PublicationEffectFailed && effect.DecisionConsumedAt != nil &&
		effect.EffectStartedAt == nil && effect.ObservedAt == nil && len(effect.Observation) == 0 {
		if effect.DecisionDigest == nil || *effect.DecisionDigest != input.DecisionDigest {
			return nil, ErrPublicationAuthorizationMismatch
		}
		if runStatus != types.RunFailed || !runError.Valid || runError.String != denialMessage {
			return nil, ErrPublicationEffectTransition
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("deny publication effect: commit reconciliation: %w", err)
		}
		return effect, nil
	}
	if effect.State != PublicationEffectPlanned {
		return nil, ErrPublicationEffectTransition
	}
	if effect.DecisionDigest != nil || effect.DecisionConsumedAt != nil || effect.EffectStartedAt != nil ||
		effect.ObservedAt != nil || len(effect.Observation) != 0 {
		return nil, ErrPublicationDecisionConsumed
	}
	if runStatus != types.RunPending && runStatus != types.RunRunning {
		return nil, ErrPublicationEffectTransition
	}

	ts := now()
	result, err := tx.Exec(
		`UPDATE publication_effects
		 SET effect_state = ?, decision_digest = ?, decision_consumed_at = ?, updated_at = ?
		 WHERE id = ? AND effect_state = ? AND decision_digest IS NULL
		   AND decision_consumed_at IS NULL AND effect_started_at IS NULL`,
		PublicationEffectFailed, input.DecisionDigest, ts, ts, effect.ID, PublicationEffectPlanned,
	)
	if err != nil {
		return nil, fmt.Errorf("deny publication effect: terminalize effect: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return nil, fmt.Errorf("deny publication effect: inspect effect update: %w", err)
	} else if affected != 1 {
		return nil, ErrPublicationEffectTransition
	}
	result, err = tx.Exec(
		`UPDATE runs SET status = ?, error = ?, updated_at = ?
		 WHERE id = ? AND run_kind = ? AND status IN (?, ?)`,
		types.RunFailed, denialMessage, ts, runID, RunKindFactoryPublicationV1, types.RunPending, types.RunRunning,
	)
	if err != nil {
		return nil, fmt.Errorf("deny publication effect: fail Run: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return nil, fmt.Errorf("deny publication effect: inspect Run update: %w", err)
	} else if affected != 1 {
		return nil, ErrPublicationEffectTransition
	}

	effect, err = getPublicationEffectTx(tx, input.PublicationID, input.Kind)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("deny publication effect: commit: %w", err)
	}
	return effect, nil
}

func validPublicationDecisionDigest(digest string) bool {
	if len(digest) != sha256.Size*2 || strings.ToLower(digest) != digest {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

func (d *DB) AuthorizePublicationEffect(input AuthorizePublicationEffectInput) (*PublicationEffect, error) {
	if input.Kind == PublicationEffectCI {
		return nil, ErrPublicationAuthorizationNotAllowed
	}
	if input.Kind != PublicationEffectPush && input.Kind != PublicationEffectPR {
		return nil, ErrPublicationEffectConflict
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, fmt.Errorf("authorize publication effect: begin: %w", err)
	}
	defer tx.Rollback()
	effect, err := getPublicationEffectTx(tx, input.PublicationID, input.Kind)
	if err != nil {
		return nil, err
	}
	if effect == nil || effect.Binding != input.Binding {
		return nil, ErrPublicationEffectConflict
	}
	if effect.DecisionConsumedAt != nil || effect.EffectStartedAt != nil {
		return nil, ErrPublicationDecisionConsumed
	}
	if input.DecisionDigest == "" {
		return nil, ErrPublicationAuthorizationMismatch
	}
	if effect.State == PublicationEffectAuthorized {
		if effect.DecisionDigest == nil || *effect.DecisionDigest != input.DecisionDigest {
			return nil, ErrPublicationAuthorizationMismatch
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("authorize publication effect: commit reconciliation: %w", err)
		}
		return effect, nil
	}
	if effect.State != PublicationEffectPlanned {
		return nil, ErrPublicationEffectTransition
	}
	if _, err := tx.Exec(
		`UPDATE publication_effects SET effect_state = ?, decision_digest = ?, updated_at = ? WHERE id = ?`,
		PublicationEffectAuthorized, input.DecisionDigest, now(), effect.ID,
	); err != nil {
		return nil, fmt.Errorf("authorize publication effect: update: %w", err)
	}
	effect, err = getPublicationEffectTx(tx, input.PublicationID, input.Kind)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("authorize publication effect: commit: %w", err)
	}
	return effect, nil
}

type BeginPublicationEffectInput struct {
	PublicationID  string
	Kind           PublicationEffectKind
	Binding        PublicationEffectBinding
	DecisionDigest string
}

func (d *DB) BeginPublicationEffect(input BeginPublicationEffectInput) (*PublicationEffect, error) {
	if !input.Kind.valid() {
		return nil, ErrPublicationEffectConflict
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin publication effect: begin transaction: %w", err)
	}
	defer tx.Rollback()
	effect, err := getPublicationEffectTx(tx, input.PublicationID, input.Kind)
	if err != nil {
		return nil, err
	}
	if effect == nil {
		return nil, ErrPublicationEffectConflict
	}
	if effect.Binding != input.Binding {
		if input.Kind != PublicationEffectCI && effect.State == PublicationEffectAuthorized && effect.DecisionConsumedAt == nil {
			ts := now()
			if _, updateErr := tx.Exec(`UPDATE publication_effects SET effect_state = ?, decision_consumed_at = ?, updated_at = ? WHERE id = ?`, PublicationEffectFailed, ts, ts, effect.ID); updateErr != nil {
				return nil, fmt.Errorf("begin publication effect: invalidate mismatched decision: %w", updateErr)
			}
			if commitErr := tx.Commit(); commitErr != nil {
				return nil, fmt.Errorf("begin publication effect: commit invalidation: %w", commitErr)
			}
		}
		return nil, ErrPublicationEffectConflict
	}
	if effect.State.terminal() {
		return nil, ErrPublicationEffectTransition
	}
	if input.Kind == PublicationEffectCI {
		if input.DecisionDigest != "" {
			return nil, ErrPublicationAuthorizationNotAllowed
		}
		if effect.EffectStartedAt == nil {
			if _, err := tx.Exec(`UPDATE publication_effects SET effect_started_at = ?, updated_at = ? WHERE id = ?`, now(), now(), effect.ID); err != nil {
				return nil, fmt.Errorf("begin publication effect: start CI observation: %w", err)
			}
		}
	} else {
		if effect.State == PublicationEffectPlanned || effect.DecisionDigest == nil {
			return nil, ErrPublicationAuthorizationRequired
		}
		if effect.DecisionConsumedAt != nil || effect.EffectStartedAt != nil {
			return nil, ErrPublicationDecisionConsumed
		}
		if *effect.DecisionDigest != input.DecisionDigest {
			return nil, ErrPublicationAuthorizationMismatch
		}
		ts := now()
		if _, err := tx.Exec(`UPDATE publication_effects SET decision_consumed_at = ?, effect_started_at = ?, updated_at = ? WHERE id = ?`, ts, ts, ts, effect.ID); err != nil {
			return nil, fmt.Errorf("begin publication effect: consume decision: %w", err)
		}
	}
	effect, err = getPublicationEffectTx(tx, input.PublicationID, input.Kind)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("begin publication effect: commit: %w", err)
	}
	return effect, nil
}

type ConcludePublicationEffectInput struct {
	PublicationID string
	Kind          PublicationEffectKind
	Binding       PublicationEffectBinding
	State         PublicationEffectState
	Observation   []byte
}

func (d *DB) ConcludePublicationEffect(input ConcludePublicationEffectInput) (*PublicationEffect, error) {
	if !input.State.terminal() {
		return nil, ErrPublicationEffectTransition
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, fmt.Errorf("conclude publication effect: begin: %w", err)
	}
	defer tx.Rollback()
	effect, err := getPublicationEffectTx(tx, input.PublicationID, input.Kind)
	if err != nil {
		return nil, err
	}
	if effect == nil || effect.Binding != input.Binding {
		return nil, ErrPublicationEffectConflict
	}
	if effect.EffectStartedAt == nil {
		return nil, ErrPublicationEffectTransition
	}
	if effect.State.terminal() {
		if effect.State != input.State || !bytes.Equal(effect.Observation, input.Observation) {
			return nil, ErrPublicationEffectTransition
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("conclude publication effect: commit reconciliation: %w", err)
		}
		return effect, nil
	}
	ts := now()
	if _, err := tx.Exec(
		`UPDATE publication_effects SET effect_state = ?, observation = ?, observed_at = ?, updated_at = ? WHERE id = ?`,
		input.State, append([]byte(nil), input.Observation...), ts, ts, effect.ID,
	); err != nil {
		return nil, fmt.Errorf("conclude publication effect: update: %w", err)
	}
	effect, err = getPublicationEffectTx(tx, input.PublicationID, input.Kind)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("conclude publication effect: commit: %w", err)
	}
	return effect, nil
}
