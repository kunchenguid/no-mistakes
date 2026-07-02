package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/skill"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func findingsJSON(t *testing.T, items []types.Finding, summary string) string {
	t.Helper()
	raw, err := types.MarshalFindingsJSON(types.Findings{Items: items, Summary: summary})
	if err != nil {
		t.Fatalf("marshal findings: %v", err)
	}
	return raw
}

func strptr(s string) *string { return &s }

func TestRunViewFromDBAwaitingStep(t *testing.T) {
	run := &db.Run{ID: "r1", Branch: "feature/x", HeadSHA: "abcdef1234567890", Status: types.RunRunning}
	steps := []*db.StepResult{
		{StepName: types.StepReview, Status: types.StepStatusCompleted},
		{StepName: types.StepTest, Status: types.StepStatusAwaitingApproval, FindingsJSON: strptr(`{"findings":[],"summary":"x"}`)},
	}
	rv := runViewFromDB(run, steps)
	gate, ok := rv.awaitingStep()
	if !ok {
		t.Fatal("expected an awaiting step")
	}
	if gate.Name != string(types.StepTest) {
		t.Errorf("gate.Name = %q, want test", gate.Name)
	}
}

func TestFindingsTally(t *testing.T) {
	rv := runView{Steps: []stepView{
		{FindingsJSON: findingsJSON(t, []types.Finding{
			{ID: "a", Action: types.ActionAskUser, Description: "x"},
			{ID: "b", Action: types.ActionAutoFix, Description: "y"},
			{ID: "c", Action: types.ActionNoOp, Description: "z"},
			{ID: "d", Action: types.ActionAskUser, Description: "w"},
		}, "s")},
	}}
	if got := rv.findingsTally(); got != "2 awaiting, 1 auto-fix, 1 info" {
		t.Errorf("findingsTally = %q", got)
	}

	empty := runView{Steps: []stepView{{}}}
	if got := empty.findingsTally(); got != "none" {
		t.Errorf("empty findingsTally = %q, want none", got)
	}
}

func TestTruncateDisclosesTotal(t *testing.T) {
	short := truncate("hello", 100)
	if short != "hello" {
		t.Errorf("short truncate changed value: %q", short)
	}
	long := truncate(strings.Repeat("x", 50), 10)
	if !strings.Contains(long, "truncated, 50 chars total") {
		t.Errorf("truncate did not disclose total: %q", long)
	}
	if !strings.HasPrefix(long, strings.Repeat("x", 10)) {
		t.Errorf("truncate did not keep the prefix: %q", long)
	}
}

func TestWriteRunObjectShape(t *testing.T) {
	rv := runView{
		ID:      "run-1",
		Branch:  "feature/x",
		Status:  string(types.RunRunning),
		HeadSHA: "abcdef1234567890",
		Steps: []stepView{
			{Name: "review", Status: "completed", DurationMS: 1200, FindingsJSON: findingsJSON(t, []types.Finding{{ID: "r1", Action: types.ActionNoOp, Description: "ok"}}, "s")},
			{Name: "test", Status: "awaiting_approval"},
		},
	}
	out := axiDoc(runObjectField(rv))

	for _, want := range []string{
		"run:\n",
		"  id: run-1\n",
		"  branch: feature/x\n",
		"  status: running\n",
		"  head: abcdef12\n",
		"  findings: 1 info\n",
		"  steps[2]{step,status,findings,duration_ms}:\n",
		"    review,completed,1,1200\n",
		"    test,awaiting_approval,0,0\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("run object missing %q in:\n%s", want, out)
		}
	}
}

func TestRunObjectRendersAwaitingAgent(t *testing.T) {
	// Pin the clock so the parked duration is deterministic.
	restore := nowUnix
	nowUnix = func() int64 { return 1_000_000 }
	defer func() { nowUnix = restore }()

	parkedSince := int64(1_000_000 - 150) // 2m30s ago
	rv := runView{
		ID:                 "run-1",
		Branch:             "feature/x",
		Status:             string(types.RunRunning),
		HeadSHA:            "abcdef1234567890",
		AwaitingAgentSince: &parkedSince,
		Steps: []stepView{
			{Name: "review", Status: "awaiting_approval"},
		},
	}
	out := axiDoc(runObjectField(rv))
	if !strings.Contains(out, "awaiting_agent: parked 2m30s\n") {
		t.Errorf("run object missing parked signal in:\n%s", out)
	}
	// The signal sits right after status so one read distinguishes parked.
	if !strings.Contains(out, "status: running\n  awaiting_agent: parked 2m30s\n") {
		t.Errorf("awaiting_agent should follow status in:\n%s", out)
	}

	// A run that is not parked omits the signal entirely.
	rv.AwaitingAgentSince = nil
	if out := axiDoc(runObjectField(rv)); strings.Contains(out, "awaiting_agent") {
		t.Errorf("non-parked run should not render awaiting_agent in:\n%s", out)
	}

	// A terminal run never renders as parked even if a stale marker survives.
	rv.AwaitingAgentSince = &parkedSince
	rv.Status = string(types.RunCompleted)
	if out := axiDoc(runObjectField(rv)); strings.Contains(out, "awaiting_agent") {
		t.Errorf("terminal run should not render awaiting_agent in:\n%s", out)
	}
}

func TestFormatParkedFor(t *testing.T) {
	restore := nowUnix
	nowUnix = func() int64 { return 1_000_000 }
	defer func() { nowUnix = restore }()

	tests := []struct {
		name    string
		agoSecs int64
		want    string
	}{
		{"fresh seconds", 4, "parked 4s"},
		{"minutes and seconds", 150, "parked 2m30s"},
		{"hours and minutes", 3*3600 + 12*60, "parked 3h12m"},
		{"days and hours", 2*86400 + 5*3600, "parked 2d5h"},
		{"clock skew clamps to zero", -10, "parked 0s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatParkedFor(1_000_000 - tt.agoSecs); got != tt.want {
				t.Errorf("formatParkedFor = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWriteGateShape(t *testing.T) {
	gate := stepView{
		Name:   "review",
		Status: "awaiting_approval",
		FindingsJSON: findingsJSON(t, []types.Finding{
			{ID: "review-1", Severity: "warning", File: "main.go", Line: 4, Action: types.ActionAskUser, Description: "calls os.Exit, leaks fd"},
		}, "1 blocking issue"),
	}
	out := axiDoc(gateFields(gate)...)

	for _, want := range []string{
		"gate:\n",
		"  step: review\n",
		"  status: awaiting_approval\n",
		"  summary: 1 blocking issue\n",
		"  findings[1]{id,severity,file,action,description}:\n",
		`    review-1,warning,main.go,ask-user,"calls os.Exit, leaks fd"`,
		"no-mistakes axi respond --action approve",
		"to have the pipeline fix the selected findings (do not edit files yourself)",
		// Review gate carries the auto-fix-disabled note and the keep-driving
		// reminder so an agent reads them at the point of use.
		"Review auto-fix is disabled by default",
		"auto_fix.review > 0",
		"the run never advances past a gate on its own",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("gate missing %q in:\n%s", want, out)
		}
	}
}

// TestGateNote_ReviewOnly verifies the review-auto-fix-disabled note appears
// only at the review gate, while the keep-driving reminder appears at every
// gate.
func TestGateNote_ReviewOnly(t *testing.T) {
	mk := func(step string) string {
		gate := stepView{
			Name:   step,
			Status: "awaiting_approval",
			FindingsJSON: findingsJSON(t, []types.Finding{
				{ID: step + "-1", Severity: "warning", File: "main.go", Action: types.ActionAutoFix, Description: "x"},
			}, "summary"),
		}
		return axiDoc(gateFields(gate)...)
	}

	review := mk("review")
	if !strings.Contains(review, "Review auto-fix is disabled by default") {
		t.Errorf("review gate missing the auto-fix-disabled note in:\n%s", review)
	}
	if !strings.Contains(review, "auto_fix.review > 0") {
		t.Errorf("review gate missing the auto-fix override note in:\n%s", review)
	}

	lint := mk("lint")
	if strings.Contains(lint, "Review auto-fix is disabled") {
		t.Errorf("non-review gate should not carry the review note in:\n%s", lint)
	}
	if !strings.Contains(lint, "the run never advances past a gate on its own") {
		t.Errorf("every gate should carry the keep-driving reminder in:\n%s", lint)
	}
}

func TestParseAddFinding(t *testing.T) {
	f, err := parseAddFinding(`{"description":"add a nil check","action":"auto-fix","file":"x.go"}`)
	if err != nil {
		t.Fatalf("parseAddFinding: %v", err)
	}
	if f.Description != "add a nil check" || f.Action != types.ActionAutoFix || f.File != "x.go" {
		t.Errorf("parsed finding = %+v", f)
	}

	if _, err := parseAddFinding(`{"action":"auto-fix"}`); err == nil {
		t.Error("expected error for missing description")
	}
	if _, err := parseAddFinding(`not json`); err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestSplitCSV(t *testing.T) {
	got := splitCSV(" a, b ,,c ")
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("splitCSV = %#v", got)
	}
	if splitCSV("") != nil {
		t.Error("empty splitCSV should be nil")
	}
}

func TestOutcomeFor(t *testing.T) {
	cases := map[string]string{
		string(types.RunCompleted): "passed",
		string(types.RunFailed):    "failed",
		string(types.RunCancelled): "cancelled",
	}
	for in, want := range cases {
		if got := outcomeFor(in); got != want {
			t.Errorf("outcomeFor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTriggerRunDoesNotRerunAfterFailedPush(t *testing.T) {
	if shouldRerunAfterNoActiveRun(errors.New("push failed")) {
		t.Fatal("failed pushes must not fall back to rerun")
	}
	if !shouldRerunAfterNoActiveRun(nil) {
		t.Fatal("successful no-op pushes should fall back to rerun")
	}
}

func TestActiveRunLookupParamsIncludeBranch(t *testing.T) {
	params := activeRunLookupParams("repo-1", "feature/x")
	if params.RepoID != "repo-1" {
		t.Fatalf("RepoID = %q, want repo-1", params.RepoID)
	}
	if params.Branch != "feature/x" {
		t.Fatalf("Branch = %q, want feature/x", params.Branch)
	}
}

func TestActiveRunIDForHeadRequiresMatchingHead(t *testing.T) {
	active := &ipc.GetActiveRunResult{Run: &ipc.RunInfo{ID: "run-old", Status: types.RunRunning, HeadSHA: "old-head"}}

	if got := activeRunIDForHead(active, "new-head"); got != "" {
		t.Fatalf("mismatched active run ID = %q, want empty", got)
	}
	if got := activeRunIDForHead(active, "old-head"); got != "run-old" {
		t.Fatalf("matching active run ID = %q, want run-old", got)
	}

	active.Run.Status = types.RunCompleted
	if got := activeRunIDForHead(active, "old-head"); got != "" {
		t.Fatalf("terminal active run ID = %q, want empty", got)
	}
}

func TestActiveRunInfoForHeadRequiresMatchingHead(t *testing.T) {
	run := &ipc.RunInfo{ID: "run-old", Status: types.RunRunning, HeadSHA: "old-head"}

	if got := activeRunInfoForHead(run, "new-head"); got != nil {
		t.Fatalf("mismatched active run = %#v, want nil", got)
	}
	if got := activeRunInfoForHead(run, "old-head"); got == nil || got.ID != "run-old" {
		t.Fatalf("matching active run = %#v, want run-old", got)
	}

	run.Status = types.RunCompleted
	if got := activeRunInfoForHead(run, "old-head"); got != nil {
		t.Fatalf("terminal active run = %#v, want nil", got)
	}
}

func TestRerunParamsIncludeSkipSteps(t *testing.T) {
	params := rerunParams("repo-1", "feature/x", []types.StepName{types.StepReview}, "user goal")
	if params.RepoID != "repo-1" || params.Branch != "feature/x" || params.Intent != "user goal" {
		t.Fatalf("unexpected rerun params: %#v", params)
	}
	if len(params.SkipSteps) != 1 || params.SkipSteps[0] != types.StepReview {
		t.Fatalf("SkipSteps = %#v, want review", params.SkipSteps)
	}
}

func TestPreflightGuardReportsWorkingTreeCheckError(t *testing.T) {
	t.Chdir(t.TempDir())

	guard := preflightGuard(context.Background(), &axiEnv{repo: &db.Repo{DefaultBranch: "main"}}, "feature/x")
	if guard == nil {
		t.Fatal("expected guard for failed working tree check")
	}

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := guard(cmd); err == nil {
		t.Fatal("expected structured preflight error")
	}
	if !strings.Contains(out.String(), "inspect working tree") {
		t.Fatalf("expected working tree check error, got:\n%s", out.String())
	}
}

func TestStatusEmptyHelpIncludesRequiredIntent(t *testing.T) {
	out := axiDoc(
		toon.Field{Key: "runs", Value: "0 runs yet in this repository"},
		toon.Field{Key: "help", Value: []string{startRunHelp()}},
	)
	if !strings.Contains(out, "--intent") {
		t.Fatalf("empty status help must include required --intent, got:\n%s", out)
	}
}

func TestLogsNoRunHelpIncludesRequiredIntent(t *testing.T) {
	if !strings.Contains(noRunLogsHelp(), "--intent") {
		t.Fatalf("no-run logs help must include required --intent, got %q", noRunLogsHelp())
	}
}

func TestAxiHomeStartsCurrentBranchWhenOtherBranchIsActive(t *testing.T) {
	repoDir := t.TempDir()
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	run(t, repoDir, "git", "init")
	run(t, repoDir, "git", "config", "user.email", "test@test.com")
	run(t, repoDir, "git", "config", "user.name", "Test")
	run(t, repoDir, "git", "commit", "--allow-empty", "-m", "initial")
	run(t, repoDir, "git", "checkout", "-b", "feature/current")
	rawRoot, err := filepath.EvalSymlinks(repoDir)
	if err != nil {
		rawRoot = repoDir
	}
	chdir(t, rawRoot)

	p := paths.WithRoot(nmHome)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()
	repo, err := database.InsertRepoWithID("repo-1", rawRoot, "origin", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	other, err := database.InsertRun(repo.ID, "feature/other", "head-other", "base")
	if err != nil {
		t.Fatalf("insert other run: %v", err)
	}
	if err := database.UpdateRunStatus(other.ID, types.RunRunning); err != nil {
		t.Fatalf("mark other run running: %v", err)
	}
	step, err := database.InsertStepResult(other.ID, types.StepReview, 0)
	if err != nil {
		t.Fatalf("insert other step: %v", err)
	}
	if err := database.UpdateStepStatus(step.ID, types.StepStatusAwaitingApproval); err != nil {
		t.Fatalf("mark other step awaiting: %v", err)
	}
	if err := database.SetStepFindings(step.ID, findingsJSON(t, nil, "other branch gate")); err != nil {
		t.Fatalf("set other step findings: %v", err)
	}

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)
	if err := runAxiHome(cmd); err != nil {
		t.Fatalf("axi home: %v\n%s", err, out.String())
	}
	got := out.String()
	for _, want := range []string{
		"current_branch: feature/current",
		"other_branch_active_run:",
		"branch: feature/other",
		"no-mistakes axi run --intent",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("axi home missing %q in:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{
		"\nactive_run:",
		"gate:",
		"no-mistakes axi respond --action approve",
		"no-mistakes axi abort",
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("axi home should not tell the agent to act on another branch via %q, got:\n%s", forbidden, got)
		}
	}
}

// TestAxiAbortByRunIDNoOpWhenDaemonStopped covers the abort-by-id path when no
// daemon is running: a run only exists in a live daemon's memory, so there is
// nothing to cancel and the command reports a successful no-op without needing
// a repo or worktree.
func TestAxiAbortByRunIDNoOpWhenDaemonStopped(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)
	if err := runAxiAbortByRunID(cmd, "some-run-id"); err != nil {
		t.Fatalf("abort by id: %v\n%s", err, out.String())
	}
	got := out.String()
	for _, want := range []string{"aborted: false", "run: some-run-id", "daemon not running"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in abort output, got:\n%s", want, got)
		}
	}
}

func TestResolveRunPrefersCurrentBranchLatestRun(t *testing.T) {
	database := openTestDB(t)
	repo, err := database.InsertRepo(t.TempDir(), "origin", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	current, err := database.InsertRun(repo.ID, "feature/current", "head-current", "base")
	if err != nil {
		t.Fatalf("insert current run: %v", err)
	}
	if err := database.UpdateRunStatus(current.ID, types.RunCompleted); err != nil {
		t.Fatalf("complete current run: %v", err)
	}
	other, err := database.InsertRun(repo.ID, "feature/other", "head-other", "base")
	if err != nil {
		t.Fatalf("insert other run: %v", err)
	}
	if err := database.UpdateRunStatus(other.ID, types.RunRunning); err != nil {
		t.Fatalf("run other run: %v", err)
	}

	got, err := resolveRun(&axiEnv{d: database, repo: repo}, "", "feature/current")
	if err != nil {
		t.Fatalf("resolve run: %v", err)
	}
	if got == nil || got.ID != current.ID {
		t.Fatalf("resolved run = %#v, want current branch run %s", got, current.ID)
	}
}

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "no-mistakes.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func TestSkillExitCodeGuidanceDistinguishesDecisionGates(t *testing.T) {
	md := skill.Markdown()
	if strings.Contains(md, "the run blocked or failed") {
		t.Fatal("skill must not describe decision gates as exit code 1 failures")
	}
	if !strings.Contains(md, "decision gates") {
		t.Fatal("skill should explicitly identify decision gates as normal exit 0 stops")
	}
}
