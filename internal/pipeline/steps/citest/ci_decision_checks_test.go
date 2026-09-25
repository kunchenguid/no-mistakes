package citest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestCIStep_DecisionCheckParksWithoutEverRunningTheFixAgent is the structural
// half of the decision guard: a check the maintainer has declared to mean "a
// person must act" is never handed to the fix agent, so the agent never gets
// the chance to reason its way into performing the person's decision itself.
func TestCIStep_DecisionCheckParksWithoutEverRunningTheFixAgent(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)

	checks := `[{"name":"build","state":"SUCCESS","bucket":"pass"},` +
		`{"name":"Workflow pin / verify (pull_request)","state":"FAILURE","bucket":"fail","completedAt":"2026-08-27T07:54:14Z"}]`
	env, _ := stepstest.FakeCIGHLoggedSequence(t, "OPEN", []string{checks, checks, checks}, "", "")

	prURL := "https://github.com/test/repo/pull/2891"
	ag := &stepstest.MockAgent{AgentName: "test"}
	sctx := stepstest.NewTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.CITimeout = time.Hour
	sctx.Config.AutoFix = config.AutoFix{CI: 3}
	sctx.Config.CI = config.CI{DecisionChecks: []string{"workflow pin*"}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	polls := 0
	step := (&steps.CIStep{}).SetWaitForNextPoll(func(ctx context.Context, _ time.Duration) error {
		polls++
		if polls >= 4 {
			cancel()
			return ctx.Err()
		}
		return nil
	})
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected an approval outcome, got error: %v", err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("a declared decision check must park for a person, got %+v", outcome)
	}
	if len(ag.Calls) != 0 {
		t.Fatalf("the fix agent ran for a decision check (%d invocations)", len(ag.Calls))
	}

	var findings types.Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("findings = %+v, want exactly the decision check", findings.Items)
	}
	if !strings.Contains(findings.Items[0].Description, "Workflow pin / verify (pull_request)") {
		t.Fatalf("finding does not name the check: %s", findings.Items[0].Description)
	}
	if findings.Items[0].Action != types.ActionAskUser {
		t.Fatalf("finding action = %q, want ask-user", findings.Items[0].Action)
	}
}

// TestCIStep_DecisionCheckStaysUnfixableUnderAnExplicitFixRequest proves the
// declaration is a standing boundary rather than a default: unlike the
// reversion guard on one concrete repair, a gate answer cannot dissolve it.
// Otherwise the incident reopens the moment anyone answers "fix".
func TestCIStep_DecisionCheckStaysUnfixableUnderAnExplicitFixRequest(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)

	checks := `[{"name":"Workflow pin","state":"FAILURE","bucket":"fail","completedAt":"2026-08-27T07:54:14Z"}]`
	env, _ := stepstest.FakeCIGHLoggedSequence(t, "OPEN", []string{checks, checks, checks}, "", "")

	prURL := "https://github.com/test/repo/pull/2891"
	ag := &stepstest.MockAgent{AgentName: "test"}
	sctx := stepstest.NewTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.CITimeout = time.Hour
	sctx.Config.AutoFix = config.AutoFix{CI: 3}
	sctx.Config.CI = config.CI{DecisionChecks: []string{"Workflow pin"}}

	// The person answered the gate with "fix", selecting the decision check
	// itself. That is exactly the answer the declaration must survive.
	selected, err := json.Marshal(types.Findings{Items: []types.Finding{{
		Severity:    types.FindingSeverityError,
		Action:      types.ActionAutoFix,
		Category:    types.FindingCategoryCICheck,
		Check:       "Workflow pin",
		Description: "CI check failing: Workflow pin",
	}}})
	if err != nil {
		t.Fatalf("marshal selected findings: %v", err)
	}
	sctx.Fixing = true
	sctx.PreviousFindings = string(selected)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	polls := 0
	step := (&steps.CIStep{}).SetWaitForNextPoll(func(ctx context.Context, _ time.Duration) error {
		polls++
		if polls >= 4 {
			cancel()
			return ctx.Err()
		}
		return nil
	})
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected an approval outcome, got error: %v", err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("an explicit fix request must not make a decision check repairable, got %+v", outcome)
	}
	if len(ag.Calls) != 0 {
		t.Fatalf("the fix agent ran for a decision check under an explicit fix request (%d invocations)", len(ag.Calls))
	}
}
