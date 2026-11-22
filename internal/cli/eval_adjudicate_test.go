package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/eval"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Each command crosses the real CLI process boundary and reopens the isolated
// store. This reproduces the shipped-unfixed path beside a proven user Fix.
func TestEvalAdjudicationCLIWorkflow(t *testing.T) {
	root := t.TempDir()
	t.Setenv("NM_HOME", root)
	chdir(t, t.TempDir())
	bin := buildNoMistakesBinary(t)
	findings := `{"findings":[{"id":"proven","severity":"error","file":"main.go","line":3,"description":"explicitly accepted defect","action":"ask-user","review_scope":"source"},{"id":"risk","severity":"warning","file":"main.go","line":4,"description":"accepted risk shipped unfixed","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"defect","risk_scope":"source-or-external"}`
	f := setupEvalCLIFixture(t, context.Background(), root, findings)
	selected := `["proven"]`
	if err := f.db.SetStepRoundSelection(f.round.ID, &selected, db.RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateStepStatus(f.step.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	execNoMistakes(t, bin, "eval", "capture", f.run.ID)
	before := execNoMistakes(t, bin, "eval", "labels")
	if !strings.Contains(before, "[recorded-human]") || strings.Contains(before, "shipped-unfixed") {
		t.Fatalf("before merge: %s", before)
	}
	if err := f.db.UpdateRunPRState(f.run.ID, "merged"); err != nil {
		t.Fatal(err)
	}
	execNoMistakes(t, bin, "eval", "relabel", f.run.ID)
	store, err := eval.Open(paths.WithRoot(root).EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	cases, err := store.ListCases("all")
	store.Close()
	if err != nil || len(cases) != 1 {
		t.Fatalf("cases %v: %v", cases, err)
	}
	c := cases[0]
	check := func(args ...string) string {
		t.Helper()
		return execNoMistakes(t, bin, append([]string{"eval"}, args...)...)
	}
	labels := check("labels", c.ID)
	if !strings.Contains(labels, "false-positive [inferred]") || !strings.Contains(labels, "recorded-shipped-unfixed") || !strings.Contains(labels, "true-positive [recorded-human]") {
		t.Fatal(labels)
	}
	manifestPath := filepath.Join(c.Dir, "manifest.json")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}

	check("adjudicate", c.ID, "--finding", "risk", "--kind", "true-positive", "--reason", "accepted risk is a real defect")
	labels = check("labels", c.ID)
	if !strings.Contains(labels, "true-positive [adjudicated]; captured false-positive") || !strings.Contains(labels, "accepted risk is a real defect") {
		t.Fatal(labels)
	}
	if out := check("sets"); !strings.Contains(out, "TP      2") || !strings.Contains(out, "FP      0") {
		t.Fatal(out)
	}

	check("adjudicate", c.ID, "--finding", "risk", "--exclude", "--reason", "disputed gold")
	check("adjudicate", c.ID, "--finding", "proven", "--exclude", "--reason", "leave case captured, exclude its gold")
	for _, args := range [][]string{{"capture", f.run.ID}, {"relabel", f.run.ID}, {"relabel"}, {"sets", "--refresh-diversified"}} {
		check(args...)
	}
	if out := check("sets"); !strings.Contains(out, "no labeled gold") {
		t.Fatal(out)
	}
	var rows []struct {
		Gold          eval.FindingGold `json:"gold"`
		EffectiveKind string           `json:"effective_kind"`
		Provenance    string           `json:"provenance"`
	}
	if err := json.Unmarshal([]byte(check("labels", c.ID, "--json")), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[1].Gold.Source != "recorded-shipped-unfixed" || rows[1].EffectiveKind != "excluded" || rows[1].Provenance != "excluded" {
		t.Fatalf("labels %#v", rows)
	}
	if after, err := os.ReadFile(manifestPath); err != nil || string(after) != string(manifest) {
		t.Fatalf("captured manifest changed: %v", err)
	}

	// Explicitly labeling an excluded finding includes it again. Same request
	// converges byte-for-byte, including the original adjudication timestamp.
	args := []string{"adjudicate", c.ID, "--finding", "risk", "--kind", "false-positive", "--reason", "confirmed invalid"}
	check(args...)
	first := check("labels", c.ID, "--json")
	check(args...)
	if second := check("labels", c.ID, "--json"); first != second {
		t.Fatalf("non-idempotent decision:\n%s\n%s", first, second)
	}
	if out := check("sets"); !strings.Contains(out, "FP      1") {
		t.Fatal(out)
	}

	// Invalid inputs must fail without changing any label or captured case.
	for _, args := range [][]string{
		{"adjudicate", c.ID, "--finding", "missing", "--kind", "true-positive", "--reason", "x"},
		{"adjudicate", "../missing", "--finding", "risk", "--kind", "true-positive", "--reason", "x"},
		{"adjudicate", c.ID, "--finding", "risk", "--kind", "made-up", "--reason", "x"},
		{"adjudicate", c.ID, "--finding", "risk", "--kind", "true-positive", "--reason", "  "},
		{"adjudicate", c.ID, "--finding", "risk", "--exclude", "--kind", "true-positive", "--reason", "x"},
		{"adjudicate", c.ID, "--finding", "risk", "--index", "1", "--exclude", "--reason", "x"},
		{"adjudicate", c.ID, "--index", "0", "--exclude", "--reason", "x"},
		{"adjudicate", c.ID, "--index", "-1", "--exclude", "--reason", "x"},
		{"adjudicate", c.ID, "--index", "99", "--exclude", "--reason", "x"},
		{"adjudicate", c.ID, "--exclude", "--reason", "x"},
		{"adjudicate", c.ID, "--finding", "risk", "--reason", "x"},
	} {
		if out, err := execNoMistakesErr(t, bin, append([]string{"eval"}, args...)...); err == nil {
			t.Fatalf("accepted %v: %s", args, out)
		}
		if after := check("labels", c.ID, "--json"); after != first {
			t.Fatalf("invalid command %v changed labels", args)
		}
	}
}
