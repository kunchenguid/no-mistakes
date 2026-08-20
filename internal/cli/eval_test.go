package cli

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/eval"
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
	if !strings.Contains(out, "eval case sets") || !strings.Contains(out, "local-only") {
		t.Fatalf("output = %q, want the eval case sets dashboard with its local-only footer", out)
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

	findings := `{"findings":[{"id":"real-bug","severity":"error","file":"main.go","line":3,"description":"bug","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`
	fixture := setupEvalCLIFixture(t, ctx, root, findings)
	selected := `["real-bug"]`
	if err := fixture.db.SetStepRoundSelection(fixture.round.ID, &selected, db.RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}

	out, err := executeCmd("eval", "capture", fixture.run.ID)
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
	if !strings.Contains(out, "TP      1") || !strings.Contains(out, "FP      0") || !strings.Contains(out, "0 unlabeled / pending") {
		t.Fatalf("sets output = %q, want finding-level gold, not park/pass", out)
	}
	if !strings.Contains(out, "Diversified holdout") || !strings.Contains(out, "Self-score") || !strings.Contains(out, "1/1 true issues") {
		t.Fatalf("sets output = %q, want the diversified headline with its instant self-score", out)
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

	findings := `{"findings":[],"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"}`
	fixture := setupEvalCLIFixture(t, ctx, root, findings)
	if err := fixture.db.UpdateStepStatus(fixture.step.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}

	out, err := executeCmd("eval", "miss", "ingest", fixture.run.ID, "--finding", `{"id":"silent-wrong-set","file":"pkg/compute.go","line":12,"severity":"error","description":"returns the wrong set for a valid input"}`)
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
	if !strings.Contains(out, "FN      1") || !strings.Contains(out, "TP      0") {
		t.Fatalf("sets output = %q, want ingested false-negative gold", out)
	}

	out, err = executeCmd("eval", "capture", fixture.run.ID)
	if err != nil {
		t.Fatalf("recapture after ingest: %v\n%s", err, out)
	}
	out, err = executeCmd("eval", "sets")
	if err != nil {
		t.Fatalf("eval sets after recapture: %v\n%s", err, out)
	}
	if !strings.Contains(out, "FN      1") || !strings.Contains(out, "TP      0") {
		t.Fatalf("sets after recapture = %q, want ingested false-negative gold to persist", out)
	}

	out, err = executeCmd("eval", "miss", "ingest", fixture.run.ID, "--finding", `{"id":"silent-wrong-set","file":"pkg/compute.go","line":12,"severity":"error","description":"returns the wrong set for a valid input"}`)
	if err != nil {
		t.Fatalf("duplicate eval miss ingest: %v\n%s", err, out)
	}
	if !strings.Contains(out, "ingested 0 false-negative gold finding") {
		t.Fatalf("duplicate ingest output = %q", out)
	}
}

// Every read-or-converging eval subcommand must be idempotent at the CLI: the
// second identical invocation prints the same output and leaves the same
// state. Both halves are asserted - stdout equality alone let a command that
// created a new file under the app root pass as idempotent. (eval run is
// additive by design and is covered separately; eval miss ingest's duplicate
// no-op is covered above.)
func TestEvalCaptureSetsReportAndRelabelAreIdempotentAtTheCLI(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("NM_HOME", root)
	chdir(t, t.TempDir())

	findings := `{"findings":[{"id":"real-bug","severity":"error","file":"main.go","line":3,"description":"bug","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`
	fixture := setupEvalCLIFixture(t, ctx, root, findings)
	selected := `["real-bug"]`
	if err := fixture.db.SetStepRoundSelection(fixture.round.ID, &selected, db.RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.UpdateRunPRState(fixture.run.ID, "merged"); err != nil {
		t.Fatal(err)
	}

	for _, command := range [][]string{
		{"eval", "capture", fixture.run.ID},
		{"eval", "sets"},
		{"eval", "sets", "--refresh-diversified"},
		{"eval", "relabel", fixture.run.ID},
		{"eval", "relabel"},
		{"eval", "report"},
	} {
		first, err := executeCmd(command...)
		if err != nil {
			t.Fatalf("%v (first): %v\n%s", command, err, first)
		}
		treeBefore := nmHomeTree(t, root)
		second, err := executeCmd(command...)
		if err != nil {
			t.Fatalf("%v (second): %v\n%s", command, err, second)
		}
		if first != second {
			t.Fatalf("%v is not idempotent:\nfirst: %s\nsecond: %s", command, first, second)
		}
		if treeAfter := nmHomeTree(t, root); !slices.Equal(treeBefore, treeAfter) {
			t.Fatalf("%v changed the app root's shape:\nbefore: %v\nafter:  %v", command, treeBefore, treeAfter)
		}
	}
}

func TestEvalRunRendersProgressAndScoreDashboard(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("NM_HOME", root)
	chdir(t, t.TempDir())

	findings := `{"findings":[{"id":"real-bug","severity":"error","file":"main.go","line":3,"description":"bug","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`
	fixture := setupEvalCLIFixture(t, ctx, root, findings)
	selected := `["real-bug"]`
	if err := fixture.db.SetStepRoundSelection(fixture.round.ID, &selected, db.RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}
	installFakeCLIReviewAgent(t, root, findings)

	if out, err := executeCmd("eval", "capture", fixture.run.ID); err != nil {
		t.Fatalf("eval capture: %v\n%s", err, out)
	}
	setsBefore, err := executeCmd("eval", "sets")
	if err != nil {
		t.Fatal(err)
	}

	out, err := executeCmd("eval", "run", "--cases", "labeled", "--candidate", "claude+test", "--repeats", "2")
	if err != nil {
		t.Fatalf("eval run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "replaying 1 case(s) x 2 repeat(s) with claude+test on labeled") {
		t.Fatalf("run output = %q, want the replay plan header", out)
	}
	if !strings.Contains(out, "1/2") || !strings.Contains(out, "2/2") || !strings.Contains(out, "TP 1 · FN 0 · FP 0 · pending 0") {
		t.Fatalf("run output = %q, want one scored progress line per replay", out)
	}
	if !strings.Contains(out, " eval run ") || !strings.Contains(out, "Recall") || !strings.Contains(out, "2/2 true issues") {
		t.Fatalf("run output = %q, want the score summary dashboard aggregating both repeats", out)
	}
	if !strings.Contains(out, "local eval session") {
		t.Fatalf("run output = %q, want the trailing session line", out)
	}

	// Re-running the same eval run is additive (a fresh measurement session)
	// but must stay safe: the frozen corpus is untouched, and the identical
	// input lands in the same cohort so the report aggregates instead of
	// fragmenting into a new comparison group.
	if out, err = executeCmd("eval", "run", "--cases", "labeled", "--candidate", "claude+test", "--repeats", "2"); err != nil {
		t.Fatalf("second eval run: %v\n%s", err, out)
	}
	setsAfter, err := executeCmd("eval", "sets")
	if err != nil {
		t.Fatal(err)
	}
	if setsBefore != setsAfter {
		t.Fatalf("eval run mutated the case sets:\nbefore: %s\nafter: %s", setsBefore, setsAfter)
	}
	report, err := executeCmd("eval", "report")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(report, "cohort"); got != 1 {
		t.Fatalf("report = %q, want both identical runs aggregated into one cohort, got %d", report, got)
	}
	if !strings.Contains(report, "replays: 4") {
		t.Fatalf("report = %q, want all four replays counted in the single cohort", report)
	}
}

type evalCLIFixture struct {
	run   *db.Run
	round *db.StepRound
	step  *db.StepResult
	db    *db.DB
}

// setupEvalCLIFixture builds the minimal real gate, working clone, and
// recorded review round the eval CLI commands read from.
func setupEvalCLIFixture(t *testing.T, ctx context.Context, root, findings string) evalCLIFixture {
	t.Helper()
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

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
	round, err := database.InsertReviewStepRoundWithProvenance(step.ID, 1, "initial", &findings, nil, headSHA, headSHA, baseSHA, []byte("{}\n"), []byte("{}\n"), 50)
	if err != nil {
		t.Fatal(err)
	}
	return evalCLIFixture{run: run, round: round, step: step, db: database}
}

// installFakeCLIReviewAgent puts a scripted claude on PATH that replies with
// the given review findings, mirroring the fake agent the internal/eval
// replay tests use.
func installFakeCLIReviewAgent(t *testing.T, root, findingsJSON string) {
	t.Helper()
	fakeDir := t.TempDir()
	fake := filepath.Join(fakeDir, "claude")
	reply := `{"type":"assistant","message":{"usage":{"input_tokens":12,"output_tokens":3},"content":[{"type":"text","text":"review"}]}}
{"type":"result","subtype":"success","is_error":false,"structured_output":` + findingsJSON + `,"usage":{"input_tokens":12,"output_tokens":3}}
`
	var script string
	if runtime.GOOS == "windows" {
		fake += ".cmd"
		script = "@echo off\r\nmore >nul\r\necho " + strings.ReplaceAll(strings.TrimSpace(reply), "\n", "\r\necho ") + "\r\n"
	} else {
		script = "#!/bin/sh\n[ \"$NM_HOME\" = \"" + root + "\" ] && touch \"" + root + "/shared-home-used\"\ncat >/dev/null\ncat <<'EOF'\n" + reply + "EOF\n"
	}
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(fake)+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func mustCLIGit(t *testing.T, ctx context.Context, dir string, args ...string) string {
	t.Helper()
	out, err := git.Run(ctx, dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return out
}

// The eval-sets dashboard identifies a case's repository by name and lays the
// finding-level gold out as a confusion matrix. The repository name is
// resolved from the locally registered repositories, since a case stores only
// the fingerprint of its upstream URL.
func TestEvalSetsNamesTheRepositoryAndTablesTheConfusionMatrix(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("NM_HOME", root)
	chdir(t, t.TempDir())

	findings := `{"findings":[{"id":"real-bug","severity":"error","file":"main.go","line":3,"description":"bug","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`
	fixture := setupEvalCLIFixture(t, ctx, root, findings)
	selected := `["real-bug"]`
	if err := fixture.db.SetStepRoundSelection(fixture.round.ID, &selected, db.RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}
	if out, err := executeCmd("eval", "capture", fixture.run.ID); err != nil {
		t.Fatalf("eval capture: %v\n%s", err, out)
	}

	out, err := executeCmd("eval", "sets")
	if err != nil {
		t.Fatalf("eval sets: %v\n%s", err, out)
	}
	if !strings.Contains(out, "org/repo") {
		t.Fatalf("sets output = %q, want the repository name from its registered upstream URL", out)
	}
	for _, want := range []string{"Confusion matrix", "real issue", "not an issue", "review raised", "review missed", "TP", "FP", "FN", "TN"} {
		if !strings.Contains(out, want) {
			t.Fatalf("sets output = %q, want confusion-matrix table cell %q", out, want)
		}
	}
	if strings.Contains(out, "TN      0") {
		t.Fatalf("sets output = %q, want an uncounted true-negative cell, not a fabricated zero", out)
	}
	if !strings.Contains(out, "Diversified holdout") || !strings.Contains(out, "1/1 true issues") {
		t.Fatalf("sets output = %q, want the diversified headline and self-score preserved", out)
	}
}

// The composition table shows one kind of repository identity for every row
// and never runs past the dashboard box, however long the names are.
func TestEvalCompositionRepoColumnIsUniformAndFitsTheBox(t *testing.T) {
	rows := []eval.CompositionRow{
		{Repo: "a-very-long-organization-name/no-mistakes", Language: "go", Size: "large", Severity: "warning", FindingType: "warning/auto-fix", Cases: 2},
		{Repo: "group/very-long-subgroup/actual-repo", Language: "go", Size: "large", Severity: "error", FindingType: "error/ask-user", Cases: 1},
	}
	lines := compositionLines(rows)
	if len(lines) != len(rows) {
		t.Fatalf("compositionLines returned %d line(s), want %d", len(lines), len(rows))
	}
	for _, line := range lines {
		if width := lipgloss.Width(line); width > evalBoxWidth-4 {
			t.Fatalf("composition line %q is %d wide, want at most %d", line, width, evalBoxWidth-4)
		}
	}
	if strings.Contains(lines[0], "a-very-long-organization-name/no-mistakes") {
		t.Fatalf("line = %q, want the long identity shortened when it does not fit its column", lines[0])
	}
	if !strings.Contains(lines[0], "no-mistakes") || !strings.Contains(lines[1], "actual-repo") {
		t.Fatalf("lines = %q, want every repository's final path segment", lines)
	}
	if strings.Contains(lines[1], "very-long-subgroup") {
		t.Fatalf("line = %q, want a uniformly shortened repository name", lines[1])
	}
	if !strings.Contains(lines[0], "warning/auto-fix") || !strings.Contains(lines[1], "error/ask-user") {
		t.Fatalf("lines = %q, want the strata kept on every row", lines)
	}

	narrow := compositionLines([]eval.CompositionRow{{Repo: "owner/name", Language: "go", Size: "tiny", Severity: "none", FindingType: "none", Cases: 1}})
	if !strings.Contains(narrow[0], "owner/name") {
		t.Fatalf("line = %q, want the full owner/name identity when it fits", narrow[0])
	}
	if got := compositionLines(nil); len(got) != 0 {
		t.Fatalf("compositionLines(nil) = %q, want no lines", got)
	}
}

// The composition table must fit the dashboard box on BOTH of its variable
// axes. The repository column is only one of them: the strata are the other,
// and a finding type carrying a non-canonical severity or action can push the
// fixed strata past the room the box has, at which point clamping the
// repository column to its minimum is not enough and the box renderer silently
// cuts the finding type off the end of the row.
func TestEvalCompositionFitsTheBoxWhenTheStrataAreOversized(t *testing.T) {
	rows := []eval.CompositionRow{
		{Repo: "kunchenguid/no-mistakes", Language: "javascript", Size: "medium", Severity: "warning", FindingType: "blocking-correctness-defect/requires-human-review", Cases: 3},
		{Repo: "another-organization/service", Language: "typescript", Size: "large", Severity: "error", FindingType: "error/ask-user", Cases: 1},
	}
	lines := compositionLines(rows)
	if len(lines) != len(rows) {
		t.Fatalf("compositionLines returned %d line(s), want %d", len(lines), len(rows))
	}
	for _, line := range lines {
		if width := lipgloss.Width(line); width > evalBoxWidth-4 {
			t.Fatalf("composition line %q is %d wide, want at most %d", line, width, evalBoxWidth-4)
		}
	}

	// The rendered box is the surface the reader sees: nothing may be cut off
	// there either, and every row must still start with its case count and
	// repository identity.
	box := renderTitledBox(" eval case sets ", evalBoxWidth, lines)
	for _, line := range strings.Split(box, "\n") {
		if width := lipgloss.Width(line); width != evalBoxWidth {
			t.Fatalf("rendered box line %q is %d wide, want exactly %d", line, width, evalBoxWidth)
		}
	}
	if !strings.Contains(lines[0], "no-mista") || !strings.Contains(lines[1], "service") {
		t.Fatalf("lines = %q, want every row to keep a repository identity", lines)
	}
	if !strings.Contains(lines[0], "javascript") || !strings.Contains(lines[1], "typescript") {
		t.Fatalf("lines = %q, want the leading strata preserved when the trailing ones are shortened", lines)
	}
}

func TestEvalCompositionFitsTheBoxWhenCaseCountsExpand(t *testing.T) {
	rows := []eval.CompositionRow{
		{Repo: "owner/abcdefghijklmnopqrstuvwxyz", Language: "go", Size: "large", Severity: "warning", FindingType: "warning/auto-fix", Cases: 9999},
		{Repo: "owner/zyxwvutsrqponmlkjihgfedcba", Language: "go", Size: "large", Severity: "warning", FindingType: "warning/auto-fix", Cases: 10000},
	}
	box := renderTitledBox(" eval case sets ", evalBoxWidth, compositionLines(rows))
	for _, line := range strings.Split(box, "\n") {
		if width := lipgloss.Width(line); width != evalBoxWidth {
			t.Fatalf("rendered box line %q is %d wide, want exactly %d", line, width, evalBoxWidth)
		}
	}
	if got := strings.Count(box, "warning/auto-fix"); got != len(rows) {
		t.Fatalf("rendered box contains %d complete finding types, want %d:\n%s", got, len(rows), box)
	}
}

// nmHomeTree lists every path under root, relative and sorted, so a test can
// assert that a command left the app root's shape untouched.
func nmHomeTree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if rel != "." {
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(out)
	return out
}

// The eval dashboards are display surfaces over the local corpus: they resolve
// repository names from the pipeline database only if one already exists, and
// must never bring that database into being. db.Open creates the file and runs
// every migration, so a display-only lookup routed through it turned `eval
// sets`, `eval report`, and `eval run` into commands that initialize pipeline
// state on a machine that has none.
func TestEvalDisplayCommandsDoNotCreateThePipelineDatabase(t *testing.T) {
	tests := []struct {
		command []string
		// eval run legitimately refuses an empty case set; the filesystem
		// effect under test is the same either way.
		allowError bool
	}{
		{command: []string{"eval", "sets"}},
		{command: []string{"eval", "report"}},
		{command: []string{"eval", "run", "--cases", "labeled", "--candidate", "claude+test", "--repeats", "1"}, allowError: true},
	}
	for _, tt := range tests {
		command := tt.command
		t.Run(strings.Join(command, "-"), func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("NM_HOME", root)
			chdir(t, t.TempDir())

			out, err := executeCmd(command...)
			if err != nil && !tt.allowError {
				t.Fatalf("%v: %v\n%s", command, err, out)
			}

			p, err := paths.New()
			if err != nil {
				t.Fatal(err)
			}
			for _, suffix := range []string{"", "-wal", "-shm"} {
				if _, statErr := os.Stat(p.DB() + suffix); !os.IsNotExist(statErr) {
					t.Fatalf("%v created pipeline database file %q (stat err %v); a display-only repository-name lookup must not create or migrate pipeline state",
						command, p.DB()+suffix, statErr)
				}
			}
		})
	}
}

// A pre-existing pipeline database still resolves repository names: opening it
// read-only removes the side effect, not the feature.
func TestEvalSetsStillNamesRepositoriesFromAnExistingDatabase(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("NM_HOME", root)
	chdir(t, t.TempDir())

	findings := `{"findings":[{"id":"real-bug","severity":"error","file":"main.go","line":3,"description":"bug","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`
	fixture := setupEvalCLIFixture(t, ctx, root, findings)
	selected := `["real-bug"]`
	if err := fixture.db.SetStepRoundSelection(fixture.round.ID, &selected, db.RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}
	if out, err := executeCmd("eval", "capture", fixture.run.ID); err != nil {
		t.Fatalf("eval capture: %v\n%s", err, out)
	}

	out, err := executeCmd("eval", "sets")
	if err != nil {
		t.Fatalf("eval sets: %v\n%s", err, out)
	}
	if !strings.Contains(out, "org/repo") {
		t.Fatalf("sets output = %q, want the repository name resolved from the existing pipeline database", out)
	}
}
