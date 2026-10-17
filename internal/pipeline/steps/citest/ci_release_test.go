package citest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/cimonitor"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
)

func TestCIStep_GreenChecksReleaseWithoutWaitingForTheMerge(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)

	env, logFile := stepstest.FakeCIGHLoggedSequence(t, "OPEN",
		[]string{`[{"name":"build","state":"SUCCESS","bucket":"pass"}]`}, "MERGEABLE", "")

	prURL := "https://github.com/test/repo/pull/42"
	ag := &stepstest.MockAgent{AgentName: "test"}
	sctx := stepstest.NewTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.CIReadinessChanged = func(ready, _ bool) {
		if ready {
			t.Error("release must not publish live-monitor readiness")
		}
	}
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 10 * time.Minute

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := (&steps.CIStep{}).SetWaitForNextPoll(func(context.Context, time.Duration) error {
		t.Fatal("the default releases the run at green and must never poll again")
		return nil
	})
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("CI step returned error: %v", err)
	}
	if outcome == nil || outcome.NeedsApproval || outcome.AutoFixable || outcome.Findings != "" {
		t.Fatalf("outcome = %#v, want a plain success verdict", outcome)
	}

	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.CIReadyAt != nil {
		t.Fatal("release readiness must wait for atomic step completion")
	}
	if outcome.CIReadyNoCI == nil || *outcome.CIReadyNoCI {
		t.Fatal("green verdict must request completion with observed-check readiness")
	}
	if run.CIReadyNoCI {
		t.Fatal("green checks must not claim the trusted no_ci declaration")
	}
	if run.PRState == nil || *run.PRState != "open" {
		t.Fatalf("PR state = %v, want open at the release point", run.PRState)
	}

	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, cimonitor.ChecksPassedCompleteMsg) {
		t.Fatalf("logs = %v, want the release verdict", logs)
	}
	if strings.Contains(joined, cimonitor.ChecksPassedMsg) {
		t.Fatalf("logs = %v, must not claim the monitor is still watching", logs)
	}

	if got := countGHStateReads(t, logFile); got != 1 {
		t.Fatalf("PR state reads = %d, want the single observation the release is made on", got)
	}
}

// TestCIStep_TrustedNoCIReleasesWithoutWaiting is the same release for the
// other readiness path: a trusted default-branch no_ci declaration with zero
// registered checks. The declaration stays inspectable on the release line.
func TestCIStep_TrustedNoCIReleasesWithoutWaiting(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)

	env := stepstest.FakeCIGH(t, "OPEN", "[]")

	prURL := "https://github.com/test/repo/pull/42"
	ag := &stepstest.MockAgent{AgentName: "test"}
	sctx := stepstest.NewTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.CIReadinessChanged = func(ready, _ bool) {
		if ready {
			t.Error("release must not publish live-monitor readiness")
		}
	}
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 10 * time.Minute
	sctx.Config.NoCI = true

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := (&steps.CIStep{}).SetWaitForNextPoll(func(context.Context, time.Duration) error {
		t.Fatal("a declared no-CI head releases at readiness and must never poll again")
		return nil
	})
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("CI step returned error: %v", err)
	}
	if outcome == nil || outcome.NeedsApproval || outcome.AutoFixable || outcome.Findings != "" {
		t.Fatalf("outcome = %#v, want a plain success verdict", outcome)
	}

	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.CIReadyAt != nil {
		t.Fatal("release readiness must wait for atomic step completion")
	}
	if outcome.CIReadyNoCI == nil || !*outcome.CIReadyNoCI {
		t.Fatal("no-CI verdict must request completion with declared readiness")
	}
	if !strings.Contains(strings.Join(logs, "\n"), cimonitor.NoChecksPassedCompleteMsg) {
		t.Fatalf("logs = %v, want the declared no-CI release verdict", logs)
	}
}

// countGHStateReads counts the `gh pr view --json state` invocations the CI
// step issued, which is its observation window over the PR's lifecycle.
func countGHStateReads(t *testing.T, logFile string) int {
	t.Helper()
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(ghLog(t, logFile)), "\n") {
		if strings.Contains(line, "pr view") && strings.Contains(line, "--json state") {
			count++
		}
	}
	return count
}
