package citest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A blocking manual job (`when: manual` with `allow_failure: false`) stops the
// pipeline at its stage until a person runs it, so GitLab itself
// treats the pipeline as unfinished. The monitor must wait on that hold - never
// report checks passed, and never call it a skip.
func TestCIStep_GitLabBlockingManualJobNeverReportsChecksPassed(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)

	checksJSON := `[{"id":1,"name":"test","status":"success","allow_failure":false},` +
		`{"id":2,"name":"deploy","status":"manual","stage":"deploy","allow_failure":false}]`
	env := stepstest.FakeCIGlab(t, "opened", checksJSON, headSHA)

	prURL := "https://gitlab.com/test/repo/-/merge_requests/42"
	ag := &stepstest.MockAgent{AgentName: "test"}
	sctx := stepstest.NewTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 5 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	step := (&steps.CIStep{}).SetWaitForNextPoll(func(ctx context.Context, interval time.Duration) error {
		cancel()
		return ctx.Err()
	})
	pinCIMonitorClock(step)
	_, err := stepstest.ExecuteWithAutoFix(t, step, sctx, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected a blocked pipeline to keep waiting while MR is open, got %v", err)
	}
	for _, line := range logs {
		if strings.Contains(line, "all CI checks passed") {
			t.Fatalf("a pipeline blocked on a manual job reported checks passed: %v", logs)
		}
	}
	found := false
	for _, line := range logs {
		if line == "CI is held on a blocking manual job awaiting a person to run it, waiting..." {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected the manual job hold to be named in the step log, got: %v", logs)
	}
}

// An optional manual job (`allow_failure: true`) is one GitLab does not wait
// for: the pipeline can succeed without it, so it stays green.
func TestCIStep_GitLabOptionalManualJobStaysGreen(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)

	checksJSON := `[{"id":1,"name":"test","status":"success","allow_failure":false},` +
		`{"id":2,"name":"deploy","status":"manual","stage":"deploy","allow_failure":true}]`
	env := stepstest.FakeCIGlab(t, "opened", checksJSON, headSHA)

	prURL := "https://gitlab.com/test/repo/-/merge_requests/42"
	ag := &stepstest.MockAgent{AgentName: "test"}
	sctx := stepstest.NewTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 5 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	step := (&steps.CIStep{}).SetWaitForNextPoll(func(ctx context.Context, interval time.Duration) error {
		cancel()
		return ctx.Err()
	})
	pinCIMonitorClock(step)
	_, err := stepstest.ExecuteWithAutoFix(t, step, sctx, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected an optional manual job to leave GitLab CI green, got %v", err)
	}
	found := false
	for _, line := range logs {
		if strings.Contains(line, "all CI checks passed - still monitoring until merged or closed") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected passing CI log for an optional manual job, got: %v", logs)
	}
}

// A genuine failure observed in the same stage as a blocking manual job reaches
// the approval gate on the first poll: the hold cannot finish on its own, so it
// must not defer the failure, and the manual job itself is not a finding anyone
// is asked to act on.
func TestCIStep_GitLabFailureBesideBlockingManualJobEscalates(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)

	checksJSON := `[{"id":1,"name":"test","status":"failed","allow_failure":false},` +
		`{"id":2,"name":"deploy","status":"manual","stage":"deploy","allow_failure":false}]`
	env := stepstest.FakeCIGlab(t, "opened", checksJSON, headSHA)

	prURL := "https://gitlab.com/test/repo/-/merge_requests/42"
	ag := &stepstest.MockAgent{AgentName: "test"}
	sctx := stepstest.NewTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 5 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 0}

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := (&steps.CIStep{}).SetWaitForNextPoll(failOnExtraPoll)
	pinCIMonitorClock(step)
	outcome, err := stepstest.ExecuteWithAutoFix(t, step, sctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected the same-stage failure to reach the approval gate, not wait on the manual hold")
	}
	for _, line := range logs {
		if strings.Contains(line, "issues detected but checks still pending") {
			t.Fatalf("the manual hold deferred the failure: %v", logs)
		}
	}

	var findings types.Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	var namedTest, namedManualJob bool
	for _, item := range findings.Items {
		if strings.Contains(item.Description, "CI check failing: test") {
			namedTest = true
		}
		if strings.Contains(item.Description, "deploy") {
			namedManualJob = true
		}
	}
	if !namedTest {
		t.Fatalf("expected the failing job in the findings, got: %+v", findings.Items)
	}
	if namedManualJob {
		t.Fatalf("the blocking manual job became a finding: %+v", findings.Items)
	}
}
