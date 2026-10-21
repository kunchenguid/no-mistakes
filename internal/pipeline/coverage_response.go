package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func CanContinueReviewCoverage(step types.StepName, payload string) bool {
	if validateCoverageResponse(step, payload, nil, nil, nil, nil) != nil {
		return false
	}
	findings, err := types.ParseFindingsJSON(payload)
	return err == nil && validCoverageRecordShape(findings)
}

func (e *Executor) validateCoverageBinding(bindings []*ipc.CoverageBinding) (*db.StepRound, error) {
	if len(bindings) != 1 || bindings[0] == nil {
		return nil, fmt.Errorf("coverage continuation requires the observed head, step and round")
	}
	b := bindings[0]
	if b.HeadSHA == "" || b.StepResultID == "" || b.RoundID == "" || b.StepResultID != e.waitingStepResultID || b.RoundID != e.waitingRoundID {
		return nil, fmt.Errorf("coverage continuation refers to a different or stale review gate")
	}
	step, err := e.db.GetStepResult(e.waitingStepResultID)
	if err != nil || step == nil {
		return nil, fmt.Errorf("cannot verify the parked review step for coverage continuation")
	}
	run, err := e.db.GetRun(step.RunID)
	if err != nil || run == nil || run.Status != types.RunRunning || run.HeadSHA != b.HeadSHA {
		return nil, fmt.Errorf("coverage continuation head does not match the current run head")
	}
	rounds, err := e.db.GetRoundsByStep(step.ID)
	if err != nil || len(rounds) == 0 || rounds[len(rounds)-1].ID != b.RoundID {
		return nil, fmt.Errorf("cannot verify the current stored coverage round")
	}
	round := rounds[len(rounds)-1]
	trusted := ""
	if e.config != nil {
		trusted = e.config.TrustedConfigSHA
	}
	if _, err := RetainedReviewCoverage(round, run, step.ID, trusted); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(e.runContext(context.Background()), 5*time.Second)
	defer cancel()
	head, err := git.HeadSHA(ctx, e.workDir)
	if err != nil || head != b.HeadSHA {
		return nil, fmt.Errorf("coverage continuation worktree head does not match the observed run head")
	}
	dirty, err := git.Run(ctx, e.workDir, "status", "--porcelain")
	if err != nil || strings.TrimSpace(dirty) != "" {
		return nil, fmt.Errorf("coverage continuation requires a clean worktree; existing work was preserved")
	}
	return round, nil
}

// RetainedReviewCoverage validates persisted evidence, never caller-supplied
// coverage. The concrete review step also compares the current trusted paths
// and resolved merge base before it asks an agent to complete the remainder.
func RetainedReviewCoverage(round *db.StepRound, run *db.Run, stepID, trusted string) (types.Findings, error) {
	refuse := func() (types.Findings, error) {
		return types.Findings{}, fmt.Errorf("coverage continuation requires a valid stored same-run/head/base/trusted-scope record")
	}
	if round == nil || run == nil || round.ID == "" || round.StepResultID != stepID || round.FindingsJSON == nil || round.ReviewedHeadSHA == nil || *round.ReviewedHeadSHA != run.HeadSHA {
		return refuse()
	}
	findings, err := types.ParseFindingsJSON(*round.FindingsJSON)
	if err != nil || !validCoverageRecordShape(findings) {
		return refuse()
	}
	scope := findings.ReviewCoverageScope
	if scope.RunID != run.ID || scope.StepResultID != stepID || scope.HeadSHA != run.HeadSHA || scope.RunBaseSHA != run.BaseSHA || scope.TrustedConfigSHA != trusted {
		return refuse()
	}
	return findings, nil
}

func validCoverageRecordShape(findings types.Findings) bool {
	scope := findings.ReviewCoverageScope
	if len(findings.Items) != 0 || scope == nil || scope.Version != 1 || scope.RunID == "" || scope.StepResultID == "" || scope.HeadSHA == "" || scope.RunBaseSHA == "" || scope.BaseSHA == "" || scope.TrustedConfigSHA == "" || len(scope.Paths) == 0 {
		return false
	}
	// Compare by cleaned path, as Review does, so a clean partial round that
	// recorded "./x" against a scope of "x" can still continue.
	paths := make(map[string]bool, len(scope.Paths))
	for _, name := range scope.Paths {
		normalized := normalizeCoveredPath(name)
		if normalized == "" || paths[normalized] {
			return false
		}
		paths[normalized] = true
	}
	reviewed := make(map[string]bool, len(findings.ReviewedPaths))
	for _, name := range findings.ReviewedPaths {
		normalized := normalizeCoveredPath(name)
		if normalized == "" || !paths[normalized] {
			return false
		}
		reviewed[normalized] = true
	}
	if len(reviewed) == len(paths) {
		return false
	}
	return true
}

func validateCoverageResponse(step types.StepName, payload string, findingIDs, ignoredIDs []string, instructions *FixInstructions, added []types.Finding) error {
	if step != types.StepReview {
		return fmt.Errorf("complete-coverage applies only to a parked Review")
	}
	if len(findingIDs)+len(ignoredIDs)+len(instructions.userNotes())+len(instructions.driverNotes())+len(added) != 0 {
		return fmt.Errorf("complete-coverage cannot select, ignore, add or change findings")
	}
	var gate struct {
		Findings json.RawMessage `json:"findings"`
	}
	if err := json.Unmarshal([]byte(payload), &gate); err != nil {
		return fmt.Errorf("read parked review before coverage continuation: %w", err)
	}
	if len(gate.Findings) == 0 || strings.TrimSpace(string(gate.Findings)) == "null" {
		return fmt.Errorf("coverage continuation requires an explicit empty findings array")
	}
	var findings []types.Finding
	if err := json.Unmarshal(gate.Findings, &findings); err != nil {
		return fmt.Errorf("read parked review findings before coverage continuation: %w", err)
	}
	if len(findings) != 0 {
		return fmt.Errorf("coverage continuation cannot resolve outstanding review findings")
	}
	return nil
}
