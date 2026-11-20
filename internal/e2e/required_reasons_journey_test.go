//go:build e2e

package e2e

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type attestedException struct {
	Step   string `json:"step"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

func publishedExceptions(t *testing.T, body string) []attestedException {
	t.Helper()
	const prefix = "<!-- no-mistakes-pipeline-attestation:v1 "
	start := strings.Index(body, prefix)
	if start < 0 {
		t.Fatalf("PR body has no attestation:\n%s", body)
	}
	payload := body[start+len(prefix):]
	end := strings.Index(payload, " -->")
	if end < 0 {
		t.Fatalf("PR body has an unterminated attestation:\n%s", body)
	}
	var attestation struct {
		Exceptions []attestedException `json:"exceptions"`
	}
	if err := json.Unmarshal([]byte(payload[:end]), &attestation); err != nil {
		t.Fatalf("attestation is not valid JSON: %v\n%s", err, payload[:end])
	}
	return attestation.Exceptions
}

// TestRequiredSkipReasonJourney drives issue #1707 part 3 through the real
// binary, daemon, and a stateful fake gh: `axi run --skip` without a reason is
// refused before any run starts, and the reason given with it is recorded on
// the skipped step and published in the PR attestation's exceptions.
func TestRequiredSkipReasonJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	statePath := setupStatefulGitHub(t, h)
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/required-skip-reason"
	h.CommitChange(branch, "reasons.txt", "reasons\n", "add required reasons fixture")
	wt := h.AddWorktree(branch)

	out, err := h.RunInDir(wt, "axi", "run", "--intent", "required skip reason", "--skip", "ci")
	if code := exitCode(err); code != 2 || !strings.Contains(out, "--skip requires --skip-reason") {
		t.Fatalf("reasonless --skip: exit %d, want a usage refusal\n%s", code, out)
	}
	if runs := h.Runs(); len(runs) != 0 {
		t.Fatalf("a refused --skip started %d run(s)", len(runs))
	}

	const reason = "CI is covered by the forge for this synthetic run"
	if out, err := h.RunInDir(wt, "axi", "run", "--intent", "required skip reason", "--skip", "ci", "--skip-reason", reason); err != nil {
		t.Fatalf("axi run --skip-reason: %v\n%s", err, out)
	}
	run := waitPublished(t, h, branch, "", "required skip reason")

	got := publishedExceptions(t, livePRBody(t, statePath, branch))
	want := attestedException{Step: string(types.StepCI), Kind: "operator-skip", Reason: reason}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("published exceptions = %+v, want [%+v]", got, want)
	}

	database, err := db.Open(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		recorded, ok := step.OperatorSkip()
		if ok != (step.StepName == types.StepCI) || (ok && recorded != reason) {
			t.Fatalf("%s operator skip = %q, %v; want the reason on ci only", step.StepName, recorded, ok)
		}
	}
}
