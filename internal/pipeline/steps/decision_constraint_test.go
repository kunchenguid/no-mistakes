package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The fixer has restored teardown reclamation despite a recorded decision to
// remove it. The independent decision check reports the contradiction; the
// shared commit boundary must refuse it even if the fixer claimed success.
func TestCommitAgentFixes_RejectsRecordedFixDecisionReversal(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if opts.Session != nil {
			t.Fatal("decision check must not resume the fixer session")
		}
		if !strings.Contains(opts.Prompt, "remove-teardown-reclamation") {
			t.Fatal("decision check did not receive the recorded ruling")
		}
		return &agent.Result{Output: json.RawMessage(`{"findings":[{"id":"decision-reversal","severity":"error","action":"ask-user","description":"review round 1 decision remove-teardown-reclamation is contradicted: teardown.txt restores build-output reclamation"}]}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"remove-teardown-reclamation","severity":"warning","action":"ask-user","file":"teardown.txt","description":"Remove teardown-side build-output reclamation"}]}`
	round, err := sctx.DB.InsertStepRound(sr.ID, 1, "initial", &findings, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	selected := `["remove-teardown-reclamation"]`
	if err := sctx.DB.SetStepRoundUserDecision(round.ID, &selected, db.RoundSelectionSourceUser, &findings); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "teardown.txt"), []byte("reclaim build outputs during teardown\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = commitAgentFixes(sctx, types.StepTest, "restore teardown reclamation", "")
	if err == nil || !strings.Contains(err.Error(), "remove-teardown-reclamation") {
		t.Fatalf("recorded decision reversal was not rejected with a named finding: %v", err)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("rejected fix advanced HEAD: %s", got)
	}
	if sctx.Run.HeadSHA != headSHA {
		t.Fatal("rejected fix advanced the recorded head")
	}
}
