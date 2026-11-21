package citest

import (
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"github.com/kunchenguid/no-mistakes/internal/scm/plugin/fakeplugin"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// checkoutDeath is the shape of a real Buildkite job log for a build whose agent
// could not clone the repository: the failure is at the head, and no repository
// command ran after it.
const checkoutDeath = "\x1b_bk;t=1\x1b\\$ cd /var/lib/buildkite-agent/builds/agent-1/keenvc/app\n" +
	"\x1b_bk;t=2\x1b\\Cloning into '.'...\n" +
	"\x1b_bk;t=3\x1b\\fatal: Could not read from remote repository.\n" +
	"\x1b_bk;t=4\x1b\\git@github.com: Permission denied (publickey).\n" +
	"\x1b_bk;t=5\x1b\\🚨 Error: cloning git repository: exit status 128\n" +
	"⚠️ Warning: Checkout failed!\n"

// providerBuild is a check the CI service published about itself: no rerun
// affordance, and a build-level page.
func providerBuild(name, log, acquisition string) fakeplugin.Check {
	return fakeplugin.Check{
		Name:                  name,
		Bucket:                "fail",
		State:                 "FAILURE",
		Link:                  "https://buildkite.com/keenvc/app/builds/3716",
		ProviderAttributed:    true,
		Log:                   log,
		RepositoryAcquisition: acquisition,
	}
}

// criterion 1: a build that died at checkout is infrastructure. The gate names
// the acquisition failure instead of a build or test verdict.
func TestCIStep_CheckoutDeathParksAsAcquisitionFailure(t *testing.T) {
	t.Parallel()
	// The plugin states the verdict itself and the log carries no acquisition
	// text at all, so this proves the CI step acts on the provider's structural
	// reading rather than on anything it guessed from the text.
	ag, sctx := checkoutDeathContext(t, []fakeplugin.Check{
		providerBuild("buildkite/keenvc/app", "no output captured for this build\n", "infrastructure"),
	}, 3)
	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := (&steps.CIStep{}).SetWaitForNextPoll(failOnExtraPoll)
	pinCIMonitorClock(step)
	outcome, err := stepstest.ExecuteWithAutoFix(t, step, sctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("checkout death outcome = %+v, want an approval park", outcome)
	}
	if len(ag.Calls) != 0 {
		t.Fatalf("a checkout death spent %d fix rounds, want 0: %v", len(ag.Calls), ag.Calls)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatalf("parse outcome findings: %v", err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("outcome findings = %+v, want one acquisition failure", findings.Items)
	}
	item := findings.Items[0]
	if item.Category != types.FindingCategoryCIInfrastructure {
		t.Fatalf("finding category = %q, want %q", item.Category, types.FindingCategoryCIInfrastructure)
	}
	if item.Action != types.ActionAskUser {
		t.Fatalf("finding action = %q, want %q", item.Action, types.ActionAskUser)
	}
	for _, want := range []string{"buildkite/keenvc/app", "failed to acquire the repository"} {
		if !strings.Contains(findings.Summary+" "+item.Description, want) {
			t.Fatalf("gate text %q/%q does not name %q", findings.Summary, item.Description, want)
		}
	}
	if strings.Contains(strings.Join(logs, "\n"), "auto-fixing") {
		t.Fatalf("a checkout death escalated to the fix agent; logs: %v", logs)
	}
}

// criterion 2: it consumes no round even when the round budget is exhausted, so
// a repo whose fix rounds are gone still parks on the acquisition failure rather
// than reporting failing tests.
func TestCIStep_CheckoutDeathConsumesNoRoundAtTheLastAllowedRound(t *testing.T) {
	t.Parallel()
	ag, sctx := checkoutDeathContext(t, []fakeplugin.Check{providerBuild("buildkite/keenvc/app", checkoutDeath, "")}, 1)
	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := (&steps.CIStep{}).SetWaitForNextPoll(failOnExtraPoll)
	pinCIMonitorClock(step)
	outcome, err := stepstest.ExecuteWithAutoFix(t, step, sctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("outcome = %+v, want an approval park", outcome)
	}
	if len(ag.Calls) != 0 {
		t.Fatalf("a checkout death at the last allowed round spent %d fix rounds, want 0", len(ag.Calls))
	}
	joined := strings.Join(logs, "\n")
	if strings.Contains(joined, "CI check failing") || strings.Contains(joined, "failing tests") {
		t.Fatalf("a checkout death was reported as a failing test; logs:\n%s", joined)
	}
	if !strings.Contains(joined, "CI infrastructure failure") {
		t.Fatalf("expected the acquisition failure to be logged, got:\n%s", joined)
	}
}

// criterion 3: a genuine failure in the same observation still consumes exactly
// one round, and it is the only thing acted on.
func TestCIStep_GenuineFailureBesideACheckoutDeathActsOnlyOnTheGenuineOne(t *testing.T) {
	t.Parallel()
	genuine := fakeplugin.Check{
		Name: "unit tests", Bucket: "fail", State: "FAILURE",
		Link: "https://git.example.com/team/repo/checks/9",
	}
	ag, sctx := checkoutDeathContext(t, []fakeplugin.Check{
		providerBuild("buildkite/keenvc/app", checkoutDeath, ""),
		genuine,
	}, 1)
	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := (&steps.CIStep{}).SetWaitForNextPoll(failOnExtraPoll)
	pinCIMonitorClock(step)
	outcome, err := stepstest.ExecuteWithAutoFix(t, step, sctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ag.Calls) != 1 {
		t.Fatalf("expected exactly one fix round for the genuine failure, got %d", len(ag.Calls))
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("outcome = %+v, want the ordinary CI gate once the round was spent", outcome)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatalf("parse outcome findings: %v", err)
	}
	targets := make([]string, 0, len(findings.Items))
	for _, item := range findings.Items {
		targets = append(targets, item.Check)
	}
	joined := strings.Join(targets, ",")
	if !strings.Contains(joined, "unit tests") {
		t.Fatalf("the genuine failure is not in the gate: %v", targets)
	}
	for _, item := range findings.Items {
		if strings.Contains(item.Check, "buildkite/keenvc/app") {
			t.Fatalf("the checkout death reached the gate as ordinary work: %+v", item)
		}
	}
}

// criterion 4: an unreadable or head-truncated log is ambiguous, and an
// ambiguous check DOES consume a round: an unreadable log is not evidence that
// the code is fine.
func TestCIStep_AmbiguousProviderLogStillSpendsARound(t *testing.T) {
	t.Parallel()
	ag, sctx := checkoutDeathContext(t, []fakeplugin.Check{
		providerBuild("buildkite/keenvc/app", "…(log truncated)\nstep output that says nothing about checkout\n", ""),
	}, 1)
	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := (&steps.CIStep{}).SetWaitForNextPoll(failOnExtraPoll)
	pinCIMonitorClock(step)
	outcome, err := stepstest.ExecuteWithAutoFix(t, step, sctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ag.Calls) != 1 {
		t.Fatalf("an ambiguous log spent %d fix rounds, want exactly 1", len(ag.Calls))
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("outcome = %+v, want the ordinary CI gate", outcome)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatalf("parse outcome findings: %v", err)
	}
	for _, item := range findings.Items {
		if item.Category == types.FindingCategoryCIInfrastructure {
			t.Fatalf("an ambiguous log was settled as infrastructure: %+v", item)
		}
	}
}

// The classification is only consulted for a check the provider attributed to
// itself, so a genuine failure whose log happens to contain acquisition text -
// a test asserting on a simulated clone failure, say - is still a failure.
func TestCIStep_GenuineFailurePrintingAcquisitionTextIsStillGenuine(t *testing.T) {
	t.Parallel()
	genuine := fakeplugin.Check{
		Name: "unit tests", Bucket: "fail", State: "FAILURE",
		Link:               "https://git.example.com/team/repo/checks/9",
		ProviderAttributed: false,
		Log:                checkoutDeath,
	}
	ag, sctx := checkoutDeathContext(t, []fakeplugin.Check{genuine}, 1)
	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := (&steps.CIStep{}).SetWaitForNextPoll(failOnExtraPoll)
	pinCIMonitorClock(step)
	if _, err := stepstest.ExecuteWithAutoFix(t, step, sctx, 0); err != nil {
		t.Fatal(err)
	}
	if len(ag.Calls) != 1 {
		t.Fatalf("an unmarked failure spent %d fix rounds, want exactly 1", len(ag.Calls))
	}
}

// The detector is gated on the transient budget like the pre-run detector, so an
// opted-out repo pays no extra provider calls and a checkout death keeps the
// prior behavior.
func TestCIStep_CheckoutDeathWithoutTheTransientBudgetIsTodaysFailure(t *testing.T) {
	t.Parallel()
	ag, sctx := checkoutDeathContext(t, []fakeplugin.Check{providerBuild("buildkite/keenvc/app", checkoutDeath, "")}, 1)
	sctx.Config.CI.RerunTransient = 0
	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := (&steps.CIStep{}).SetWaitForNextPoll(failOnExtraPoll)
	pinCIMonitorClock(step)
	if _, err := stepstest.ExecuteWithAutoFix(t, step, sctx, 0); err != nil {
		t.Fatal(err)
	}
	if len(ag.Calls) != 1 {
		t.Fatalf("opted-out repo spent %d fix rounds, want the prior behavior of 1", len(ag.Calls))
	}
}

func checkoutDeathContext(t *testing.T, checks []fakeplugin.Check, autoFixCI int) (*stepstest.MockAgent, *pipeline.StepContext) {
	t.Helper()
	// The fake provider keeps reporting the same failure after a push, so a
	// budget of one round is what makes "exactly one round" observable: a run
	// that spends its single round stops, and a run that spends none also stops.
	ag := &stepstest.MockAgent{AgentName: "test"}
	sctx, _ := pluginCIContextWithAgent(t, ag, fakeplugin.State{Checks: checks, PerCheckLogs: true})
	sctx.Config.AutoFix = config.AutoFix{CI: autoFixCI}
	sctx.Config.CI.RerunTransient = 1
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sctx.Ctx = ctx
	return ag, sctx
}
