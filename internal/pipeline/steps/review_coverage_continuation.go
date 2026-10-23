package steps

import (
	"encoding/json"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func currentReviewCoverageScope(sctx *pipeline.StepContext, base, head string, paths []string) *types.ReviewCoverageScope {
	scope := &types.ReviewCoverageScope{Version: 1, RunID: sctx.Run.ID, StepResultID: sctx.StepResultID, HeadSHA: head, RunBaseSHA: sctx.Run.BaseSHA, BaseSHA: base, TrustedConfigSHA: sctx.Config.TrustedConfigSHA, Paths: append([]string(nil), paths...)}
	if sctx.CoverageContinuation != nil {
		scope.SourceRoundID = sctx.CoverageContinuation.ID
	}
	return scope
}

func retainedCoverageForReview(sctx *pipeline.StepContext, base, head string, paths []string) (Findings, error) {
	source := sctx.CoverageContinuation
	if source == nil || sctx.DB == nil {
		return Findings{}, fmt.Errorf("stored coverage source is missing")
	}
	rounds, err := sctx.DB.GetRoundsByStep(sctx.StepResultID)
	if err != nil || len(rounds) == 0 {
		return Findings{}, fmt.Errorf("stored coverage source is unreadable")
	}
	stored := rounds[len(rounds)-1]
	if stored.ID != source.ID || stored.FindingsJSON == nil || source.FindingsJSON == nil || *stored.FindingsJSON != *source.FindingsJSON {
		return Findings{}, fmt.Errorf("stored coverage source changed after the gate response")
	}
	findings, err := pipeline.RetainedReviewCoverage(stored, sctx.Run, sctx.StepResultID, sctx.Config.TrustedConfigSHA)
	if err != nil {
		return Findings{}, err
	}
	scope := findings.ReviewCoverageScope
	actual, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil || actual != head || scope.HeadSHA != head || scope.BaseSHA != base || len(scope.Paths) != len(paths) || hasInvalidReviewedPath(findings.ReviewedPaths) {
		return Findings{}, fmt.Errorf("retained coverage does not match the current head/base/trusted paths")
	}
	dirty, err := git.Run(sctx.Ctx, sctx.WorkDir, "status", "--porcelain")
	if err != nil || dirty != "" {
		return Findings{}, fmt.Errorf("coverage continuation changed or cannot verify its worktree; existing work was preserved")
	}
	expected := make(map[string]bool, len(scope.Paths))
	for _, name := range scope.Paths {
		expected[name] = true
	}
	for _, name := range paths {
		if !expected[name] {
			return Findings{}, fmt.Errorf("retained coverage does not match the current trusted paths")
		}
	}
	sctx.Log(fmt.Sprintf("validated retained coverage from stored round %s at head %s, base %s, trusted config %s", stored.ID, head, base, scope.TrustedConfigSHA))
	return findings, nil
}

func refusedCoverageContinuation(sctx *pipeline.StepContext, err error) (*pipeline.StepOutcome, error) {
	sctx.Log("coverage continuation refused: " + err.Error())
	findings := Findings{Items: []Finding{}, Summary: "Retained coverage cannot certify the current review scope", RiskLevel: "medium", RiskRationale: err.Error(), RiskScope: types.FindingsRiskScopeSourceOrExternal}
	encoded, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{NeedsApproval: true, Findings: string(encoded)}, nil
}
