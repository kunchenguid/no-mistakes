package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type validationDoc struct {
	Outcome string `toon:"outcome"`
	Run     struct {
		Validation struct {
			Verdict    string                   `toon:"verdict"`
			Live       int                      `toon:"live"`
			Total      int                      `toon:"total"`
			Untested   []untestedRow            `toon:"untested"`
			Exceptions []validationExceptionRow `toon:"exceptions"`
		} `toon:"validation"`
	} `toon:"run"`
	Help []string `toon:"help"`
}

const credentialReason = "needs a live payment-provider credential that is not provided"

func validationFindingsJSON(t *testing.T, verdict string) string {
	t.Helper()
	raw, err := json.Marshal(types.Findings{
		Scenarios: []types.TestScenario{
			{Name: "checkout succeeds", Result: types.ScenarioResultPass, Live: true, Evidence: "drove checkout"},
			{Name: "cart persists", Result: types.ScenarioResultPass, Live: true, Evidence: "reloaded the app"},
			{Name: "refund reaches the provider", Result: types.ScenarioResultUntested, Reason: credentialReason},
		},
		Verdict: verdict,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func renderValidationDoc(t *testing.T, r *ipc.RunInfo, ciReady bool) validationDoc {
	t.Helper()
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)
	if err := renderDriveResult(cmd, r, ciReady); err != nil {
		t.Fatal(err)
	}
	var doc validationDoc
	if err := toon.UnmarshalString(out.String(), &doc); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	return doc
}

func containsLine(lines []string, sub string) bool {
	for _, l := range lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

// A `go` run with an untested scenario and an operator skip keeps its outcome
// word, but the run view lists both with their reasons and tells the driver not
// to report the run as fully validated.
func TestAxiValidationBlock_ListsUntestedScenariosAndOperatorSkips(t *testing.T) {
	findings := validationFindingsJSON(t, types.TestVerdictGo)
	r := &ipc.RunInfo{ID: "validation-run", Status: types.RunCompleted, HeadSHA: strings.Repeat("a", 40), Steps: []ipc.StepResultInfo{
		{StepName: types.StepReview, Status: types.StepStatusCompleted},
		{StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
		{StepName: types.StepDocument, Status: types.StepStatusSkipped, SkipReason: types.OperatorSkipReason},
	}}
	doc := renderValidationDoc(t, r, false)
	if doc.Outcome != "passed" {
		t.Fatalf("outcome = %q, want the unchanged passed", doc.Outcome)
	}
	v := doc.Run.Validation
	if v.Verdict != types.TestVerdictGo || v.Live != 2 || v.Total != 3 {
		t.Fatalf("validation = %+v", v)
	}
	if len(v.Untested) != 1 || v.Untested[0].Scenario != "refund reaches the provider" || v.Untested[0].Reason != credentialReason {
		t.Fatalf("untested = %+v", v.Untested)
	}
	if len(v.Exceptions) != 1 || v.Exceptions[0] != (validationExceptionRow{Step: "document", Kind: pipeline.ValidationKindOperatorSkip, Reason: pipeline.NoReasonRecorded}) {
		t.Fatalf("exceptions = %+v", v.Exceptions)
	}
	if !containsLine(doc.Help, "1 of 3 scenarios were not validated live") || !containsLine(doc.Help, "do not describe this run as fully validated") {
		t.Fatalf("help lacks the untested report line: %q", doc.Help)
	}
	if !containsLine(doc.Help, "The document step was skipped by the operator") {
		t.Fatalf("help lacks the operator skip line: %q", doc.Help)
	}
}

// The CI-ready return keeps its bare checks-passed word, so the Test exception
// must be in the validation block a consumer can read without parsing help.
func TestAxiValidationBlock_CIReadyCarriesTheTestException(t *testing.T) {
	findings := validationFindingsJSON(t, types.TestVerdictInconclusive)
	r := &ipc.RunInfo{ID: "ci-ready-run", Status: types.RunRunning, HeadSHA: strings.Repeat("b", 40), CIReady: true,
		TestOverrideReason: "live validation verdict: inconclusive\nTest exception approved: sandbox down",
		Steps: []ipc.StepResultInfo{
			{StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
			{StepName: types.StepCI, Status: types.StepStatusRunning},
		}}
	doc := renderValidationDoc(t, r, true)
	if doc.Outcome != "checks-passed" {
		t.Fatalf("outcome = %q, want the unchanged checks-passed", doc.Outcome)
	}
	ex := doc.Run.Validation.Exceptions
	if len(ex) != 1 || ex[0].Step != "test" || ex[0].Kind != pipeline.ValidationKindTestException || !strings.Contains(ex[0].Reason, "sandbox down") {
		t.Fatalf("exceptions = %+v", ex)
	}
	if doc.Run.Validation.Verdict != types.TestVerdictInconclusive || len(doc.Run.Validation.Untested) != 1 {
		t.Fatalf("validation = %+v", doc.Run.Validation)
	}
}

func TestAxiValidationBlock_FullyLiveRunHasNoGapsOrHelp(t *testing.T) {
	raw, err := json.Marshal(types.Findings{Scenarios: []types.TestScenario{{Name: "checkout", Result: types.ScenarioResultPass, Live: true, Evidence: "drove it"}}, Verdict: types.TestVerdictGo})
	if err != nil {
		t.Fatal(err)
	}
	findings := string(raw)
	r := &ipc.RunInfo{ID: "live-run", Status: types.RunCompleted, HeadSHA: strings.Repeat("c", 40), Steps: []ipc.StepResultInfo{
		{StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}}
	doc := renderValidationDoc(t, r, false)
	if v := doc.Run.Validation; v.Live != 1 || v.Total != 1 || len(v.Untested) != 0 || len(v.Exceptions) != 0 {
		t.Fatalf("validation = %+v", v)
	}
	if containsLine(doc.Help, "not validated live") {
		t.Fatalf("fully live run got an untested help line: %q", doc.Help)
	}
}

// The DB-backed status path and the live IPC path must report the same gaps,
// mirroring the CI override reason both already carry.
func TestAxiValidationBlock_DBAndIPCPathsAgree(t *testing.T) {
	findings := validationFindingsJSON(t, types.TestVerdictGo)
	ciOverride := "lint check still red"
	operator := types.OperatorSkipReason
	automatic := "gh is not authenticated"
	run := &db.Run{ID: "agree-run", Status: types.RunCompleted, HeadSHA: strings.Repeat("d", 40)}
	rows := []*db.StepResult{
		{StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
		{StepName: types.StepDocument, Status: types.StepStatusSkipped, SkipReason: &operator},
		{StepName: types.StepPR, Status: types.StepStatusSkipped, SkipReason: &automatic},
		{StepName: types.StepCI, Status: types.StepStatusCompleted, OverrideReason: &ciOverride},
	}
	fromDB := runViewFromDB(run, rows, nil).validationGaps()
	fromIPC := runViewFromIPC(&ipc.RunInfo{ID: run.ID, Status: run.Status, HeadSHA: run.HeadSHA, CIOverrideReason: ciOverride, Steps: []ipc.StepResultInfo{
		{StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
		{StepName: types.StepDocument, Status: types.StepStatusSkipped, SkipReason: operator},
		{StepName: types.StepPR, Status: types.StepStatusSkipped, SkipReason: automatic},
		{StepName: types.StepCI, Status: types.StepStatusCompleted},
	}}).validationGaps()
	a, _ := json.Marshal(fromDB)
	b, _ := json.Marshal(fromIPC)
	if string(a) != string(b) {
		t.Fatalf("DB and IPC gaps disagree:\nDB:  %s\nIPC: %s", a, b)
	}
	kinds := map[string]bool{}
	for _, e := range fromDB.Exceptions {
		kinds[e.Kind] = true
	}
	for _, kind := range []string{pipeline.ValidationKindCIOverride, pipeline.ValidationKindOperatorSkip, pipeline.ValidationKindAutomaticSkip} {
		if !kinds[kind] {
			t.Fatalf("missing %s in %+v", kind, fromDB.Exceptions)
		}
	}
}
