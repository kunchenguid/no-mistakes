package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const failedVerificationRan = "bash tests/fm-park.test.sh reported 3 failures"

func TestCIStep_AutoFixFailedVerificationDoesNotPublish(t *testing.T) {
	t.Parallel()
	outcome := runFailedVerificationRepair(t, false, false)
	assertFailedVerificationPark(t, outcome)
}

func TestCIStep_UserFixFailedVerificationDoesNotPublish(t *testing.T) {
	t.Parallel()
	outcome := runFailedVerificationRepair(t, false, true)
	assertFailedVerificationPark(t, outcome)
}

func TestCIStep_FailedVerificationDoesNotRestartReview(t *testing.T) {
	t.Parallel()
	f, outcome := runFailedVerificationRepairFixture(t, true, true)
	assertFailedVerificationPark(t, outcome)
	if outcome.RestartFrom != "" {
		t.Fatalf("RestartFrom = %s, want no Review or Test restart", outcome.RestartFrom)
	}
	run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.ReviewApprovedHeadSHA == nil || *run.ReviewApprovedHeadSHA != f.headSHA {
		t.Fatalf("review approval = %v, want the published head %s", run.ReviewApprovedHeadSHA, f.headSHA)
	}
}

func TestCIStep_ReportedVerificationPassStillPublishes(t *testing.T) {
	t.Parallel()
	f := newFailedVerificationFixture(t, false, true)
	marked := 0
	f.sctx.MarkRunning = func() error { marked++; return nil }
	outcome, err := f.run(t)
	if outcome != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("outcome = %#v err = %v, want monitoring after a published repair", outcome, err)
	}
	if marked != 1 {
		t.Fatalf("MarkRunning calls = %d, want 1", marked)
	}
	if f.remoteHead(t) != f.localHead(t) || f.remoteHead(t) == f.headSHA {
		t.Fatalf("pass was not published: local=%s remote=%s original=%s", f.localHead(t), f.remoteHead(t), f.headSHA)
	}
}

func TestCIStep_RejectedSummaryStillWithholdsFailedVerification(t *testing.T) {
	t.Parallel()
	overlong := strings.Repeat("x", config.MaxFixMessageSummaryBytes+1)
	f := newFixerOutputFixture(t, false, `{"summary":"`+overlong+`","code_change_needed":true,"verification_passed":false,"verification_ran":"`+failedVerificationRan+`"}`)
	outcome, err := f.run(t)
	if err != nil {
		t.Fatalf("repair: %v\nlog:\n%s", err, f.log())
	}
	assertFailedVerificationPark(t, outcome)
	if f.remoteHead(t) != f.headSHA {
		t.Fatalf("remote head = %s, want the unpublished pre-repair head %s", f.remoteHead(t), f.headSHA)
	}
}

func TestCIStep_FixOverPinnedFailedRepairRunsTheFixerAndKeepsOneFinding(t *testing.T) {
	t.Parallel()
	f, first := runFailedVerificationRepairFixture(t, false, false)
	assertFailedVerificationPark(t, first)
	pinned := f.localHead(t)

	f.sctx.Ctx = context.Background()
	f.sctx.Fixing = true
	f.sctx.PreviousFindings = ciGateFindingsJSON("test")
	f.sctx.DeferredFindings = first.Findings
	outcome, err := (&CIStep{waitForNextPoll: func(context.Context, time.Duration) error {
		t.Fatal("failed verification must park before another poll")
		return nil
	}}).Execute(f.sctx)
	if err != nil {
		t.Fatalf("second fix: %v\nlog:\n%s", err, f.log())
	}
	assertFailedVerificationPark(t, outcome)
	if f.localHead(t) == pinned {
		t.Fatalf("the fixer did not run over the pinned repair:\n%s", f.log())
	}
	if f.remoteHead(t) != f.headSHA {
		t.Fatalf("remote head = %s, want the unpublished pre-repair head %s", f.remoteHead(t), f.headSHA)
	}
	parsed, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, item := range parsed.Items {
		if item.ID == types.FindingIDCIVerificationFailed {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("verification findings = %d, want 1: %+v", count, parsed.Items)
	}
}

func runFailedVerificationRepair(t *testing.T, revalidate, userFix bool) *pipeline.StepOutcome {
	t.Helper()
	_, outcome := runFailedVerificationRepairFixture(t, revalidate, userFix)
	return outcome
}

func runFailedVerificationRepairFixture(t *testing.T, revalidate, userFix bool) (*ciRepairFixture, *pipeline.StepOutcome) {
	t.Helper()
	wrapped := newFailedVerificationFixture(t, revalidate, false)
	f := wrapped.ciRepairFixture
	marked := 0
	f.sctx.MarkRunning = func() error { marked++; return nil }
	var outcome *pipeline.StepOutcome
	var err error
	if userFix {
		f.sctx.Ctx = context.Background()
		f.sctx.Fixing = true
		f.sctx.PreviousFindings = ciGateFindingsJSON("test")
		outcome, err = (&CIStep{waitForNextPoll: func(context.Context, time.Duration) error {
			t.Fatal("failed verification must park before another poll")
			return nil
		}}).Execute(f.sctx)
	} else {
		outcome, err = f.run(t)
	}
	if err != nil {
		t.Fatalf("repair: %v\nlog:\n%s", err, f.log())
	}
	if !f.sctx.Fixing {
		t.Fatal("the park must be a fix round, which the executor records as fix_review")
	}
	if marked != 0 {
		t.Fatalf("MarkRunning calls = %d, want 0", marked)
	}
	if f.remoteHead(t) != f.headSHA {
		t.Fatalf("remote head = %s, want the pre-repair head %s", f.remoteHead(t), f.headSHA)
	}
	local := f.localHead(t)
	if local == f.headSHA {
		t.Fatal("the repair commit was not created")
	}
	run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.HeadSHA != local {
		t.Fatalf("recorded head = %s, want the pinned repair %s", run.HeadSHA, local)
	}
	if run.LastPushedSHA == nil || *run.LastPushedSHA != f.headSHA {
		t.Fatalf("push binding = %v, want the pre-repair head %s", run.LastPushedSHA, f.headSHA)
	}
	if run.ReviewApprovedHeadSHA == nil || *run.ReviewApprovedHeadSHA != f.headSHA {
		t.Fatalf("review approval = %v, want the published head %s", run.ReviewApprovedHeadSHA, f.headSHA)
	}
	body, err := os.ReadFile(wrapped.bodyFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != wrapped.attestation {
		t.Fatal("attestation body changed")
	}
	message := gitCmd(t, f.dir, "log", "-1", "--format=%B")
	if strings.Contains(message, failedVerificationRan) {
		t.Fatalf("commit message contains the verification report: %s", message)
	}
	if !strings.Contains(f.log(), "not published") {
		t.Fatalf("log missing the withhold: %s", f.log())
	}
	return f, outcome
}

func assertFailedVerificationPark(t *testing.T, outcome *pipeline.StepOutcome) {
	t.Helper()
	if outcome == nil || !outcome.NeedsApproval || outcome.AutoFixable || outcome.RepairPublished || outcome.RestartFrom != "" {
		t.Fatalf("outcome = %#v, want an approval park with nothing published and no restart", outcome)
	}
	parsed, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, item := range parsed.Items {
		if item.ID != types.FindingIDCIVerificationFailed {
			continue
		}
		found = true
		if item.Action != types.ActionAskUser || item.Category != types.FindingCategoryCIVerification {
			t.Fatalf("verification finding = %+v", item)
		}
		if !strings.Contains(item.Description, failedVerificationRan) {
			t.Fatalf("description = %q, want the structured verification_ran text", item.Description)
		}
	}
	if !found {
		t.Fatalf("findings = %+v, missing %s", parsed.Items, types.FindingIDCIVerificationFailed)
	}
}

type attestationFixture struct {
	*ciRepairFixture
	attestation string
	bodyFile    string
}

func newFailedVerificationFixture(t *testing.T, revalidate, passed bool) *attestationFixture {
	t.Helper()
	passedJSON := "false"
	if passed {
		passedJSON = "true"
	}
	return newFixerOutputFixture(t, revalidate, `{"summary":"repair the failing check","code_change_needed":true,"verification_passed":`+passedJSON+`,"verification_ran":"`+failedVerificationRan+`"}`)
}

func newFixerOutputFixture(t *testing.T, revalidate bool, output string) *attestationFixture {
	t.Helper()
	f := newCIRepairFixture(t, revalidate, nil)
	original := compliantPipelineBody(t, f.headSHA)
	bodyFile := filepath.Join(t.TempDir(), "pr-body.md")
	if err := os.WriteFile(bodyFile, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	f.sctx.Env = append(f.sctx.Env, "FAKE_CLI_PR_BODY_FILE="+bodyFile, "FAKE_CLI_PR_TITLE=fix: ci")
	runs := 0
	f.sctx.Agent = &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		runs++
		if err := os.WriteFile(filepath.Join(opts.CWD, "ci-fix.txt"), []byte(strings.Repeat("fixed", runs)), 0o644); err != nil {
			return nil, err
		}
		if !strings.Contains(opts.Prompt, "verification_passed") || !strings.Contains(opts.Prompt, "verification_ran") {
			t.Errorf("prompt missing verification fields:\n%s", opts.Prompt)
		}
		if !strings.Contains(string(opts.JSONSchema), `"verification_passed"`) || !strings.Contains(string(opts.JSONSchema), `"verification_ran"`) {
			t.Errorf("schema missing verification fields: %s", opts.JSONSchema)
		}
		return &agent.Result{Output: json.RawMessage(output)}, nil
	}}
	return &attestationFixture{ciRepairFixture: f, attestation: original, bodyFile: bodyFile}
}
