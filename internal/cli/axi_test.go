package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
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
	} {
		if !strings.Contains(out, want) {
			t.Errorf("gate missing %q in:\n%s", want, out)
		}
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

func TestSkillExitCodeGuidanceDistinguishesDecisionGates(t *testing.T) {
	md := skill.Markdown()
	if strings.Contains(md, "the run blocked or failed") {
		t.Fatal("skill must not describe decision gates as exit code 1 failures")
	}
	if !strings.Contains(md, "decision gates") {
		t.Fatal("skill should explicitly identify decision gates as normal exit 0 stops")
	}
}
