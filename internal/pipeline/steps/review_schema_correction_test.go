package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// schemaRejected is the shape a text-validating adapter such as Pi returns:
// no Result, and a rejection whose error text holds only the reason while the
// complete rejected response travels beside it.
func schemaRejected(message, output string) error {
	return rejectedStructuredOutputError{message: message, output: output}
}

// correctRejectedReview stands in for a correcting model. It repairs only the
// schema slip in the rejected review handed to it through the correction
// prompt, so every finding it returns must have reached it through that
// prompt.
func correctRejectedReview(prompt string) (*agent.Result, error) {
	_, section, _ := strings.Cut(prompt, "<rejected-json>")
	section, _, _ = strings.Cut(section, "</rejected-json>")
	start, end := strings.Index(section, "{"), strings.LastIndex(section, "}")
	if start < 0 || end < start {
		return nil, errors.New("correction prompt carried no rejected review")
	}
	var review map[string]any
	if err := json.Unmarshal([]byte(section[start:end+1]), &review); err != nil {
		return nil, fmt.Errorf("correction prompt carried an incomplete rejected review: %w", err)
	}
	delete(review, "tested")
	review["risk_level"] = "medium"
	review["risk_rationale"] = "restored from the rejected review"
	review["risk_scope"] = types.FindingsRiskScopeSourceOrExternal
	output, err := json.Marshal(review)
	return &agent.Result{Output: output}, err
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
		message    string
		rejected   string
		wantPrompt string
	}{
		{
			name:       "required risk_level missing",
			message:    `JSON output missing required field "risk_level"`,
			rejected:   missingRiskLevelJSON,
			wantPrompt: `JSON output missing required field "risk_level"`,
		},
		{
			name:       "tested set to a boolean",
			message:    "JSON output tested must be array or null",
			rejected:   `{"findings":[{"severity":"warning","action":"ask-user","description":"unrequired helper","file":"a.txt","line":1,"review_scope":"source"}],"tested":true,"risk_level":"medium","risk_rationale":"one warning","risk_scope":"source-or-external"}`,
			wantPrompt: "JSON output tested must be array or null",
		},
		{
			name:       "fenced Pi output with adapter parse prefix",
			message:    `pi output parse: JSON output missing required field "risk_level"`,
			rejected:   "```json\n" + missingRiskLevelJSON + "\n```",
			wantPrompt: `JSON output missing required field "risk_level"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			calls := 0
			ag := &mockAgent{
				name: "pi",
				runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
					calls++
					if calls == 1 {
						return nil, schemaRejected(tc.message, tc.rejected)
					}
					return correctRejectedReview(opts.Prompt)
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
			for i, call := range ag.calls {
				if call.Purpose != "review" {
					t.Fatalf("call %d purpose = %q, want review", i+1, call.Purpose)
				}
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

// TestReviewStep_PiRejectionHandsTheFullReviewToCorrection covers the adapter
// shape issue #1045 hit: Pi validates its final text itself, so its rejection
// arrives with no Result and an error text holding at most a 200-character
// snippet. The correction must still receive the whole review, or findings
// past the snippet silently disappear from the corrected review.
func TestReviewStep_PiRejectionHandsTheFullReviewToCorrection(t *testing.T) {
	t.Parallel()
	descriptions := []string{
		"nil dereference when the config file is empty",
		"retry loop never backs off after a 503",
		"cache key ignores the tenant id",
	}
	items := make([]string, 0, len(descriptions))
	for i, description := range descriptions {
		items = append(items, fmt.Sprintf(`{"severity":"error","action":"auto-fix","description":%q,"file":"a.txt","line":%d,"review_scope":"source"}`, description, i+1))
	}
	rejected := "```json\n{\"findings\":[" + strings.Join(items, ",") + "],\"tested\":true}\n```"
	if len(rejected) <= 400 {
		t.Fatalf("rejected review is %d bytes, want it well past the error snippet", len(rejected))
	}

	dir, baseSHA, headSHA := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{
		name: "pi",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			calls++
			if calls == 1 {
				return nil, schemaRejected(`pi output parse: JSON output missing required field "risk_level"`, rejected)
			}
			return correctRejectedReview(opts.Prompt)
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("a Pi schema slip must be corrected, not fail the step: %v", err)
	}
	if len(ag.calls) != 2 {
		t.Fatalf("agent calls = %d, want 1 rejected review plus 1 correction", len(ag.calls))
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, item := range findings.Items {
		got[item.Description] = true
	}
	for _, description := range descriptions {
		if !got[description] {
			t.Fatalf("corrected review lost %q; findings = %+v", description, findings.Items)
		}
	}
	if len(findings.Items) != len(descriptions) {
		t.Fatalf("corrected review has %d findings, want the %d the rejected review reported", len(findings.Items), len(descriptions))
	}
	if !outcome.NeedsApproval {
		t.Fatal("the corrected review's blocking findings must still gate the step")
	}
}

// TestReviewStep_UnreadableReviewFailsClosedWithoutCorrection keeps issue
// #703's guarantee under the correction loop. With no review content to
// repair - no output at all, or no findings array - a correction turn could
// only invent a clean review, so the step fails on the first turn exactly as
// it did before corrections existed.
func TestReviewStep_UnreadableReviewFailsClosedWithoutCorrection(t *testing.T) {
	t.Parallel()
	nullFindings := `{"findings":null,"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"}`
	for _, tc := range []struct {
		name      string
		result    *agent.Result
		err       error
		wantError string
	}{
		{
			name:      "no structured output",
			err:       schemaRejected("claude returned no structured output", ""),
			wantError: "agent review: claude returned no structured output",
		},
		{
			name:      "no text output",
			err:       schemaRejected("pi returned no text output", ""),
			wantError: "agent review: pi returned no text output",
		},
		{
			name:      "prose instead of JSON",
			err:       schemaRejected("pi output parse: ended its turn with prose instead of the required JSON object", "The change looks fine to me."),
			wantError: "ended its turn with prose",
		},
		{
			name:      "null findings",
			result:    &agent.Result{Output: json.RawMessage(nullFindings)},
			wantError: "review analyzer findings missing findings array",
		},
		{
			name:      "absent findings",
			result:    &agent.Result{Output: json.RawMessage(`{"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"}`)},
			wantError: "review analyzer findings missing findings array",
		},
		{
			name:      "null findings in rejected Pi text",
			err:       schemaRejected("pi output parse: JSON output findings must be array", nullFindings),
			wantError: "findings must be array",
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
						return tc.result, tc.err
					}
					return &agent.Result{Output: json.RawMessage(validReviewJSON)}, nil
				},
			}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

			outcome, err := (&ReviewStep{}).Execute(sctx)
			if err == nil {
				t.Fatalf("an unreadable review must fail the step, got outcome %+v", outcome)
			}
			if outcome != nil {
				t.Fatalf("outcome = %+v, want none", outcome)
			}
			if len(ag.calls) != 1 {
				t.Fatalf("agent calls = %d, want 1: an unreadable review has nothing to correct", len(ag.calls))
			}
			if !strings.Contains(err.Error(), tc.wantError) || strings.Contains(err.Error(), "attempts") {
				t.Fatalf("error = %q, want the first turn's own failure %q", err, tc.wantError)
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
			return nil, schemaRejected(`JSON output missing required field "risk_level"`, missingRiskLevelJSON)
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
	if !strings.Contains(got, fmt.Sprintf("after %d attempts", analyzerCorrectionMaxAttempts)) {
		t.Fatalf("error = %q, want the exhausted bound named", got)
	}
	if !agent.IsStructuredOutputRejected(err) {
		t.Fatalf("exhausted error = %v, want a parse/schema failure", err)
	}
	if !strings.Contains(got, `missing required field "risk_level"`) {
		t.Fatalf("error = %q, want the schema reason", got)
	}
	for i, call := range ag.calls {
		if call.Purpose != "review" {
			t.Fatalf("call %d purpose = %q, want review", i+1, call.Purpose)
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
	ag := &mockAgent{
		name: "pi",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if strings.Contains(opts.Prompt, "Investigate previous review findings") {
				return &agent.Result{Output: json.RawMessage(`{"summary":"address review findings"}`)}, nil
			}
			if strings.Contains(opts.Prompt, "This is a correction-only turn") {
				return &agent.Result{Output: json.RawMessage(validReviewJSON)}, nil
			}
			return nil, schemaRejected(`JSON output missing required field "risk_level"`, missingRiskLevelJSON)
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
	for i, want := range []string{"review-fix", "review", "review"} {
		if ag.calls[i].Purpose != want {
			t.Fatalf("call %d purpose = %q, want %q", i+1, ag.calls[i].Purpose, want)
		}
	}
	if ag.calls[2].Session != nil {
		t.Fatal("rereview correction inherited a session")
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

func TestReviewStep_SchemaRejectionIsRecordedAsParseFailure(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{
		name: "pi",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			calls++
			if calls == 1 {
				return nil, schemaRejected(`JSON output missing required field "risk_level"`, missingRiskLevelJSON)
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
	if second.Purpose != "review" || second.ExitStatus != "ok" || second.FailureCategory != "" {
		t.Fatalf("correction row = purpose %q exit %q category %q, want review/ok/empty", second.Purpose, second.ExitStatus, second.FailureCategory)
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
			return nil, schemaRejected(`JSON output missing required field "risk_level"`, missingRiskLevelJSON)
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
