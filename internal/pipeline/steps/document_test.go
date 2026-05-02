package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestDocumentStep_FixMode_UsesFallbackSummaryWhenStructuredSummaryMalformed(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			if callCount == 1 {
				os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Updated\n"), 0o644)
				return &agent.Result{Output: json.RawMessage(`{"not_summary":"oops"}`)}, nil
			}
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"docs are fine"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"severity":"warning","description":"docs outdated"}],"summary":"docs outdated"}`

	step := &DocumentStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected no approval after fallback summary commit and clean reassessment")
	}
	if got := lastCommitMessage(t, dir); got != "no-mistakes(document): update documentation" {
		t.Fatalf("last commit message = %q", got)
	}
}

func TestDocumentStep_AgentError(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return nil, errors.New("agent crashed")
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &DocumentStep{}
	_, err := step.Execute(sctx)
	if err == nil {
		t.Fatal("expected error from agent failure")
	}
	if !strings.Contains(err.Error(), "agent document") {
		t.Errorf("error = %v, want to contain 'agent document'", err)
	}
}

func TestDocumentStep_MalformedOutput(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{
				Output: json.RawMessage(`{not valid json`),
				Text:   "I updated the docs",
			}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &DocumentStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected malformed output to require approval")
	}
	if outcome.AutoFixable {
		t.Fatal("expected malformed output finding to require manual review")
	}
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("expected 1 finding, got %+v", findings.Items)
	}
	if findings.Items[0].Action != types.ActionAskUser {
		t.Fatal("expected malformed output finding to require human review")
	}
}

func TestDocumentStep_NoStructuredOutputRequiresApproval(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Text: "docs status unavailable"}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &DocumentStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected missing structured output to require approval")
	}
	if outcome.AutoFixable {
		t.Fatal("expected missing structured output finding to require manual review")
	}
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("expected 1 finding, got %+v", findings.Items)
	}
	if findings.Items[0].Action != types.ActionAskUser {
		t.Fatal("expected missing structured output finding to require human review")
	}
}

func TestDocumentStep_MissingSummaryRequiresApproval(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[]}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &DocumentStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected missing summary to require approval")
	}
	if outcome.AutoFixable {
		t.Fatal("expected missing summary finding to require manual review")
	}
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("expected 1 finding, got %+v", findings.Items)
	}
	if findings.Items[0].Action != types.ActionAskUser {
		t.Fatal("expected missing summary finding to require human review")
	}
	if findings.Summary != "agent returned no structured output" {
		t.Fatalf("summary = %q, want %q", findings.Summary, "agent returned no structured output")
	}
	if findings.Items[0].Description != "agent returned no structured output" {
		t.Fatalf("description = %q, want %q", findings.Items[0].Description, "agent returned no structured output")
	}
}

func TestDocumentStep_FixMode_CommitsAndReassesses(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			if callCount == 1 {
				// Fix call: agent writes a file and returns summary
				os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Docs\n"), 0o644)
				return &agent.Result{Output: json.RawMessage(`{"summary":"add README"}`)}, nil
			}
			// Re-assessment call: docs are now up to date, empty findings
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"docs are current"}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"severity":"warning","description":"add README"}],"summary":"add README"}`

	step := &DocumentStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if callCount != 2 {
		t.Fatalf("expected 2 agent calls (fix + reassess), got %d", callCount)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval after successful fix")
	}
	if sctx.Run.HeadSHA == headSHA {
		t.Error("expected HeadSHA to be updated after doc commit")
	}
	branchSHA := gitCmd(t, dir, "rev-parse", "refs/heads/feature")
	if branchSHA != sctx.Run.HeadSHA {
		t.Fatalf("branch SHA = %s, want %s", branchSHA, sctx.Run.HeadSHA)
	}
	if got := lastCommitMessage(t, dir); got != "no-mistakes(document): add README" {
		t.Fatalf("last commit message = %q", got)
	}
}

func TestDocumentStep_FixMode_StillNeedsWorkAfterFix(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			if callCount == 1 {
				// Fix call: agent writes partial docs
				os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Partial\n"), 0o644)
				return &agent.Result{Output: json.RawMessage(`{"summary":"partial update"}`)}, nil
			}
			// Re-assessment: still needs more work
			return &agent.Result{Output: json.RawMessage(`{"findings":[{"severity":"warning","description":"config section still missing","action":"auto-fix"}],"summary":"config section still missing"}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"severity":"warning","description":"docs outdated"}],"summary":"docs outdated"}`

	step := &DocumentStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected approval needed when re-assessment finds remaining issues")
	}
	if !outcome.AutoFixable {
		t.Fatal("expected remaining issues to be auto-fixable for another round")
	}
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("expected 1 finding, got %+v", findings.Items)
	}
	if findings.Items[0].Description != "config section still missing" {
		t.Fatalf("finding description = %q", findings.Items[0].Description)
	}
}

func TestDocumentStep_FixMode_NoChangesStillReassesses(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			if callCount == 1 {
				// Fix call: agent decides no changes needed
				return &agent.Result{Output: json.RawMessage(`{"summary":"no changes needed"}`)}, nil
			}
			// Re-assessment
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"docs are fine"}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"severity":"warning","description":"check docs"}],"summary":"check docs"}`

	step := &DocumentStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if callCount != 2 {
		t.Fatalf("expected 2 agent calls even with no changes, got %d", callCount)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval after clean re-assessment")
	}
}

func TestDocumentStep_FixMode_RequiresPreviousFindings(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true

	step := &DocumentStep{}
	_, err := step.Execute(sctx)
	if err == nil {
		t.Fatal("expected error when fixing without previous findings")
	}
	if !strings.Contains(err.Error(), "previous findings") {
		t.Errorf("error = %v, want to contain 'previous findings'", err)
	}
}
