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
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

const reviewPrepareFindings = `{"findings":[{"id":"review-1","severity":"error","file":"pane.go","line":12,"description":"Resize clamps the width but not the height","action":"auto-fix"}],"summary":"1 issue"}`

func newReviewPrepareContext(t *testing.T, optIn, fixing bool, prepare string) (*pipeline.StepContext, *mockAgent, *[]bool, string) {
	t.Helper()
	dir, base, head := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", head)
	ignoreTestDependencies(t, dir)
	var depsAtTurn []bool
	ag := &mockAgent{name: "review-prepare", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		_, err := os.Stat(filepath.Join(opts.CWD, ".deps", "count"))
		depsAtTurn = append(depsAtTurn, err == nil)
		if strings.Contains(opts.Prompt, "Investigate previous review findings") {
			return &agent.Result{Output: json.RawMessage(`{"summary":"address findings"}`)}, nil
		}
		j, _ := json.Marshal(cleanReviewFindings())
		return &agent.Result{Output: j}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Prepare: prepare, Lint: dependencyExistsCommand()})
	gateDir := t.TempDir()
	gitCmd(t, gateDir, "init", "--bare")
	sctx.GateDir = gateDir
	sctx.Shared = &pipeline.RunShared{}
	sctx.Config.Review.Prepare = optIn
	sctx.Fixing = fixing
	if fixing {
		sctx.PreviousFindings = reviewPrepareFindings
	}
	return sctx, ag, &depsAtTurn, dir
}

func TestReviewPrepare_RunsOnceBeforeFirstFixTurnAndTestReusesReceipt(t *testing.T) {
	t.Parallel()
	sctx, ag, depsAtTurn, dir := newReviewPrepareContext(t, true, true, preparationCommand())
	if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 2 || !(*depsAtTurn)[0] {
		t.Fatalf("fix turn must start with dependencies installed: calls=%d deps=%v", len(ag.calls), *depsAtTurn)
	}
	out, err := (&LintStep{}).Execute(sctx)
	if err != nil || out.ExitCode != 0 {
		t.Fatalf("lint: %v, %+v", err, out)
	}
	if got := strings.Count(readFile(t, filepath.Join(dir, ".deps", "count")), "prepared"); got != 1 {
		t.Fatalf("materializations=%d, want 1", got)
	}
}

func TestReviewPrepare_OffKeepsTodaysBehaviour(t *testing.T) {
	t.Parallel()
	sctx, ag, depsAtTurn, dir := newReviewPrepareContext(t, false, true, preparationCommand())
	if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 2 || (*depsAtTurn)[0] {
		t.Fatalf("opt-out fix turn must not prepare: calls=%d deps=%v", len(ag.calls), *depsAtTurn)
	}
	if _, err := os.Stat(filepath.Join(dir, ".deps")); !os.IsNotExist(err) {
		t.Fatalf("preparation ran without opt-in: %v", err)
	}
}

func TestReviewPrepare_CleanReviewCostsNothing(t *testing.T) {
	t.Parallel()
	sctx, ag, _, dir := newReviewPrepareContext(t, true, false, preparationCommand())
	if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("expected a single review turn, got %d", len(ag.calls))
	}
	if _, err := os.Stat(filepath.Join(dir, ".deps")); !os.IsNotExist(err) {
		t.Fatalf("clean review prepared dependencies: %v", err)
	}
}

func TestReviewPrepare_FailureStopsBeforeTheFixer(t *testing.T) {
	t.Parallel()
	sctx, ag, _, _ := newReviewPrepareContext(t, true, true, failingPreparationCommand())
	_, err := (&ReviewStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "prepare review fix dependencies") {
		t.Fatalf("expected a preparation failure, got %v", err)
	}
	if len(ag.calls) != 0 {
		t.Fatalf("fixer launched despite failed preparation: %d calls", len(ag.calls))
	}
}
