package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestEvalSetsIsLocalOnlyAndEmitsNoTelemetry(t *testing.T) {
	t.Setenv("NM_HOME", t.TempDir())
	chdir(t, t.TempDir())

	recorder := &telemetryRecorder{}
	restore := telemetry.SetDefaultForTesting(recorder)
	defer restore()

	out, err := executeCmd("eval", "sets")
	if err != nil {
		t.Fatalf("eval sets: %v", err)
	}
	if !strings.Contains(out, "LOCAL-ONLY EVAL CASE SETS") {
		t.Fatalf("output = %q", out)
	}
	if strings.Contains(out, "verdict") || strings.Contains(out, "park") || strings.Contains(out, ", pass ") {
		t.Fatalf("eval sets still uses park/pass accuracy language: %q", out)
	}
	if recorder.count("command") != 0 || recorder.count("pageview") != 0 {
		t.Fatalf("eval emitted remote telemetry: %#v", recorder.events)
	}
}

func TestEvalCaptureAndSetsSpeakInFindingGoldTerms(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("NM_HOME", root)
	chdir(t, t.TempDir())

	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	gateDir := p.RepoDir("eval-repo")
	if err := git.InitBare(ctx, gateDir); err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(root, "source")
	mustCLIGit(t, ctx, root, "clone", gateDir, workDir)
	mustCLIGit(t, ctx, workDir, "config", "user.email", "eval@example.test")
	mustCLIGit(t, ctx, workDir, "config", "user.name", "Eval Test")
	if err := os.WriteFile(filepath.Join(workDir, "main.go"), []byte("package sample\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustCLIGit(t, ctx, workDir, "add", ".")
	mustCLIGit(t, ctx, workDir, "commit", "-m", "base")
	mustCLIGit(t, ctx, workDir, "branch", "-M", "main")
	mustCLIGit(t, ctx, workDir, "push", "origin", "main")
	baseSHA := mustCLIGit(t, ctx, workDir, "rev-parse", "HEAD")
	mustCLIGit(t, ctx, workDir, "checkout", "-b", "feature/eval")
	if err := os.WriteFile(filepath.Join(workDir, "main.go"), []byte("package sample\n\nfunc Changed() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustCLIGit(t, ctx, workDir, "add", "main.go")
	mustCLIGit(t, ctx, workDir, "commit", "-m", "change")
	mustCLIGit(t, ctx, workDir, "push", "origin", "feature/eval")
	headSHA := mustCLIGit(t, ctx, workDir, "rev-parse", "HEAD")

	repo, err := database.InsertRepoWithID("eval-repo", workDir, "https://example.test/org/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature/eval", headSHA, baseSHA)
	if err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"real-bug","severity":"error","file":"main.go","line":3,"description":"bug","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`
	round, err := database.InsertReviewStepRoundWithProvenance(step.ID, 1, "initial", &findings, nil, headSHA, headSHA, baseSHA, []byte("{}\n"), []byte("{}\n"), 50)
	if err != nil {
		t.Fatal(err)
	}
	selected := `["real-bug"]`
	if err := database.SetStepRoundSelection(round.ID, &selected, db.RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}

	out, err := executeCmd("eval", "capture", run.ID)
	if err != nil {
		t.Fatalf("eval capture: %v\n%s", err, out)
	}
	if !strings.Contains(out, "captured 1 local review case") {
		t.Fatalf("capture output = %q", out)
	}

	out, err = executeCmd("eval", "sets")
	if err != nil {
		t.Fatalf("eval sets: %v\n%s", err, out)
	}
	if !strings.Contains(out, "1 with finding-level gold (true-positive 1, false-negative 0, false-positive 0)") || !strings.Contains(out, "0 unlabeled / pending") {
		t.Fatalf("sets output = %q, want finding-level gold, not park/pass", out)
	}
	if strings.Contains(out, "verdict") || strings.Contains(out, "park") || strings.Contains(out, ", pass ") {
		t.Fatalf("sets output still uses park/pass accuracy language: %q", out)
	}

	out, err = executeCmd("eval", "report")
	if err != nil {
		t.Fatalf("eval report: %v\n%s", err, out)
	}
	if !strings.Contains(out, "LOCAL-ONLY EVAL REPORT") || !strings.Contains(out, "no candidate replays recorded yet") {
		t.Fatalf("report output = %q", out)
	}
}

func TestEvalMissIngestLabelsFalseNegativeGold(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("NM_HOME", root)
	chdir(t, t.TempDir())

	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	gateDir := p.RepoDir("eval-repo")
	if err := git.InitBare(ctx, gateDir); err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(root, "source")
	mustCLIGit(t, ctx, root, "clone", gateDir, workDir)
	mustCLIGit(t, ctx, workDir, "config", "user.email", "eval@example.test")
	mustCLIGit(t, ctx, workDir, "config", "user.name", "Eval Test")
	if err := os.WriteFile(filepath.Join(workDir, "main.go"), []byte("package sample\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustCLIGit(t, ctx, workDir, "add", ".")
	mustCLIGit(t, ctx, workDir, "commit", "-m", "base")
	mustCLIGit(t, ctx, workDir, "branch", "-M", "main")
	mustCLIGit(t, ctx, workDir, "push", "origin", "main")
	baseSHA := mustCLIGit(t, ctx, workDir, "rev-parse", "HEAD")
	mustCLIGit(t, ctx, workDir, "checkout", "-b", "feature/eval")
	if err := os.WriteFile(filepath.Join(workDir, "main.go"), []byte("package sample\n\nfunc Changed() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustCLIGit(t, ctx, workDir, "add", "main.go")
	mustCLIGit(t, ctx, workDir, "commit", "-m", "change")
	mustCLIGit(t, ctx, workDir, "push", "origin", "feature/eval")
	headSHA := mustCLIGit(t, ctx, workDir, "rev-parse", "HEAD")

	repo, err := database.InsertRepoWithID("eval-repo", workDir, "https://example.test/org/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature/eval", headSHA, baseSHA)
	if err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[],"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"}`
	if _, err := database.InsertReviewStepRoundWithProvenance(step.ID, 1, "initial", &findings, nil, headSHA, headSHA, baseSHA, []byte("{}\n"), []byte("{}\n"), 50); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatus(step.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}

	out, err := executeCmd("eval", "miss", "ingest", run.ID, "--finding", `{"id":"silent-wrong-set","file":"pkg/compute.go","line":12,"severity":"error","description":"returns the wrong set for a valid input"}`)
	if err != nil {
		t.Fatalf("eval miss ingest: %v\n%s", err, out)
	}
	if !strings.Contains(out, "ingested 1 false-negative gold finding") {
		t.Fatalf("ingest output = %q", out)
	}

	out, err = executeCmd("eval", "sets")
	if err != nil {
		t.Fatalf("eval sets: %v\n%s", err, out)
	}
	if !strings.Contains(out, "1 with finding-level gold (true-positive 0, false-negative 1, false-positive 0)") {
		t.Fatalf("sets output = %q, want ingested false-negative gold", out)
	}

	out, err = executeCmd("eval", "capture", run.ID)
	if err != nil {
		t.Fatalf("recapture after ingest: %v\n%s", err, out)
	}
	out, err = executeCmd("eval", "sets")
	if err != nil {
		t.Fatalf("eval sets after recapture: %v\n%s", err, out)
	}
	if !strings.Contains(out, "1 with finding-level gold (true-positive 0, false-negative 1, false-positive 0)") {
		t.Fatalf("sets after recapture = %q, want ingested false-negative gold to persist", out)
	}

	out, err = executeCmd("eval", "miss", "ingest", run.ID, "--finding", `{"id":"silent-wrong-set","file":"pkg/compute.go","line":12,"severity":"error","description":"returns the wrong set for a valid input"}`)
	if err != nil {
		t.Fatalf("duplicate eval miss ingest: %v\n%s", err, out)
	}
	if !strings.Contains(out, "ingested 0 false-negative gold finding") {
		t.Fatalf("duplicate ingest output = %q", out)
	}
}

func mustCLIGit(t *testing.T, ctx context.Context, dir string, args ...string) string {
	t.Helper()
	out, err := git.Run(ctx, dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return out
}
