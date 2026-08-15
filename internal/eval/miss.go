package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ErrReviewDidNotPassGreen is returned when ingest is asked to label a run
// whose review did not complete with a non-blocking (green) pass. A parked or
// blocking review is a different class: it found something, so it is not a
// post-PR miss.
var ErrReviewDidNotPassGreen = errors.New("review did not pass green")

// IngestResult reports which captured case received post-PR-miss gold.
type IngestResult struct {
	CaseID string
	Added  int
	Total  int
}

// ParsePostPRMissFinding accepts one finding object. ID and description are
// required; they are the eval matcher keys. This is the typed source of truth
// for a confirmed post-PR miss. no-mistakes does not read firstmate ledgers
// or scrape GitHub review comments.
func ParsePostPRMissFinding(raw string) (FindingGold, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return FindingGold{}, fmt.Errorf("finding JSON is empty")
	}
	var finding types.Finding
	if err := json.Unmarshal([]byte(raw), &finding); err != nil {
		return FindingGold{}, fmt.Errorf("parse finding JSON: %w", err)
	}
	id := strings.TrimSpace(finding.ID)
	description := strings.TrimSpace(finding.Description)
	if id == "" || description == "" {
		return FindingGold{}, fmt.Errorf("finding requires id and description")
	}
	severity := strings.TrimSpace(finding.Severity)
	if severity == "" {
		severity = "error"
	}
	return FindingGold{
		ID:          id,
		Kind:        GoldFalseNegative,
		Source:      goldSourcePostPRMiss,
		File:        strings.TrimSpace(finding.File),
		Line:        finding.Line,
		Description: description,
		Severity:    severity,
	}, nil
}

// IngestPostPRMiss captures a run that already passed review green, then
// writes confirmed post-PR misses as false-negative gold on the last green
// review pass. Capture of an existing case is a no-op, so later ingest still
// attaches gold. Duplicate finding IDs are no-ops.
func IngestPostPRMiss(ctx context.Context, store *Store, p *paths.Paths, database *db.DB, runID string, misses []FindingGold) (IngestResult, error) {
	if store == nil || p == nil || database == nil {
		return IngestResult{}, fmt.Errorf("eval miss ingest requires a store, paths, and database")
	}
	if len(misses) == 0 {
		return IngestResult{}, fmt.Errorf("eval miss ingest requires at least one finding")
	}
	for i, miss := range misses {
		if strings.TrimSpace(miss.ID) == "" || strings.TrimSpace(miss.Description) == "" {
			return IngestResult{}, fmt.Errorf("finding %d requires id and description", i+1)
		}
		if miss.Kind == "" {
			misses[i].Kind = GoldFalseNegative
		}
		if miss.Source == "" {
			misses[i].Source = goldSourcePostPRMiss
		}
		if misses[i].Kind != GoldFalseNegative {
			return IngestResult{}, fmt.Errorf("finding %q must be false-negative gold", miss.ID)
		}
	}

	cases, err := Capture(ctx, store, p, database, runID)
	if err != nil {
		return IngestResult{}, err
	}
	run, err := database.GetRun(strings.TrimSpace(runID))
	if err != nil {
		return IngestResult{}, fmt.Errorf("read source run: %w", err)
	}
	if run == nil {
		return IngestResult{}, fmt.Errorf("run %q not found", runID)
	}
	green, err := lastGreenReviewRound(database, run.ID)
	if err != nil {
		return IngestResult{}, err
	}
	var target *Case
	for i := range cases {
		if cases[i].SourceRoundID == green.ID {
			target = &cases[i]
			break
		}
	}
	if target == nil {
		return IngestResult{}, fmt.Errorf("%w: green review round %q was not captured", ErrNoCapturableReview, green.ID)
	}

	unlock, err := lockCorpus(ctx, store.root)
	if err != nil {
		return IngestResult{}, err
	}
	defer unlock()

	updated, added, err := store.appendFindingGold(*target, misses)
	if err != nil {
		return IngestResult{}, err
	}
	return IngestResult{CaseID: updated.ID, Added: added, Total: len(updated.Labels.Findings)}, nil
}

func lastGreenReviewRound(database *db.DB, runID string) (*db.StepRound, error) {
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		return nil, fmt.Errorf("read source steps: %w", err)
	}
	var reviewStep *db.StepResult
	for _, step := range steps {
		if step.StepName == types.StepReview {
			reviewStep = step
			break
		}
	}
	if reviewStep == nil {
		return nil, fmt.Errorf("%w: run %q has no review step", ErrNoCapturableReview, runID)
	}
	if reviewStep.Status != types.StepStatusCompleted {
		return nil, fmt.Errorf("%w: review step status is %s", ErrReviewDidNotPassGreen, reviewStep.Status)
	}
	rounds, err := database.GetRoundsByStep(reviewStep.ID)
	if err != nil {
		return nil, fmt.Errorf("read review rounds: %w", err)
	}
	var last *db.StepRound
	for _, round := range rounds {
		if round.FindingsJSON == nil || strings.TrimSpace(*round.FindingsJSON) == "" {
			continue
		}
		last = round
	}
	if last == nil {
		return nil, fmt.Errorf("%w: no completed review pass with findings", ErrReviewDidNotPassGreen)
	}
	if !reviewPassedGreen(*last.FindingsJSON) {
		return nil, fmt.Errorf("%w: last completed review pass was blocking", ErrReviewDidNotPassGreen)
	}
	return last, nil
}

func reviewPassedGreen(findingsJSON string) bool {
	findings, err := types.ParseFindingsJSON(findingsJSON)
	if err != nil {
		return false
	}
	for _, item := range findings.Items {
		switch strings.ToLower(strings.TrimSpace(item.Severity)) {
		case "error", "warning":
			return false
		}
	}
	return true
}
