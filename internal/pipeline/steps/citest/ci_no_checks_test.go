package citest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/cimonitor"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The base branch requires nothing, as GitHub reports it for an unprotected
// branch, and no ruleset applies.
const (
	unprotectedBranch = `{"name":"main","protected":false,"protection":{"enabled":false,"required_status_checks":{"enforcement_level":"off","contexts":[],"checks":[]}}}`
	noRulesets        = `[[]]`
)

// commitWorkflow commits a workflow file on the feature branch and returns the
// new head, so the probe reads it from the delivered commit's tree.
func commitWorkflow(t *testing.T, dir, name, content string) string {
	t.Helper()
	workflows := filepath.Join(dir, ".github", "workflows")
	if err := os.MkdirAll(workflows, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workflows, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	stepstest.GitCmd(t, dir, "add", "-A")
	stepstest.GitCmd(t, dir, "commit", "-m", "add workflow "+name)
	return stepstest.GitCmd(t, dir, "rev-parse", "HEAD")
}

type noChecksRun struct {
	outcome *pipeline.StepOutcome
	err     error
	logs    []string
	elapsed time.Duration
	sctx    *pipeline.StepContext
}

// runNoChecksMonitor drives the CI step on a 30s poll with a fake clock until
// it returns, stop reports true, or limit elapses (then it is cancelled).
func runNoChecksMonitor(t *testing.T, dir, baseSHA, headSHA string, env []string, limit time.Duration, stop func(logs []string) bool) noChecksRun {
	t.Helper()
	prURL := "https://github.com/test/repo/pull/1"
	sctx := stepstest.NewTestContextWithDBRecords(t, &stepstest.MockAgent{AgentName: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 168 * time.Hour
	sctx.Config.NoCI = false

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	started := time.Date(2026, time.October, 26, 23, 0, 0, 0, time.UTC)
	current := started
	step := (&steps.CIStep{}).
		SetPollIntervalOverride(30 * time.Second).
		SetNow(func() time.Time { return current }).
		SetBaseBranchTip(func(context.Context) (string, bool) { return baseSHA, true }).
		SetWaitForNextPoll(func(ctx context.Context, interval time.Duration) error {
			current = current.Add(interval)
			if current.Sub(started) > limit || (stop != nil && stop(logs)) {
				cancel()
				return ctx.Err()
			}
			return nil
		})
	outcome, err := step.Execute(sctx)
	return noChecksRun{outcome: outcome, err: err, logs: logs, elapsed: current.Sub(started), sctx: sctx}
}

func assertParkedAsNoChecksConfigured(t *testing.T, run noChecksRun) {
	t.Helper()
	if run.err != nil {
		t.Fatalf("expected a park, got error %v; logs=%v", run.err, run.logs)
	}
	if run.outcome == nil || !run.outcome.NeedsApproval {
		t.Fatalf("expected the step to park for a decision, got %+v; logs=%v", run.outcome, run.logs)
	}
	findings, err := types.ParseFindingsJSON(run.outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 1 || findings.Items[0].ID != "ci-no-checks-configured" || findings.Items[0].Action != types.ActionAskUser {
		t.Fatalf("expected one ask-user ci-no-checks-configured finding, got %+v", findings.Items)
	}
	if !strings.Contains(findings.Items[0].Description, "no_ci: true") {
		t.Fatalf("finding must point at the no_ci declaration, got %q", findings.Items[0].Description)
	}
	// Parked, not passed: nothing may report the PR ready.
	if cimonitor.ChecksPassed(run.logs) {
		t.Fatalf("a no-CI park must never read as checks passed; logs=%v", run.logs)
	}
	dbRun, err := run.sctx.DB.GetRun(run.sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbRun.CIReadyAt != nil {
		t.Fatalf("a no-CI park must not persist readiness, got %v", *dbRun.CIReadyAt)
	}
}

func assertStillWaiting(t *testing.T, run noChecksRun, past time.Duration) {
	t.Helper()
	if !errors.Is(run.err, context.Canceled) {
		t.Fatalf("expected the monitor to keep waiting until cancelled, got outcome %+v err %v; logs=%v", run.outcome, run.err, run.logs)
	}
	if run.elapsed <= past {
		t.Fatalf("test did not run past %v (elapsed %v)", past, run.elapsed)
	}
}

// TestCIStep_NoCIConfiguredParksPromptlyAsNotApplicable is the qith shape: a
// repository with no workflows, no third-party CI, and no required checks on
// its base. Instead of polling to the 168h ci_timeout, the step parks for a
// decision shortly after the grace period, never reporting the PR green.
func TestCIStep_NoCIConfiguredParksPromptlyAsNotApplicable(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
	env := append(stepstest.FakeCIGH(t, "OPEN", "[]"),
		"FAKE_CLI_BRANCH_PROTECTION="+unprotectedBranch,
		"FAKE_CLI_BRANCH_RULES="+noRulesets,
	)

	run := runNoChecksMonitor(t, dir, baseSHA, headSHA, env, time.Hour, nil)

	assertParkedAsNoChecksConfigured(t, run)
	if run.elapsed < 5*time.Minute || run.elapsed > 6*time.Minute {
		t.Fatalf("expected the park right after the 5m grace period, got %v", run.elapsed)
	}
}

// TestCIStep_IssueOnlyWorkflowStillParks is the #990 reproduction: the only
// workflow fires on newly opened issues, which GitHub never attaches to a PR
// head, and the private repository's plan has no rulesets at all.
func TestCIStep_IssueOnlyWorkflowStillParks(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := stepstest.SetupGitRepo(t)
	headSHA := commitWorkflow(t, dir, "triage.yml", "name: triage\non:\n  issues:\n    types: [opened]\njobs:\n  label:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo triage\n")
	env := append(stepstest.FakeCIGH(t, "OPEN", "[]"),
		"FAKE_CLI_BRANCH_PROTECTION="+unprotectedBranch,
		"FAKE_CLI_BRANCH_RULES_ERR=gh: Upgrade to GitHub Pro or make this repository public to enable this feature. (HTTP 403)",
	)

	run := runNoChecksMonitor(t, dir, baseSHA, headSHA, env, time.Hour, nil)

	assertParkedAsNoChecksConfigured(t, run)
}

// TestCIStep_PRWorkflowSlowToRegisterKeepsWaiting proves the probe never cuts
// short a repository that has CI: its pull_request workflow takes longer than
// the grace period to register, and the monitor keeps waiting until the
// checks appear and pass.
func TestCIStep_PRWorkflowSlowToRegisterKeepsWaiting(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := stepstest.SetupGitRepo(t)
	headSHA := commitWorkflow(t, dir, "ci.yml", "name: CI\non:\n  pull_request:\n  push:\n    branches: [main]\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: go test ./...\n")
	sequence := make([]string, 0, 24)
	for i := 0; i < 20; i++ { // ten minutes of nothing registered
		sequence = append(sequence, `[]`)
	}
	sequence = append(sequence,
		`[{"name":"test","state":"PENDING","bucket":"pending"}]`,
		`[{"name":"test","state":"SUCCESS","bucket":"pass"}]`,
	)
	env := append(stepstest.FakeCIGHSequence(t, "OPEN", sequence),
		"FAKE_CLI_BRANCH_PROTECTION="+unprotectedBranch,
		"FAKE_CLI_BRANCH_RULES="+noRulesets,
	)

	run := runNoChecksMonitor(t, dir, baseSHA, headSHA, env, time.Hour, cimonitor.ChecksPassed)

	assertStillWaiting(t, run, 10*time.Minute)
	if !cimonitor.ChecksPassed(run.logs) {
		t.Fatalf("expected the late checks to be honored once green; logs=%v", run.logs)
	}
	if !logContains(run.logs, "CI is configured (workflow .github/workflows/ci.yml") {
		t.Fatalf("expected the probe to name the pull_request workflow; logs=%v", run.logs)
	}
}

// TestCIStep_RequiredCheckWithoutWorkflowKeepsWaiting covers CI the tree does
// not show: the base branch requires a check an external service reports, so
// an empty list is still a wait, however long.
func TestCIStep_RequiredCheckWithoutWorkflowKeepsWaiting(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
	env := append(stepstest.FakeCIGH(t, "OPEN", "[]"),
		"FAKE_CLI_BRANCH_PROTECTION="+unprotectedBranch,
		`FAKE_CLI_BRANCH_RULES=[[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"buildkite/pipeline"}]}}]]`,
	)

	run := runNoChecksMonitor(t, dir, baseSHA, headSHA, env, 30*time.Minute, nil)

	assertStillWaiting(t, run, 30*time.Minute)
	if !logContains(run.logs, "main requires checks before merge: buildkite/pipeline") {
		t.Fatalf("expected the required check to be named; logs=%v", run.logs)
	}
}

// TestCIStep_UnreadableBranchRequirementsKeepsWaiting proves a failed forge
// read is never mistaken for "no CI": the monitor keeps waiting and says why.
func TestCIStep_UnreadableBranchRequirementsKeepsWaiting(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
	env := append(stepstest.FakeCIGH(t, "OPEN", "[]"),
		"FAKE_CLI_BRANCH_PROTECTION="+unprotectedBranch,
		"FAKE_CLI_BRANCH_RULES_ERR=gh: API rate limit exceeded (HTTP 403)",
	)

	run := runNoChecksMonitor(t, dir, baseSHA, headSHA, env, 30*time.Minute, nil)

	assertStillWaiting(t, run, 30*time.Minute)
	if !logContains(run.logs, "warning: could not check whether CI is configured") {
		t.Fatalf("expected the failed probe to be logged; logs=%v", run.logs)
	}
}

func logContains(logs []string, want string) bool {
	for _, l := range logs {
		if strings.Contains(l, want) {
			return true
		}
	}
	return false
}
