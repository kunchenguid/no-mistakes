package eval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// goldSourceCIFalseNegative marks false-negative gold auto-ingested from a CI
// finding that a green Review missed and the pipeline then fixed in-run. Any
// real code defect CI surfaces, that is confirmed and fixed, is by definition a
// Review false negative: Review passed green and missed it.
const goldSourceCIFalseNegative = "recorded-ci-false-negative"

// isCIFalseNegativeCategory reports whether a CI finding category names a real
// code defect Review could have caught: a failing check the provider attributes
// to the job (ci-check) or a review bot's comment about the change
// (ci-review-bot). A ci-transient failure is a provider/infra outcome no code
// change clears, and a merge conflict is not a defect Review reads for, so both
// are excluded.
func isCIFalseNegativeCategory(category string) bool {
	switch category {
	case types.FindingCategoryCICheck, types.FindingCategoryCIReviewBot:
		return true
	default:
		return false
	}
}

// CIFalseNegativesFromRun reads a finished run's persisted CI findings and
// returns false-negative gold for every ci-check / ci-review-bot finding the
// run surfaced, confirmed, and fixed.
//
// The CI step already persists its structured findings on each round
// (FindingsJSON) together with the IDs it selected for repair
// (SelectedFindingIDs); this reads them back. A finding counts as confirmed and
// fixed when it was selected for a fix round AND the CI step completed genuinely
// green - the run reached RunCompleted and the CI step is completed with no
// approval override (a passed-with-override CI step is a human accepting an
// unresolved condition, not a fix). Findings that were never selected
// (dismissed at the gate), ci-transient / provider-infra failures, and merge
// conflicts are excluded.
//
// It never fabricates: a run that did not finish, whose CI step did not
// complete cleanly green, or that has no such fixed finding yields nothing. It
// makes no head/commit provenance, same-head, or cross-run judgement - a real
// defect that slipped a green Review is a valid case regardless of which commit
// introduced it.
func CIFalseNegativesFromRun(database *db.DB, runID string) ([]FindingGold, error) {
	if database == nil {
		return nil, fmt.Errorf("ci false-negative ingest requires a database")
	}
	runID = strings.TrimSpace(runID)
	run, err := database.GetRun(runID)
	if err != nil {
		return nil, fmt.Errorf("read source run: %w", err)
	}
	if run == nil || run.Status != types.RunCompleted {
		return nil, nil
	}
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		return nil, fmt.Errorf("read source steps: %w", err)
	}
	var ciStep *db.StepResult
	for _, step := range steps {
		if step.StepName == types.StepCI {
			ciStep = step
			break
		}
	}
	if ciStep == nil || ciStep.Status != types.StepStatusCompleted {
		return nil, nil
	}
	if ciStep.OverrideReason != nil && strings.TrimSpace(*ciStep.OverrideReason) != "" {
		return nil, nil
	}
	rounds, err := database.GetRoundsByStep(ciStep.ID)
	if err != nil {
		return nil, fmt.Errorf("read CI rounds: %w", err)
	}
	var gold []FindingGold
	seen := map[string]bool{}
	for _, round := range rounds {
		if round.FindingsJSON == nil || round.SelectedFindingIDs == nil {
			continue
		}
		selected := parseSelectedFindingIDs(*round.SelectedFindingIDs)
		if len(selected) == 0 {
			continue
		}
		findings, err := types.ParseFindingsJSON(*round.FindingsJSON)
		if err != nil {
			continue
		}
		for _, finding := range findings.Items {
			if !isCIFalseNegativeCategory(finding.Category) {
				continue
			}
			id := strings.TrimSpace(finding.ID)
			if id == "" || !selected[id] {
				continue
			}
			g := ciFindingGold(finding)
			if seen[g.ID] {
				continue
			}
			seen[g.ID] = true
			gold = append(gold, g)
		}
	}
	return gold, nil
}

// AutoIngestCIFalseNegatives writes false-negative gold for a finished run's
// fixed CI findings onto its green review case. It is the CI-side counterpart
// of AutoCapture: the caller owns the timeout and the decision to run. It opens
// its own store, does its work, and closes it, so a failure here cannot reach
// the run that triggered it.
//
// Skipped is true, with no error, when the run has no fixed ci-check /
// ci-review-bot finding, or when its review did not pass green (there is no
// green review case to attach the misses to) - both are ordinary outcomes.
func AutoIngestCIFalseNegatives(ctx context.Context, p *paths.Paths, database *db.DB, runID string) (IngestResult, bool, error) {
	if p == nil || database == nil {
		return IngestResult{}, false, fmt.Errorf("eval ci false-negative ingest requires paths and a database")
	}
	misses, err := CIFalseNegativesFromRun(database, runID)
	if err != nil {
		return IngestResult{}, false, err
	}
	if len(misses) == 0 {
		return IngestResult{}, true, nil
	}
	store, err := Open(p.EvalDir())
	if err != nil {
		return IngestResult{}, false, err
	}
	defer store.Close()

	result, err := IngestPostPRMiss(ctx, store, p, database, runID, misses)
	if err != nil {
		// A run whose review did not pass green, or has no capturable review,
		// has nowhere to attach these misses: skip it rather than fault.
		if errors.Is(err, ErrReviewDidNotPassGreen) || errors.Is(err, ErrNoCapturableReview) {
			return IngestResult{}, true, nil
		}
		return IngestResult{}, false, err
	}
	return result, false, nil
}

// ciFindingGold converts one persisted CI finding into false-negative gold. It
// carries only what the structured finding actually gives - file, line,
// description - and never enriches from log text, so it cannot fabricate a
// location the finding did not record.
func ciFindingGold(finding types.Finding) FindingGold {
	severity := types.NormalizeFindingSeverity(finding.Severity)
	if !types.IsKnownFindingSeverity(severity) {
		severity = types.FindingSeverityError
	}
	return FindingGold{
		ID:          ciFalseNegativeID(finding),
		Kind:        GoldFalseNegative,
		Source:      goldSourceCIFalseNegative,
		File:        strings.TrimSpace(finding.File),
		Line:        finding.Line,
		Description: strings.TrimSpace(finding.Description),
		Severity:    severity,
	}
}

// ciFalseNegativeID derives a deterministic gold ID from the finding's semantic
// identity so re-ingesting the same run is a no-op (the corpus dedupes gold by
// ID) and two identical findings across rounds collapse to one case.
func ciFalseNegativeID(finding types.Finding) string {
	h := sha256.Sum256([]byte(strings.Join([]string{
		finding.Category,
		strings.TrimSpace(finding.CheckID),
		strings.TrimSpace(finding.Check),
		strings.TrimSpace(finding.File),
		strconv.Itoa(finding.Line),
		strings.TrimSpace(finding.Description),
	}, "\x00")))
	return "ci-fn-" + hex.EncodeToString(h[:6])
}

func parseSelectedFindingIDs(raw string) map[string]bool {
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil
	}
	selected := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			selected[id] = true
		}
	}
	return selected
}
