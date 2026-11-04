package pipeline

import (
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// This file owns the one answer every surface gives to "what did this run NOT
// validate live, and why": the axi run view's validation block and the PR
// body's Not validated live callout both read ValidationGaps, so a driving
// agent and a reviewer see the same list with the same reasons.

// Validation exception kinds.
const (
	ValidationKindTestException = "test-exception"
	ValidationKindCIOverride    = "ci-override"
	ValidationKindOperatorSkip  = "operator-skip"
	ValidationKindAutomaticSkip = "automatic-skip"
)

// NoReasonRecorded stands in for an explicit skip whose operator gave no
// reason, so the gap is still listed rather than read as a clean pass.
const NoReasonRecorded = "no reason recorded"

// UntestedScenario is one Test scenario that was not driven live.
type UntestedScenario struct {
	Scenario string
	Reason   string
}

// ValidationException is one human or automatic decision that let the run
// continue without the validation it would otherwise have required.
type ValidationException struct {
	Step   string
	Kind   string
	Reason string
}

// ValidationGaps summarizes a run's live validation and everything it left
// unvalidated.
type ValidationGaps struct {
	Verdict    string
	Live       int
	Total      int
	Untested   []UntestedScenario
	Exceptions []ValidationException
}

// ValidationStep is the per-step input ValidationGaps needs, readable from
// both a database row and the daemon's IPC snapshot.
type ValidationStep struct {
	Name       string
	Status     string
	SkipReason string
}

// ValidationInput is everything CollectValidationGaps reads.
type ValidationInput struct {
	Scenarios []types.TestScenario
	Verdict   string
	// TestException and CIOverride are the qualified reasons the existing
	// override contract records (db.StepResult.TestOverrideReason and the CI
	// step's override reason); empty means none.
	TestException string
	CIOverride    string
	Steps         []ValidationStep
}

// CollectValidationGaps derives the run's validation gaps. It never decides
// anything: untested scenarios still never park, and every exception listed
// here was already recorded by the step or gate that allowed it.
func CollectValidationGaps(in ValidationInput) ValidationGaps {
	gaps := ValidationGaps{}
	if types.IsKnownTestVerdict(in.Verdict) {
		gaps.Verdict = in.Verdict
	}
	gaps.Live, gaps.Total = types.LiveScenarioCounts(in.Scenarios)
	for _, s := range in.Scenarios {
		if s.Live {
			continue
		}
		reason := strings.TrimSpace(s.Reason)
		if reason == "" {
			reason = strings.TrimSpace(s.Evidence)
		}
		if reason == "" {
			reason = NoReasonRecorded
		}
		gaps.Untested = append(gaps.Untested, UntestedScenario{Scenario: strings.TrimSpace(s.Name), Reason: reason})
	}
	if reason := strings.TrimSpace(in.TestException); reason != "" {
		gaps.Exceptions = append(gaps.Exceptions, ValidationException{Step: string(types.StepTest), Kind: ValidationKindTestException, Reason: reason})
	}
	if reason := strings.TrimSpace(in.CIOverride); reason != "" {
		gaps.Exceptions = append(gaps.Exceptions, ValidationException{Step: string(types.StepCI), Kind: ValidationKindCIOverride, Reason: reason})
	}
	for _, s := range in.Steps {
		if s.Status != string(types.StepStatusSkipped) {
			continue
		}
		switch {
		case types.IsOperatorSkipReason(s.SkipReason):
			gaps.Exceptions = append(gaps.Exceptions, ValidationException{Step: s.Name, Kind: ValidationKindOperatorSkip, Reason: operatorSkipDetail(s.SkipReason)})
		case IsAutomaticSkip(s.Name, s.SkipReason):
			gaps.Exceptions = append(gaps.Exceptions, ValidationException{Step: s.Name, Kind: ValidationKindAutomaticSkip, Reason: s.SkipReason})
		}
	}
	return gaps
}

// IsAutomaticSkip reports whether a skipped step recorded an automatic
// publication or CI-verification skip (the passed-with-skips cause).
func IsAutomaticSkip(step, skipReason string) bool {
	if skipReason == "" || types.IsOperatorSkipReason(skipReason) {
		return false
	}
	return step == string(types.StepPR) || step == string(types.StepCI)
}

func operatorSkipDetail(skipReason string) string {
	detail := strings.TrimSpace(strings.TrimPrefix(skipReason, types.OperatorSkipReason))
	detail = strings.TrimSpace(strings.TrimPrefix(detail, ":"))
	if detail == "" {
		return NoReasonRecorded
	}
	return detail
}

// HasGaps reports whether anything was left unvalidated live.
func (g ValidationGaps) HasGaps() bool {
	return len(g.Untested) > 0 || len(g.Exceptions) > 0
}

// Empty reports whether there is nothing at all to report: no verdict, no
// scenarios, and no exceptions.
func (g ValidationGaps) Empty() bool {
	return g.Verdict == "" && g.Total == 0 && len(g.Exceptions) == 0
}
