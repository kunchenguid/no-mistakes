package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func executeAxiRounds(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append([]string{"axi", "rounds"}, args...))
	err := cmd.Execute()
	return out.String(), err
}

func insertAxiRound(t *testing.T, d *db.DB, stepID string, number int, trigger string, findings *string) *db.StepRound {
	t.Helper()
	r, err := d.InsertStepRound(stepID, number, trigger, findings, nil, int64(number*10))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAxiRoundsExportsStoredFindingsAndDecisions(t *testing.T) {
	repoDir, _, database, repo := setupAxiQueryRepo(t)
	chdir(t, repoDir)
	run(t, repoDir, "git", "checkout", "-b", "feature/rounds")
	r, err := database.InsertRun(repo.ID, "feature/rounds", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(r.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	description := longAskUserDescription() + " 完整な説明\nOptions: keep, remove"
	payload, err := json.Marshal(map[string]any{
		"summary":  "Initial review",
		"findings": []map[string]any{{"id": "r1", "description": description, "line": 42, "extra": "preserved", "large": json.Number("9007199254740993")}, {"id": "r2", "description": "another issue"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	initialJSON := string(payload)
	first := insertAxiRound(t, database, step.ID, 1, "initial", &initialJSON)
	selection := `["r1"]`
	// Stored through the production marshaller, which writes a findings object
	userFindings, err := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{
		{ID: "r1", Description: "selected", UserInstructions: "keep compatibility"},
		{ID: "added", Description: "operator addition"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetStepRoundUserDecision(first.ID, &selection, db.RoundSelectionSourceUser, &userFindings, types.ResponseOriginInteractive); err != nil {
		t.Fatal(err)
	}
	secondJSON := `{"summary":"Still open","findings":[{"id":"r1","description":"same ID, later round"}]}`
	fixSummary := "Updated retry handling"
	second, err := database.InsertStepRound(step.ID, 2, "auto_fix", &secondJSON, &fixSummary, 20)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetStepRoundSelection(second.ID, &selection, db.RoundSelectionSourceAutoFix); err != nil {
		t.Fatal(err)
	}
	thirdJSON := `{"summary":"Decision needed","findings":[{"id":"r3","description":"tradeoff"}]}`
	third := insertAxiRound(t, database, step.ID, 3, "coverage", &thirdJSON)
	emptySelection := "[]"
	if err := database.SetStepRoundUserDecision(third.ID, &emptySelection, db.RoundSelectionSourceUserDeclined, nil, types.ResponseOriginAutoYes); err != nil {
		t.Fatal(err)
	}
	cleanJSON := `{"summary":"Clean","findings":[]}`
	insertAxiRound(t, database, step.ID, 4, "answer", &cleanJSON)
	insertAxiRound(t, database, step.ID, 5, "retry", nil)
	before, err := database.GetRoundsByStep(step.ID)
	if err != nil {
		t.Fatal(err)
	}

	out, err := executeAxiRounds(t, "--step", "review", "--json")
	if err != nil {
		t.Fatalf("rounds: %v\n%s", err, out)
	}
	var doc axiRoundsDocument
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.SchemaVersion != 1 || doc.RunID != r.ID || doc.Step != "review" || len(doc.Rounds) != 5 {
		t.Fatalf("unexpected document: %s", out)
	}
	for i, round := range doc.Rounds {
		if round.Round != i+1 || round.DurationMS != int64((i+1)*10) || round.CreatedAt == 0 {
			t.Fatalf("round order or timing lost: %+v", round)
		}
	}
	if !strings.Contains(out, `"large":9007199254740993`) {
		t.Fatalf("optional numeric finding field was rounded: %s", out)
	}
	firstView := doc.Rounds[0]
	if firstView.Findings[0]["description"] != description || firstView.Findings[0]["extra"] != "preserved" || firstView.Findings[0]["line"] != float64(42) {
		t.Fatalf("finding fields were altered: %+v", firstView.Findings)
	}
	if !reflect.DeepEqual(firstView.SelectedFindingIDs, []string{"r1"}) || *firstView.SelectionSource != "user" || firstView.ResponseOrigin != "interactive" || len(firstView.UserFindings) != 2 {
		t.Fatalf("user decision was lost: %+v", firstView)
	}
	if *doc.Rounds[1].SelectionSource != "auto_fix" || doc.Rounds[1].Findings[0]["description"] != "same ID, later round" || *doc.Rounds[1].FixSummary != fixSummary {
		t.Fatalf("automatic selection or repeated ID changed: %+v", doc.Rounds[1])
	}
	if doc.Rounds[2].SelectedFindingIDs == nil || len(doc.Rounds[2].SelectedFindingIDs) != 0 || *doc.Rounds[2].SelectionSource != "user_declined" {
		t.Fatalf("explicit decline not distinguished: %+v", doc.Rounds[2])
	}
	if doc.Rounds[3].SelectedFindingIDs != nil || doc.Rounds[3].SelectionSource != nil || doc.Rounds[3].Findings == nil || doc.Rounds[4].Findings != nil {
		t.Fatalf("absent findings/decision confused with an empty array: %s", out)
	}

	toonOut, err := executeAxiRounds(t, "--step", "review", "--run", r.ID)
	if err != nil {
		t.Fatalf("TOON: %v\n%s", err, toonOut)
	}
	for _, want := range []string{"schema_version: 1", "rounds[5]:", strconv.Quote(description), "selection_source: user_declined", "selected_finding_ids: null", "extra: preserved", `large: "9007199254740993"`, "line: 42"} {
		if !strings.Contains(toonOut, want) {
			t.Fatalf("TOON output missing %q:\n%s", want, toonOut)
		}
	}
	after, err := database.GetRoundsByStep(step.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("query changed stored rounds: %v", err)
	}
}

func TestAxiRoundsRunScopeAndEmptyHistory(t *testing.T) {
	repoDir, _, database, repo := setupAxiQueryRepo(t)
	chdir(t, repoDir)
	run(t, repoDir, "git", "checkout", "-b", "feature/current")
	current, err := database.InsertRun(repo.ID, "feature/current", "current", "base")
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := database.InsertRun(repo.ID, "feature/other", "other", "base")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []*db.Run{current, foreign} {
		if _, err := database.InsertStepResult(r.ID, types.StepReview); err != nil {
			t.Fatal(err)
		}
		if err := database.UpdateRunStatus(r.ID, types.RunCompleted); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"--step", "review", "--json"}, {"--step", "review", "--json", "--run", foreign.ID}} {
		out, err := executeAxiRounds(t, args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		var doc axiRoundsDocument
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatal(err)
		}
		want := current.ID
		if len(args) > 3 {
			want = foreign.ID
		}
		if doc.RunID != want || doc.Rounds == nil || len(doc.Rounds) != 0 {
			t.Fatalf("wrong scope or empty-history shape: %s", out)
		}
	}
	run(t, repoDir, "git", "checkout", "-b", "feature/no-runs")
	out, err := executeAxiRounds(t, "--step", "review", "--json")
	if err == nil || !strings.Contains(out, "no run found for this branch") {
		t.Fatalf("must not fall back to another branch: %v\n%s", err, out)
	}
	run(t, repoDir, "git", "checkout", "--detach")
	if out, err := executeAxiRounds(t, "--step", "review", "--json"); err == nil {
		t.Fatalf("detached HEAD must require an explicit run: %s", out)
	}
	chdir(t, t.TempDir())
	if out, err := executeAxiRounds(t, "--step", "review", "--run", foreign.ID, "--json"); err != nil {
		t.Fatalf("explicit query should work outside a checkout: %v\n%s", err, out)
	}
}

func TestAxiRoundsCarriedStepExportsItsSourceRounds(t *testing.T) {
	repoDir, _, database, repo := setupAxiQueryRepo(t)
	chdir(t, repoDir)
	source, err := database.InsertRun(repo.ID, "feature/carried", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	sourceStep, err := database.InsertStepResult(source.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"summary":"Source review","findings":[{"id":"r1","description":"carried issue"}]}`
	insertAxiRound(t, database, sourceStep.ID, 1, "initial", &findings)
	if err := database.UpdateStepStatus(sourceStep.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	sourceStep, err = database.GetStepResult(sourceStep.ID)
	if err != nil {
		t.Fatal(err)
	}
	continued, err := database.InsertRun(repo.ID, "feature/carried", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertCarriedStepResult(continued.ID, sourceStep); err != nil {
		t.Fatal(err)
	}

	out, err := executeAxiRounds(t, "--step", "review", "--run", continued.ID, "--json")
	if err != nil {
		t.Fatalf("rounds: %v\n%s", err, out)
	}
	var doc axiRoundsDocument
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.RunID != continued.ID || len(doc.Rounds) != 1 || doc.Rounds[0].Findings[0]["description"] != "carried issue" {
		t.Fatalf("carried step lost its source rounds: %s", out)
	}
}

func TestAxiRoundsErrorsAreNotCleanHistory(t *testing.T) {
	repoDir, _, database, repo := setupAxiQueryRepo(t)
	chdir(t, repoDir)
	r, err := database.InsertRun(repo.ID, "feature/errors", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertStepResult(r.ID, types.StepReview); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, findings, selected, userFindings, want string
	}{
		{"broken findings", `{"findings":[`, "[]", `{"findings":[]}`, "parse findings"},
		{"wrong findings type", `{"findings":{}}`, "[]", `{"findings":[]}`, "parse findings"},
		{"broken selection", `{"findings":[]}`, "[1]", `{"findings":[]}`, "parse selected finding IDs"},
		{"bare user findings array", `{"findings":[]}`, "[]", "[]", "parse user findings"},
		{"null finding member", `{"findings":[null]}`, "[]", `{"findings":[]}`, "parse findings"},
		{"null selection member", `{"findings":[]}`, "[null]", `{"findings":[]}`, "parse selected finding IDs"},
		{"null user finding member", `{"findings":[]}`, "[]", `{"findings":[null]}`, "parse user findings"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := database.InsertRun(repo.ID, "feature/corrupt", "head", "base")
			if err != nil {
				t.Fatal(err)
			}
			step, err := database.InsertStepResult(r.ID, types.StepReview)
			if err != nil {
				t.Fatal(err)
			}
			round := insertAxiRound(t, database, step.ID, 1, "initial", &tc.findings)
			if err := database.SetStepRoundUserDecision(round.ID, &tc.selected, "user", &tc.userFindings); err != nil {
				t.Fatal(err)
			}
			for _, jsonFlag := range [][]string{{}, {"--json"}} {
				out, err := executeAxiRounds(t, append([]string{"--step", "review", "--run", r.ID}, jsonFlag...)...)
				if err == nil || !strings.Contains(out, tc.want) || !strings.Contains(out, "round 1") {
					t.Fatalf("corrupt data must fail visibly: %v\n%s", err, out)
				}
				if len(jsonFlag) > 0 && !json.Valid([]byte(out)) {
					t.Fatalf("error is not JSON: %s", out)
				}
			}
		})
	}
	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{"--json"}, 2},
		{[]string{"--step", "../bad", "--json"}, 2},
		{[]string{"--step", "review", "--run", "missing", "--json"}, 1},
		{[]string{"--step", "lint", "--run", r.ID, "--json"}, 1},
	} {
		out, err := executeAxiRounds(t, tc.args...)
		var exit *exitError
		if !errors.As(err, &exit) || exit.code != tc.code || !json.Valid([]byte(out)) {
			t.Fatalf("wrong error for %v: %v\n%s", tc.args, err, out)
		}
	}
}

func TestAxiRoundsMissingDatabaseDoesNotCreateState(t *testing.T) {
	home := filepath.Join(t.TempDir(), "absent")
	t.Setenv("NM_HOME", home)
	out, err := executeAxiRounds(t, "--step", "review", "--run", "missing", "--json")
	if err == nil || !json.Valid([]byte(out)) {
		t.Fatalf("missing database should fail: %v\n%s", err, out)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("query created state directories: %v", err)
	}
}
