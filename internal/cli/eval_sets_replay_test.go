package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/eval"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// The CLI contract is that unchanged gold uses persisted queue counts, while
// excluding matched gold makes those candidates pending. Missing or corrupt
// historical output must not block inspection of unchanged case sets.
func TestEvalSetsHistoricalReplayAvailability(t *testing.T) {
	root := t.TempDir()
	t.Setenv("NM_HOME", root)
	chdir(t, t.TempDir())
	bin := buildNoMistakesBinary(t)
	findings := `{"findings":[{"id":"real-bug","severity":"error","file":"main.go","line":3,"description":"bug","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`
	f := setupEvalCLIFixture(t, context.Background(), root, findings)
	selected := `["real-bug"]`
	if err := f.db.SetStepRoundSelection(f.round.ID, &selected, db.RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}
	candidate := strings.Replace(findings, `"findings":[`, `"findings":[{"id":"extra","severity":"warning","file":"extra.go","line":20,"description":"unmatched candidate","action":"ask-user","review_scope":"source"},`, 1)
	installFakeCLIReviewAgent(t, root, candidate)
	execNoMistakes(t, bin, "eval", "capture", f.run.ID)
	execNoMistakes(t, bin, "eval", "run", "--cases", "labeled", "--candidate", "omp,model=test", "--repeats", "2")
	store, err := eval.Open(paths.WithRoot(root).EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	cases, err := store.ListCases("all")
	store.Close()
	if err != nil || len(cases) != 1 {
		t.Fatalf("cases: %v %v", cases, err)
	}
	c := cases[0]
	baseline := execNoMistakes(t, bin, "eval", "sets")
	if !strings.Contains(baseline, "2 queued") {
		t.Fatalf("expected one unmatched finding per replay: %s", baseline)
	}
	outputs, err := filepath.Glob(filepath.Join(c.Dir, "evals", "*", "*", "repeat-*.json"))
	if err != nil || len(outputs) != 2 {
		t.Fatalf("outputs: %v %v", outputs, err)
	}
	saved, err := os.ReadFile(outputs[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, damage := range []string{"missing", "corrupt"} {
		t.Run(damage, func(t *testing.T) {
			t.Cleanup(func() {
				if err := os.WriteFile(outputs[0], saved, 0o600); err != nil {
					t.Error(err)
				}
			})
			if damage == "missing" {
				err = os.Remove(outputs[0])
			} else {
				err = os.WriteFile(outputs[0], []byte("{invalid"), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := execNoMistakes(t, bin, "eval", "sets"); got != baseline {
				t.Fatalf("historical output changed sets:\n%s\n%s", baseline, got)
			}
		})
	}
	execNoMistakes(t, bin, "eval", "adjudicate", c.ID, "--finding", "real-bug", "--exclude", "--reason", "disputed gold")
	if got := execNoMistakes(t, bin, "eval", "sets"); !strings.Contains(got, "4 queued") {
		t.Fatalf("exclusion must queue both candidates in both replays: %s", got)
	}
	// Once gold is adjudicated, unavailable output cannot silently fall back
	// to the old registry score.
	if err := os.Remove(outputs[0]); err != nil {
		t.Fatal(err)
	}
	if out, err := execNoMistakesErr(t, bin, "eval", "sets"); err == nil {
		t.Fatalf("adjudicated replay silently used stale counts: %s", out)
	}
}
