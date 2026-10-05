package citest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
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

// TestCIStep_DecisionCheckBesideABuildFailureKeepsTheBuildRepairable stops a
// declared decision check from hiding the ordinary failures that share its
// observation: the build failure stays on the gate as auto-fix work, and the
// decision check stays beside it as the person's question.
func TestCIStep_DecisionCheckBesideABuildFailureKeepsTheBuildRepairable(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)

	checks := `[{"name":"build","state":"FAILURE","bucket":"fail","completedAt":"2026-08-27T07:54:14Z"},` +
		`{"name":"Workflow pin","state":"FAILURE","bucket":"fail","completedAt":"2026-08-27T07:54:14Z"}]`
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
		t.Fatalf("expected an observation outcome, got error: %v", err)
	}
	if outcome == nil || !outcome.NeedsApproval || !outcome.AutoFixable {
		t.Fatalf("the build failure must stay auto-fixable beside the decision check, got %+v", outcome)
	}
	if len(ag.Calls) != 0 {
		t.Fatalf("the step ran the fix agent itself (%d invocations)", len(ag.Calls))
	}

	var findings types.Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	actions := map[string]string{}
	for _, item := range findings.Items {
		actions[item.Check] = item.Action
	}
	if len(findings.Items) != 2 || actions["build"] != types.ActionAutoFix || actions["Workflow pin"] != types.ActionAskUser {
		t.Fatalf("findings = %+v, want build as auto-fix and Workflow pin as ask-user", findings.Items)
	}
}

// TestCIStep_FixSelectingOnlyTheDecisionCheckKeepsTheBuildOnTheGate stops a
// fix response that selected only the decision check from hiding the build
// failure left unselected beside it: the gate it returns still shows the build,
// so approving it cannot pass a red build nobody saw.
func TestCIStep_FixSelectingOnlyTheDecisionCheckKeepsTheBuildOnTheGate(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)

	checks := `[{"name":"build","state":"FAILURE","bucket":"fail","completedAt":"2026-08-27T07:54:14Z"},` +
		`{"name":"Workflow pin","state":"FAILURE","bucket":"fail","completedAt":"2026-08-27T07:54:14Z"}]`
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

	marshal := func(item types.Finding) string {
		encoded, err := json.Marshal(types.Findings{Items: []types.Finding{item}})
		if err != nil {
			t.Fatalf("marshal findings: %v", err)
		}
		return string(encoded)
	}
	sctx.Fixing = true
	sctx.PreviousFindings = marshal(types.Finding{ID: "ci-2", Severity: types.FindingSeverityError, Action: types.ActionAskUser, Category: types.FindingCategoryCICheck, Check: "Workflow pin", Description: "CI check failing: Workflow pin"})
	sctx.DeferredFindings = marshal(types.Finding{ID: "ci-1", Severity: types.FindingSeverityError, Action: types.ActionAutoFix, Category: types.FindingCategoryCICheck, Check: "build", Description: "CI check failing: build"})

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
		t.Fatalf("a fix response selecting only the decision check must park, got %+v", outcome)
	}
	if len(ag.Calls) != 0 {
		t.Fatalf("the fix agent ran for a decision check (%d invocations)", len(ag.Calls))
	}
	var findings types.Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	checksOnGate := map[string]bool{}
	for _, item := range findings.Items {
		checksOnGate[item.Check] = true
	}
	if len(findings.Items) != 2 || !checksOnGate["build"] || !checksOnGate["Workflow pin"] {
		t.Fatalf("findings = %+v, want the decision check and the unselected build failure", findings.Items)
	}
}

// TestCIStep_FixRoundRepairsTheBuildButNeverShowsTheAgentTheDecisionCheck
// keeps decision checks outside the fix agent by excluding them from its
// targets, so a fix response that selects both still repairs the build.
func TestCIStep_FixRoundRepairsTheBuildButNeverShowsTheAgentTheDecisionCheck(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)

	checks := `[{"name":"build","state":"FAILURE","bucket":"fail","completedAt":"2026-08-27T07:54:14Z"},` +
		`{"name":"Workflow pin","state":"FAILURE","bucket":"fail","completedAt":"2026-08-27T07:54:14Z"}]`
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

	selected, err := json.Marshal(types.Findings{Items: []types.Finding{
		{Severity: types.FindingSeverityError, Action: types.ActionAutoFix, Category: types.FindingCategoryCICheck, Check: "build", Description: "CI check failing: build"},
		{Severity: types.FindingSeverityError, Action: types.ActionAskUser, Category: types.FindingCategoryCICheck, Check: "Workflow pin", Description: "CI check failing: Workflow pin"},
	}})
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
	_, _ = step.Execute(sctx)

	if len(ag.Calls) == 0 {
		t.Fatal("a decision check beside a build failure refused the whole fix round")
	}
	for _, call := range ag.Calls {
		if !strings.Contains(call.Prompt, "build") {
			t.Fatalf("the fix agent was not asked to repair the build: %s", call.Prompt)
		}
		if strings.Contains(call.Prompt, "Workflow pin") {
			t.Fatalf("the fix agent was shown the decision check: %s", call.Prompt)
		}
	}
}

// TestCIStep_AutoFixReversionParksAndIsNeverAuthorisedByAnotherRound drives the
// step end to end: an automatic round whose repair deletes a file the branch
// added must park at the reversion gate, and a later round that never selected
// that gate's refusal must park again rather than commit it as authorised.
func TestCIStep_AutoFixReversionParksAndIsNeverAuthorisedByAnotherRound(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)

	checks := `[{"name":"build","state":"FAILURE","bucket":"fail","completedAt":"2026-08-27T07:54:14Z"}]`
	env, _ := stepstest.FakeCIGHLoggedSequence(t, "OPEN", []string{checks, checks, checks, checks, checks, checks}, "", "")

	prURL := "https://github.com/test/repo/pull/2891"
	ag := &stepstest.MockAgent{AgentName: "test"}
	ag.RunFn = func(context.Context, agent.RunOpts) (*agent.Result, error) {
		_ = os.Remove(filepath.Join(dir, "feature.txt"))
		return &agent.Result{}, nil
	}
	sctx := stepstest.NewTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.CITimeout = time.Hour
	sctx.Config.AutoFix = config.AutoFix{CI: 3}

	build, err := json.Marshal(types.Findings{Items: []types.Finding{{
		Severity: types.FindingSeverityError, Action: types.ActionAutoFix, Category: types.FindingCategoryCICheck,
		Check: "build", Description: "CI check failing: build",
	}}})
	if err != nil {
		t.Fatalf("marshal findings: %v", err)
	}

	step := &steps.CIStep{}
	for round := 1; round <= 2; round++ {
		ctx, cancel := context.WithCancel(context.Background())
		sctx.Ctx = ctx
		polls := 0
		step.SetWaitForNextPoll(func(ctx context.Context, _ time.Duration) error {
			polls++
			if polls >= 4 {
				cancel()
				return ctx.Err()
			}
			return nil
		})
		sctx.Fixing = true
		sctx.PreviousFindings = string(build)
		outcome, err := step.Execute(sctx)
		cancel()
		if err != nil {
			t.Fatalf("round %d: expected the reversion gate, got error: %v", round, err)
		}
		if outcome == nil || !outcome.NeedsApproval || outcome.AutoFixable || !pipeline.HasDecisionReversionRefusal(outcome.Findings) {
			t.Fatalf("round %d: a repair undoing branch work must park at the reversion gate, got %+v", round, outcome)
		}
		if head := stepstest.GitCmd(t, dir, "rev-parse", "HEAD"); head != headSHA {
			t.Fatalf("round %d: the reversion was committed (head %s, want %s)", round, head, headSHA)
		}
	}
}
