package steps

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	blockingReviewJSON = `{"findings":[{"severity":"error","action":"auto-fix","description":"nil dereference when the config file is empty","file":"a.txt","line":1,"review_scope":"source"}],"risk_level":"high","risk_rationale":"one defect","risk_scope":"source-or-external"}`
	cleanReviewJSON    = `{"findings":[],"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"}`
)

// TestReviewStep_SchemaRejectionRerunsAFreshReview is issue #1045's Pi case:
// the adapter rejects the reviewer's final JSON and returns no Result. Review
// reruns the same session-free review, told only the validation error, and
// takes its findings from the attempt that validates.
func TestReviewStep_SchemaRejectionRerunsAFreshReview(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	const validation = `pi output parse: JSON output missing required field "risk_level"`
	calls := 0
	ag := &mockAgent{
		name: "pi",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			calls++
			if calls == 1 {
				return nil, rejectedStructuredOutputError{message: validation}
			}
			return &agent.Result{Output: json.RawMessage(blockingReviewJSON)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("a schema slip must rerun the review, not fail the step: %v", err)
	}
	if len(ag.calls) != 2 {
		t.Fatalf("agent calls = %d, want the rejected review plus one rerun", len(ag.calls))
	}
	for i, call := range ag.calls {
		if call.Session != nil || call.Purpose != "review" {
			t.Fatalf("call %d = purpose %q session %+v, want a session-free review", i+1, call.Purpose, call.Session)
		}
	}
	note, ok := strings.CutPrefix(ag.calls[1].Prompt, ag.calls[0].Prompt)
	if !ok {
		t.Fatalf("rerun prompt is not the original review prompt plus a note:\n%s", ag.calls[1].Prompt)
	}
	if !strings.Contains(note, "REJECTED") || !strings.Contains(note, validation) {
		t.Fatalf("rerun note = %q, want the rejection and its validation error", note)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || len(findings.Items) != 1 || findings.Items[0].Description != "nil dereference when the config file is empty" {
		t.Fatalf("outcome = %+v, want Review parked on the rerun's blocking finding", outcome)
	}
	if !slices.ContainsFunc(logs, func(line string) bool {
		return strings.Contains(line, validation) && strings.Contains(line, "attempt 2 of 3")
	}) {
		t.Fatalf("logs = %q, want one retry line naming the validation error and attempt 2 of 3", logs)
	}
}

func TestReviewStep_InvalidReviewOutputRerunsAFreshReview(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{
		name: "claude",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			calls++
			if calls == 1 {
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"risk_rationale":"clean","risk_scope":"source-or-external"}`)}, nil
			}
			return &agent.Result{Output: json.RawMessage(cleanReviewJSON)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("a missing risk_level must rerun the review, not fail the step: %v", err)
	}
	if len(ag.calls) != 2 {
		t.Fatalf("agent calls = %d, want the invalid review plus one rerun", len(ag.calls))
	}
	if !strings.Contains(ag.calls[1].Prompt, "review analyzer findings missing risk assessment") {
		t.Fatalf("rerun prompt does not name the validation error:\n%s", ag.calls[1].Prompt)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval || len(findings.Items) != 0 || findings.RiskLevel != "low" {
		t.Fatalf("outcome = %+v, want Review completed with the rerun's clean review", outcome)
	}
}

func TestReviewStep_InvalidReviewExhaustsTheRetryBound(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	const last = "pi output parse: JSON output tested must be array or null"
	calls := 0
	ag := &mockAgent{
		name: "pi",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			calls++
			switch calls {
			case 1:
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"clean"}`)}, nil
			case 2:
				return &agent.Result{Output: json.RawMessage(`{"findings":null,"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"}`)}, nil
			default:
				return nil, rejectedStructuredOutputError{message: last}
			}
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err == nil || outcome != nil {
		t.Fatalf("Execute() = %+v, %v; an invalid review must never pass", outcome, err)
	}
	if len(ag.calls) != reviewAnalyzerMaxAttempts {
		t.Fatalf("agent calls = %d, want %d", len(ag.calls), reviewAnalyzerMaxAttempts)
	}
	if !strings.Contains(err.Error(), "validate review analyzer findings after 3 attempts") || !strings.Contains(err.Error(), last) {
		t.Fatalf("error = %q, want the exhausted bound wrapping the last attempt's validation error", err)
	}
	if !agent.IsStructuredOutputRejected(err) {
		t.Fatalf("error = %v, want it to wrap the last attempt's structured-output rejection", err)
	}
}

func TestReviewStep_NonSchemaFailuresAreNotRetried(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		timeout   time.Duration
		run       func(ctx context.Context) error
		wantError string
	}{
		{
			name:      "agent error",
			run:       func(context.Context) error { return errors.New("pi exited: status 1") },
			wantError: "pi exited: status 1",
		},
		{
			name:    "timeout that ends in a schema rejection",
			timeout: 20 * time.Millisecond,
			run: func(ctx context.Context) error {
				<-ctx.Done()
				return rejectedStructuredOutputError{message: `pi output parse: JSON output missing required field "risk_level"`}
			},
			wantError: "reached its absolute wall-clock limit after 20ms",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			ag := &mockAgent{
				name: "pi",
				runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
					return nil, tc.run(ctx)
				},
			}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			if tc.timeout > 0 {
				sctx.Config.ReviewAgentTimeout = tc.timeout
			}

			outcome, err := (&ReviewStep{}).Execute(sctx)
			if err == nil || outcome != nil {
				t.Fatalf("Execute() = %+v, %v; want the failure returned", outcome, err)
			}
			if len(ag.calls) != 1 {
				t.Fatalf("agent calls = %d, want 1: only a schema slip reruns the review", len(ag.calls))
			}
			if !strings.Contains(err.Error(), tc.wantError) || strings.Contains(err.Error(), "attempts") {
				t.Fatalf("error = %q, want the unchanged failure %q", err, tc.wantError)
			}
		})
	}
}

func TestReviewStep_RereviewSchemaRejectionRerunsAFreshReview(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	reviews := 0
	ag := &mockAgent{
		name: "pi",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if strings.Contains(opts.Prompt, "Investigate previous review findings") {
				return &agent.Result{Output: json.RawMessage(`{"summary":"address review findings"}`)}, nil
			}
			reviews++
			if reviews == 1 {
				return nil, rejectedStructuredOutputError{message: `pi output parse: JSON output missing required field "risk_level"`}
			}
			return &agent.Result{Output: json.RawMessage(cleanReviewJSON)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"review-1","severity":"warning","action":"auto-fix","description":"nil dereference in helper","file":"a.txt","line":1}],"summary":"1 issue"}`

	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("a rereview schema slip must rerun the review: %v", err)
	}
	if outcome == nil || outcome.NeedsApproval {
		t.Fatalf("outcome = %+v, want the rereview completed by its rerun", outcome)
	}
	if len(ag.calls) != 3 {
		t.Fatalf("agent calls = %d, want fixer, rejected rereview, and one rerun", len(ag.calls))
	}
	for i, want := range []string{"review-fix", "review", "review"} {
		if ag.calls[i].Purpose != want {
			t.Fatalf("call %d purpose = %q, want %q", i+1, ag.calls[i].Purpose, want)
		}
	}
	if ag.calls[1].Session != nil || ag.calls[2].Session != nil {
		t.Fatal("a rereview attempt ran with a session")
	}
	if !strings.HasPrefix(ag.calls[2].Prompt, ag.calls[1].Prompt) {
		t.Fatalf("rerun prompt is not the original rereview prompt plus a note:\n%s", ag.calls[2].Prompt)
	}
}
