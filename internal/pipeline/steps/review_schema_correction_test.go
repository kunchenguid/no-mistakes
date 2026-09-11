package steps

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	validReviewJSON      = `{"findings":[],"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"}`
	reviewFindingJSON    = `{"findings":[{"severity":"warning","action":"ask-user","description":"unrequired helper","file":"a.txt","line":1,"review_scope":"source"}],"risk_level":"medium","risk_rationale":"one warning","risk_scope":"source-or-external"}`
	missingRiskLevelJSON = `{"findings":[{"severity":"warning","action":"ask-user","description":"unrequired helper","file":"a.txt","line":1,"review_scope":"source"}]}`
	testedBooleanJSON    = `{"findings":[],"tested":false,"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"}`
	distinctUserIntent   = "Ship the helper only if the captain confirms it is required"
)

func schemaRejected(message string) error {
	return rejectedStructuredOutputError{message: message}
}

// TestReviewStep_InvalidSchemaTriggersCorrectionRound is issue #1045's
// recoverability contract: a reviewer whose final JSON misses required
// risk_level, or types tested as a boolean, is asked to correct rather than
// failing the step on that first attempt. The two shapes are the ones that
// ended consecutive real runs.
func TestReviewStep_InvalidSchemaTriggersCorrectionRound(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		first      func() (*agent.Result, error)
		wantPrompt string
	}{
		{
			name: "required risk_level missing",
			first: func() (*agent.Result, error) {
				return nil, schemaRejected(`JSON output missing required field "risk_level"`)
			},
			wantPrompt: `JSON output missing required field "risk_level"`,
		},
		{
			name: "tested set to false",
			first: func() (*agent.Result, error) {
				return nil, schemaRejected("JSON output tested must be array or null")
			},
			wantPrompt: "JSON output tested must be array or null",
		},
		{
			name: "adapter parse prefix does not have to be matched",
			first: func() (*agent.Result, error) {
				return nil, schemaRejected(`pi output parse: JSON output missing required field "risk_level"`)
			},
			wantPrompt: `JSON output missing required field "risk_level"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			calls := 0
			ag := &mockAgent{
				name: "pi",
				runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
					calls++
					if calls == 1 {
						return tc.first()
					}
					return &agent.Result{Output: json.RawMessage(reviewFindingJSON)}, nil
				},
			}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.UserIntent = distinctUserIntent

			outcome, err := (&ReviewStep{}).Execute(sctx)
			if err != nil {
				t.Fatalf("invalid review JSON must be returned for correction, not fail the step: %v", err)
			}
			if len(ag.calls) != 2 {
				t.Fatalf("agent calls = %d, want 1 rejected review plus 1 correction", len(ag.calls))
			}
			if ag.calls[0].Purpose != "review" {
				t.Fatalf("first purpose = %q, want review", ag.calls[0].Purpose)
			}
			if ag.calls[1].Purpose != "review-correction" {
				t.Fatalf("correction purpose = %q, want review-correction so local invocation records can tell the turns apart", ag.calls[1].Purpose)
			}
			if ag.calls[1].Session != nil {
				t.Fatal("correction inherited a session")
			}
			first := ag.calls[0].Prompt
			if strings.Contains(first, "REJECTED") {
				t.Fatalf("first review prompt must not be a correction round:\n%s", first)
			}
			correction := ag.calls[1].Prompt
			for _, want := range []string{
				"was REJECTED",
				"This is a correction-only turn",
				"Do not use tools",
				"Keep every finding",
				tc.wantPrompt,
			} {
				if !strings.Contains(correction, want) {
					t.Fatalf("correction prompt missing %q:\n%s", want, correction)
				}
			}
			for _, replayed := range []string{
				"Review the code changes and return structured findings",
				"Fix-round provenance",
				"Investigate previous review findings",
				distinctUserIntent,
				`"risk_level": "low"`,
				`"risk_level":"low"`,
			} {
				if strings.Contains(correction, replayed) {
					t.Fatalf("correction prompt replayed review-task instruction or a default risk_level %q:\n%s", replayed, correction)
				}
			}
			findings, err := types.ParseFindingsJSON(outcome.Findings)
			if err != nil {
				t.Fatal(err)
			}
			if findings.RiskLevel != "medium" || len(findings.Items) != 1 {
				t.Fatalf("corrected payload was not accepted: %+v", findings)
			}
			if findings.Items[0].Description != "unrequired helper" {
				t.Fatalf("correction dropped the rejected review's finding: %+v", findings.Items)
			}
		})
	}
}

func TestReviewStep_RejectedPayloadKeepsReportedFindings(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{
		name: "pi",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			calls++
			if calls == 1 {
				return &agent.Result{Output: json.RawMessage(missingRiskLevelJSON)}, nil
			}
			return &agent.Result{Output: json.RawMessage(reviewFindingJSON)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("missing risk assessment must enter correction, not fail the step: %v", err)
	}
	if len(ag.calls) != 2 {
		t.Fatalf("agent calls = %d, want 1 rejected payload plus 1 correction", len(ag.calls))
	}
	if !strings.Contains(ag.calls[1].Prompt, "<rejected-json>") {
		t.Fatalf("correction prompt omitted the rejected payload:\n%s", ag.calls[1].Prompt)
	}
	if !strings.Contains(ag.calls[1].Prompt, "unrequired helper") {
		t.Fatalf("correction prompt dropped the rejected findings:\n%s", ag.calls[1].Prompt)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 1 || findings.Items[0].Description != "unrequired helper" {
		t.Fatalf("corrected review lost the reported finding: %+v", findings)
	}
}

func TestReviewStep_TestedBooleanPayloadTriggersCorrectionRound(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{
		name: "pi",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			calls++
			if calls == 1 {
				return &agent.Result{Output: json.RawMessage(testedBooleanJSON)}, nil
			}
			return &agent.Result{Output: json.RawMessage(validReviewJSON)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("boolean tested must enter correction, not fail the step: %v", err)
	}
	if outcome == nil || outcome.Findings == "" {
		t.Fatal("corrected review produced no findings")
	}
	if len(ag.calls) != 2 {
		t.Fatalf("agent calls = %d, want 1 rejected payload plus 1 correction", len(ag.calls))
	}
}

func TestReviewStep_InvalidSchemaExhaustsCorrectionBound(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "pi",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return nil, schemaRejected(`JSON output missing required field "risk_level"`)
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("a review that stays invalid must fail after the correction bound")
	}
	if outcome != nil {
		t.Fatalf("Execute() outcome = %+v, want no outcome after the bound is exhausted", outcome)
	}
	if len(ag.calls) != analyzerCorrectionMaxAttempts {
		t.Fatalf("agent calls = %d, want %d", len(ag.calls), analyzerCorrectionMaxAttempts)
	}
	got := err.Error()
	if !strings.Contains(got, "after 3 attempts") && !strings.Contains(got, "after 3 attempt") {
		t.Fatalf("error = %q, want the exhausted bound named", got)
	}
	if !agent.IsStructuredOutputRejected(err) {
		t.Fatalf("exhausted error = %v, want a parse/schema failure", err)
	}
	if !strings.Contains(got, `missing required field "risk_level"`) {
		t.Fatalf("error = %q, want the schema reason", got)
	}
	if ag.calls[0].Purpose != "review" {
		t.Fatalf("first purpose = %q, want review", ag.calls[0].Purpose)
	}
	for i := 1; i < len(ag.calls); i++ {
		if ag.calls[i].Purpose != "review-correction" {
			t.Fatalf("call %d purpose = %q, want review-correction", i+1, ag.calls[i].Purpose)
		}
	}
}

func TestReviewStep_ValidFirstOutputDoesNotRetry(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "pi",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(validReviewJSON)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("agent calls = %d, want 1: a valid review must not enter a correction round", len(ag.calls))
	}
	if ag.calls[0].Purpose != "review" {
		t.Fatalf("purpose = %q, want review", ag.calls[0].Purpose)
	}
	if outcome.NeedsApproval {
		t.Fatalf("clean review must not park, findings: %s", outcome.Findings)
	}
}

func TestReviewStep_NonSchemaAgentFailureIsNotRetried(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "exit", err: errors.New("pi exited: status 1")},
		{name: "spawn", err: errors.New("pi start: executable not found")},
		{name: "transient", err: errors.New("connection reset by peer")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			want := tc.err
			ag := &mockAgent{
				name: "pi",
				runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
					return nil, want
				},
			}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

			outcome, err := (&ReviewStep{}).Execute(sctx)
			if err == nil {
				t.Fatal("non-schema agent failure must still fail the step")
			}
			if outcome != nil {
				t.Fatalf("outcome = %+v, want nil", outcome)
			}
			if len(ag.calls) != 1 {
				t.Fatalf("agent calls = %d, want 1: only schema/parse failures enter correction", len(ag.calls))
			}
			if !strings.Contains(err.Error(), want.Error()) {
				t.Fatalf("error = %q, want the original agent failure %q", err, want)
			}
			if agent.IsStructuredOutputRejected(err) {
				t.Fatalf("error = %v, must not be attributed as a parse failure", err)
			}
		})
	}
}

func TestReviewStep_RereviewSchemaFailureUsesFreshCorrection(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	calls := 0
	ag := &mockAgent{
		name: "pi",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			calls++
			if strings.Contains(opts.Prompt, "Investigate previous review findings") {
				return &agent.Result{Output: json.RawMessage(`{"summary":"address review findings"}`)}, nil
			}
			if strings.Contains(opts.Prompt, "This is a correction-only turn") {
				return &agent.Result{Output: json.RawMessage(validReviewJSON)}, nil
			}
			return nil, schemaRejected(`JSON output missing required field "risk_level"`)
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"review-1","severity":"warning","action":"auto-fix","description":"nil dereference in helper","file":"a.txt","line":1}],"summary":"1 issue"}`
	sctx.UserIntent = distinctUserIntent

	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("rereview schema slip must be correctable: %v", err)
	}
	if outcome == nil {
		t.Fatal("expected a completed rereview after correction")
	}
	if len(ag.calls) != 3 {
		t.Fatalf("agent calls = %d, want fixer + rejected rereview + correction", len(ag.calls))
	}
	if ag.calls[0].Purpose != "review-fix" {
		t.Fatalf("first purpose = %q, want review-fix", ag.calls[0].Purpose)
	}
	if ag.calls[1].Purpose != "review" {
		t.Fatalf("rereview purpose = %q, want review", ag.calls[1].Purpose)
	}
	if ag.calls[2].Purpose != "review-correction" {
		t.Fatalf("correction purpose = %q, want review-correction", ag.calls[2].Purpose)
	}
	if !strings.Contains(ag.calls[1].Prompt, "Fix-round provenance") {
		t.Fatalf("rereview prompt missing fixer provenance:\n%s", ag.calls[1].Prompt)
	}
	correction := ag.calls[2].Prompt
	for _, leaked := range []string{
		"Fix-round provenance",
		"Investigate previous review findings",
		"nil dereference in helper",
		distinctUserIntent,
		"Review the code changes and return structured findings",
	} {
		if strings.Contains(correction, leaked) {
			t.Fatalf("rereview correction inherited fixer/review context %q:\n%s", leaked, correction)
		}
	}
}

func TestReviewStep_SchemaCorrectionIsRecordedSeparately(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{
		name: "pi",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			calls++
			if calls == 1 {
				return nil, schemaRejected(`JSON output missing required field "risk_level"`)
			}
			return &agent.Result{Output: json.RawMessage(validReviewJSON)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	exec := pipeline.NewExecutor(sctx.DB, paths.WithRoot(t.TempDir()), sctx.Config, ag, []pipeline.Step{&ReviewStep{}}, nil)
	if err := exec.Execute(context.Background(), sctx.Run, sctx.Repo, dir); err != nil {
		t.Fatalf("execute: %v", err)
	}

	invocations, err := sctx.DB.GetAgentInvocationsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(invocations) != 2 {
		t.Fatalf("got %d invocation rows, want 2", len(invocations))
	}
	first, second := invocations[0], invocations[1]
	if first.Purpose != "review" || first.ExitStatus != "error" || first.FailureCategory != "parse" {
		t.Fatalf("first row = purpose %q exit %q category %q, want review/error/parse", first.Purpose, first.ExitStatus, first.FailureCategory)
	}
	if second.Purpose != "review-correction" || second.ExitStatus != "ok" || second.FailureCategory != "" {
		t.Fatalf("correction row = purpose %q exit %q category %q, want review-correction/ok/empty", second.Purpose, second.ExitStatus, second.FailureCategory)
	}
	if first.SessionMode != db.InvocationModeCold || second.SessionMode != db.InvocationModeCold {
		t.Fatalf("session modes = %q/%q, want cold/cold", first.SessionMode, second.SessionMode)
	}
}

func TestReviewStep_ExhaustedSchemaFailureNeverPasses(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "pi",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return nil, schemaRejected(`JSON output missing required field "risk_level"`)
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	exec := pipeline.NewExecutor(sctx.DB, paths.WithRoot(t.TempDir()), sctx.Config, ag, []pipeline.Step{&ReviewStep{}}, nil)
	if err := exec.Execute(context.Background(), sctx.Run, sctx.Repo, dir); err == nil {
		t.Fatal("exhausted schema correction must fail the run")
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != types.RunFailed {
		t.Fatalf("run status = %s, want %s", run.Status, types.RunFailed)
	}
	if run.ReviewApprovedHeadSHA != nil {
		t.Fatalf("unreadable review gained approval authority: %#v", run.ReviewApprovedHeadSHA)
	}
	invocations, err := sctx.DB.GetAgentInvocationsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(invocations) != analyzerCorrectionMaxAttempts {
		t.Fatalf("got %d invocation rows, want %d", len(invocations), analyzerCorrectionMaxAttempts)
	}
	for i, inv := range invocations {
		if inv.ExitStatus != "error" || inv.FailureCategory != "parse" {
			t.Fatalf("row %d = exit %q category %q, want error/parse", i, inv.ExitStatus, inv.FailureCategory)
		}
	}
}
