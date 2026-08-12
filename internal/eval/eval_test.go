package eval

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCaptureCreatesPortableReviewCaseWithoutRecordingRemoteURL(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, repo, reviewRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cases, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 {
		t.Fatalf("captured cases = %d, want 1", len(cases))
	}
	captured := cases[0]
	if captured.SourceRunID != run.ID || captured.SourceRoundID != reviewRound.ID {
		t.Fatalf("capture provenance = %#v, want run %q round %q", captured, run.ID, reviewRound.ID)
	}
	if !captured.Labels.Verdict.Known || !captured.Labels.Verdict.ShouldPark {
		t.Fatalf("verdict label = %#v, want recorded user-fix park label", captured.Labels.Verdict)
	}
	restored := filepath.Join(t.TempDir(), "restore.git")
	if err := git.InitBare(ctx, restored); err != nil {
		t.Fatal(err)
	}
	if err := restoreCaseObjects(ctx, store.poolDir(captured.RepoFingerprint), restored, captured.ID); err != nil {
		t.Fatalf("case objects are not restorable: %v", err)
	}
	if got := mustGit(t, ctx, restored, "rev-parse", captured.ReviewedHeadSHA+"^{commit}"); got != captured.ReviewedHeadSHA {
		t.Fatalf("restored reviewed commit = %q, want %q", got, captured.ReviewedHeadSHA)
	}

	manifestBytes, err := os.ReadFile(filepath.Join(captured.Dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(manifestBytes), repo.UpstreamURL) || strings.Contains(string(manifestBytes), "secret-token") {
		t.Fatalf("manifest leaked source remote credential: %s", manifestBytes)
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.TrustedConfigSHA == "" || manifest.ReviewedHeadSHA == "" {
		t.Fatalf("manifest did not pin replay inputs: %#v", manifest)
	}

	listed, err := store.ListCases("all")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != captured.ID {
		t.Fatalf("registry cases = %#v, want captured case", listed)
	}
}

func TestCaptureRejectsReviewRoundBeforeGateDecision(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, reviewRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	if err := sourceDB.SetStepRoundSelection(reviewRound.ID, nil, ""); err != nil {
		t.Fatal(err)
	}
	steps, err := sourceDB.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := sourceDB.UpdateStepStatus(steps[0].ID, types.StepStatusAwaitingApproval); err != nil {
		t.Fatal(err)
	}

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := Capture(ctx, store, p, sourceDB, run.ID); err == nil || !strings.Contains(err.Error(), "no recorded gate decision") {
		t.Fatalf("capture error = %v, want missing gate decision", err)
	}
	cases, err := store.ListCases("all")
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 0 {
		t.Fatalf("premature capture registered %d cases", len(cases))
	}
}

func TestCaptureExplainsMissingConfigurationProvenance(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	steps, err := sourceDB.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	clean := `{"findings":[],"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"}`
	if _, err := sourceDB.InsertReviewStepRound(steps[0].ID, 2, "legacy", &clean, nil, run.HeadSHA, 10); err != nil {
		t.Fatal(err)
	}

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = Capture(ctx, store, p, sourceDB, run.ID)
	if !errors.Is(err, ErrNoCapturableReview) {
		t.Fatalf("capture error = %v, want ErrNoCapturableReview", err)
	}
	if !strings.Contains(err.Error(), "eval.capture_provenance was off") {
		t.Fatalf("capture error = %q, want the disabled setting named as the likely cause", err)
	}
}

func TestCapturePinsConfigurationFromSourceReview(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, reviewRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	gateDir := p.RepoDir(run.RepoID)
	workDir := filepath.Join(p.Root(), "advance-main")
	mustGit(t, ctx, p.Root(), "clone", gateDir, workDir)
	mustGit(t, ctx, workDir, "config", "user.email", "eval@example.test")
	mustGit(t, ctx, workDir, "config", "user.name", "Eval Test")
	mustGit(t, ctx, workDir, "checkout", "main")
	if err := os.WriteFile(filepath.Join(workDir, ".no-mistakes.yaml"), []byte("ignore_patterns: ['advanced-only']\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, ctx, workDir, "add", ".no-mistakes.yaml")
	t.Setenv("GIT_AUTHOR_DATE", time.Unix(reviewRound.CreatedAt+60, 0).Format(time.RFC3339))
	t.Setenv("GIT_COMMITTER_DATE", time.Unix(reviewRound.CreatedAt+60, 0).Format(time.RFC3339))
	mustGit(t, ctx, workDir, "commit", "-m", "advance trusted config")
	mustGit(t, ctx, workDir, "push", "origin", "main")

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cases, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 || cases[0].TrustedConfigSHA != run.BaseSHA {
		t.Fatalf("trusted config pin = %#v, want source-review SHA %s", cases, run.BaseSHA)
	}
}

func TestCapturePreservesFixRoundStartingHead(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, repo, firstRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, "main.go"), []byte("package sample\n\nfunc Fixed() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, ctx, repo.WorkingPath, "add", "main.go")
	mustGit(t, ctx, repo.WorkingPath, "commit", "-m", "fix review finding")
	mustGit(t, ctx, repo.WorkingPath, "push", "origin", "feature/eval")
	fixedSHA := mustGit(t, ctx, repo.WorkingPath, "rev-parse", "HEAD")
	steps, err := sourceDB.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	clean := `{"findings":[],"risk_level":"low","risk_rationale":"fixed","risk_scope":"source-or-external"}`
	secondRound, err := sourceDB.InsertReviewStepRoundWithProvenance(steps[0].ID, 2, "auto_fix", &clean, nil, fixedSHA, stringValue(firstRound.ReviewedHeadSHA), stringValue(firstRound.TrustedConfigSHA), firstRound.GlobalConfigYAML, firstRound.RepoConfigYAML, 25)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cases, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 2 || cases[1].SourceRoundID != secondRound.ID || cases[1].StartingHeadSHA != stringValue(firstRound.ReviewedHeadSHA) || cases[1].ReviewedHeadSHA != fixedSHA {
		t.Fatalf("captured fix-round provenance = %#v", cases)
	}
}

func TestReplayRestoresCaseIntoAnIsolatedWorktree(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
	defer sourceDB.Close()

	fakeDir := t.TempDir()
	fake := filepath.Join(fakeDir, "claude")
	const reply = `{"type":"assistant","message":{"usage":{"input_tokens":12,"output_tokens":3},"content":[{"type":"text","text":"clean"}]}}
{"type":"result","subtype":"success","is_error":false,"structured_output":{"findings":[],"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"},"usage":{"input_tokens":12,"output_tokens":3}}
`
	var script string
	if runtime.GOOS == "windows" {
		fake += ".cmd"
		script = "@echo off\r\nmore >nul\r\necho " + strings.ReplaceAll(strings.TrimSpace(reply), "\n", "\r\necho ") + "\r\n"
	} else {
		script = "#!/bin/sh\n[ \"$NM_HOME\" = \"" + p.Root() + "\" ] && touch \"" + p.Root() + "/shared-home-used\"\ncat >/dev/null\ncat <<'EOF'\n" + reply + "EOF\n"
	}
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(fake)+string(os.PathListSeparator)+os.Getenv("PATH"))
	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := Capture(ctx, store, p, sourceDB, run.ID); err != nil {
		t.Fatal(err)
	}

	session, evaluations, err := Replay(ctx, store, ReplayOptions{Set: "all", Candidate: Candidate{Agent: types.AgentClaude, Model: "test"}, Repeats: 1})
	if err != nil {
		t.Fatal(err)
	}
	if session.ID == "" || len(evaluations) != 1 {
		t.Fatalf("replay = session %#v evaluations %#v", session, evaluations)
	}
	got := evaluations[0]
	if got.Status != "completed" || got.CandidateParked {
		t.Fatalf("replay outcome = %#v, want completed non-parked review", got)
	}
	if !got.TokensReported || got.FreshInputTokens != 12 || got.OutputTokens != 3 {
		t.Fatalf("replay metrics = %#v", got)
	}
	if strings.Contains(got.Error, p.Root()) {
		t.Fatalf("replay error leaked production root: %q", got.Error)
	}
	if _, err := os.Stat(filepath.Join(p.Root(), "shared-home-used")); !os.IsNotExist(err) {
		t.Fatalf("candidate used production NM_HOME: %v", err)
	}
	var reservations int
	if err := store.db.QueryRow(`SELECT count(*) FROM replay_case_reservations WHERE session_id = ?`, session.ID).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if reservations != 0 {
		t.Fatalf("completed replay retained %d case reservations", reservations)
	}
}

func TestBaselineForRoundIncludesOnlyCompleteReviewInvocationMetrics(t *testing.T) {
	input, output, cache := 100, 20, 30
	invocations := []db.AgentInvocation{
		{StepName: string(types.StepReview), Round: 2, Purpose: "review-fix", DurationMS: 900, DeltaInputTokens: &input, DeltaOutputTokens: &output, DeltaCacheReadTokens: &cache},
		{StepName: string(types.StepReview), Round: 2, Purpose: "review", DurationMS: 100, DeltaInputTokens: &input, DeltaOutputTokens: &output, DeltaCacheReadTokens: &cache},
	}
	baseline := baselineForRound(invocations, 2)
	if baseline.DurationMS != 100 || !baseline.TokensReported || baseline.InputTokens != 100 || baseline.OutputTokens != 20 || baseline.CacheReadTokens != 30 || baseline.FreshInputTokens != 70 {
		t.Fatalf("review baseline = %#v", baseline)
	}

	invocations = append(invocations, db.AgentInvocation{StepName: string(types.StepReview), Round: 2, Purpose: "review", DurationMS: 50})
	baseline = baselineForRound(invocations, 2)
	if baseline.DurationMS != 150 || baseline.TokensReported || baseline.InputTokens != 0 || baseline.OutputTokens != 0 || baseline.CacheReadTokens != 0 || baseline.FreshInputTokens != 0 {
		t.Fatalf("incomplete review baseline = %#v", baseline)
	}
}

func TestReportQueuesUnexpectedParksInsteadOfScoringThemWrong(t *testing.T) {
	summary := SummarizeEvaluations([]Evaluation{
		{CaseID: "must-park", Candidate: "claude+test", Status: "completed", ExpectedPark: boolPtr(true), CandidateParked: true},
		{CaseID: "must-park", Candidate: "claude+test", Status: "completed", ExpectedPark: boolPtr(true), CandidateParked: false},
		{CaseID: "human-passed", Candidate: "claude+test", Status: "completed", ExpectedPark: boolPtr(false), CandidateParked: true},
		{CaseID: "human-passed", Candidate: "claude+test", Status: "completed", ExpectedPark: boolPtr(false), CandidateParked: false},
	})

	if summary.Conclusive != 3 || summary.Correct != 2 || summary.UnexpectedParks != 1 {
		t.Fatalf("summary = %#v, want 3 conclusive, 2 correct, 1 queued unexpected park", summary)
	}
	if got := summary.ConfirmedAccuracy(); got != 2.0/3.0 {
		t.Fatalf("confirmed accuracy = %v, want %v", got, 2.0/3.0)
	}
	if got := summary.LowerBoundAccuracy(); got != 0.5 {
		t.Fatalf("lower-bound accuracy = %v, want 0.5", got)
	}
}

func TestFailedLabeledReplayCountsAgainstAccuracyAndFrontier(t *testing.T) {
	summary := SummarizeEvaluations([]Evaluation{
		{Candidate: "claude+test", Status: "completed", ExpectedPark: boolPtr(true), CandidateParked: true},
		{Candidate: "claude+test", Status: "failed", ExpectedPark: boolPtr(true)},
	})
	if summary.Labeled != 2 || summary.Conclusive != 2 || summary.Correct != 1 || summary.Misses != 1 || summary.ConfirmedAccuracy() != 0.5 {
		t.Fatalf("summary = %#v, want failed labeled replay scored conservatively", summary)
	}
	cost := 10.0
	reports := []CandidateReport{{Cohort: "same", Summary: summary, AverageTokens: &cost}}
	markFrontier(reports)
	if reports[0].OnFrontier {
		t.Fatal("candidate with failed replay was marked frontier-eligible")
	}
}

func TestPersistEvaluationQueuesEveryUnexpectedCandidateFinding(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	caseDir := store.caseDir("candidate-findings")
	if err := os.MkdirAll(caseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	labels := Labels{Version: 1, Verdict: VerdictLabel{Known: true}}
	if err := writeJSON(filepath.Join(caseDir, "labels.json"), labels); err != nil {
		t.Fatal(err)
	}
	c := Case{Manifest: Manifest{ID: "candidate-findings", SourceRunID: "run", SourceRoundID: "round"}, Labels: labels, Dir: caseDir}
	if err := store.registerCase(c); err != nil {
		t.Fatal(err)
	}
	if err := store.persistEvaluation(c, Evaluation{
		ID:              "evaluation",
		SessionID:       "session",
		CaseID:          c.ID,
		Candidate:       "claude+test",
		Repeat:          1,
		Status:          "completed",
		ExpectedPark:    boolPtr(false),
		CandidateParked: true,
		FindingCount:    3,
	}); err != nil {
		t.Fatal(err)
	}

	if err := readJSON(filepath.Join(caseDir, "labels.json"), &labels); err != nil {
		t.Fatal(err)
	}
	if labels.QueuedCandidateFindings != 3 {
		t.Fatalf("queued candidate findings = %d, want 3", labels.QueuedCandidateFindings)
	}
}

func TestBaselineForRoundDerivesFreshTokensFromPerRoundDeltas(t *testing.T) {
	deltaInput, deltaOutput, deltaCache := 100, 20, 40
	cumulativeFresh := 900
	got := baselineForRound([]db.AgentInvocation{
		{
			StepName:             string(types.StepReview),
			Round:                2,
			Purpose:              "review",
			DurationMS:           50,
			FreshInputTokens:     &cumulativeFresh,
			DeltaInputTokens:     &deltaInput,
			DeltaOutputTokens:    &deltaOutput,
			DeltaCacheReadTokens: &deltaCache,
		},
	}, 2)

	if !got.TokensReported || got.InputTokens != 100 || got.OutputTokens != 20 || got.CacheReadTokens != 40 || got.FreshInputTokens != 60 {
		t.Fatalf("baseline = %#v, want per-round token metrics", got)
	}
}

func TestConfidenceIntervalRequiresMultipleIndependentCases(t *testing.T) {
	rows := []Evaluation{{CaseID: "only", Candidate: "claude+test", Status: "completed", ExpectedPark: boolPtr(true), CandidateParked: true}}
	if got := confidenceInterval("claude+test", rows); got != nil {
		t.Fatalf("single-case confidence interval = %#v, want unavailable", got)
	}
}

func TestConfidenceIntervalRepresentsUniformSampleUncertainty(t *testing.T) {
	rows := []Evaluation{
		{CaseID: "one", Status: "completed", ExpectedPark: boolPtr(true), CandidateParked: true},
		{CaseID: "two", Status: "completed", ExpectedPark: boolPtr(true), CandidateParked: true},
	}
	got := confidenceInterval("claude+test", rows)
	if got == nil || got.Lower <= 0 || got.Lower >= 1 || got.Upper < 0.999 {
		t.Fatalf("uniform-success confidence interval = %#v, want finite-sample uncertainty ending at 1", got)
	}
}

func TestConfidenceIntervalIncludesFailedLabeledReplays(t *testing.T) {
	rows := []Evaluation{
		{CaseID: "passed", Status: "completed", ExpectedPark: boolPtr(true), CandidateParked: true},
		{CaseID: "failed", Status: "failed", ExpectedPark: boolPtr(true)},
	}
	got := confidenceInterval("claude+test", rows)
	if got == nil || got.Cases != 2 || got.Lower >= 0.5 || got.Upper <= 0.5 {
		t.Fatalf("confidence interval with scored failure = %#v, want interval around 50%% over two cases", got)
	}
}

func TestRenderReportNamesWilsonScoreInterval(t *testing.T) {
	output := RenderReport([]CandidateReport{
		{
			Cohort:     "cohort",
			Summary:    EvaluationSummary{Candidate: "claude+test", Total: 2, Labeled: 2, Conclusive: 2, Correct: 2},
			Confidence: &Interval{Lower: 0.34, Upper: 1, Cases: 2},
		},
	})
	if !strings.Contains(output, "95% Wilson score CI: 34.0%-100.0% over 2 case(s)") {
		t.Fatalf("report confidence interval = %q", output)
	}
}

func TestAverageTokensRequiresCompleteReplayCoverage(t *testing.T) {
	rows := []Evaluation{
		{TokensReported: true, FreshInputTokens: 10, OutputTokens: 2},
		{TokensReported: false},
	}
	if cost, ok := averageTokens(rows); ok {
		t.Fatalf("partial token cost = %v, want unknown", cost)
	}
}

func TestFrontierDoesNotCompareDifferentCohorts(t *testing.T) {
	cheap := 10.0
	expensive := 100.0
	reports := []CandidateReport{
		{Cohort: "a", Summary: EvaluationSummary{Labeled: 1, Correct: 1}, AverageTokens: &expensive},
		{Cohort: "b", Summary: EvaluationSummary{Labeled: 1, Correct: 1}, AverageTokens: &cheap},
	}
	markFrontier(reports)
	if !reports[0].OnFrontier || !reports[1].OnFrontier {
		t.Fatalf("different cohorts dominated each other: %#v", reports)
	}
}

func TestParseCandidateRequiresAgentAndModel(t *testing.T) {
	for _, input := range []string{"claude", "+model", "claude+", "claude+model+extra", "cursor+model", "acp:custom+model"} {
		if _, err := ParseCandidate(input); err == nil {
			t.Errorf("ParseCandidate(%q) succeeded, want error", input)
		}
	}
	candidate, err := ParseCandidate("codex+gpt-5.4")
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Agent != types.AgentCodex || candidate.Model != "gpt-5.4" {
		t.Fatalf("candidate = %#v", candidate)
	}
}

func setupCapturedRun(t *testing.T, ctx context.Context) (*paths.Paths, *db.DB, *db.Run, *db.Repo, *db.StepRound) {
	t.Helper()
	return setupCapturedRunWithHistory(t, ctx, 0)
}

// setupCapturedRunWithHistory builds the fixture run. padCommits adds that many
// commits of incompressible content to the default branch BEFORE the reviewed
// branch is cut, so the padding is real ancestry of every commit a case pins -
// which is what makes duplicated history measurable.
func setupCapturedRunWithHistory(t *testing.T, ctx context.Context, padCommits int) (*paths.Paths, *db.DB, *db.Run, *db.Repo, *db.StepRound) {
	t.Helper()
	root := t.TempDir()
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}

	gateDir := p.RepoDir("eval-repo")
	if err := git.InitBare(ctx, gateDir); err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(root, "source")
	mustGit(t, ctx, root, "clone", gateDir, workDir)
	mustGit(t, ctx, workDir, "config", "user.email", "eval@example.test")
	mustGit(t, ctx, workDir, "config", "user.name", "Eval Test")
	if err := os.WriteFile(filepath.Join(workDir, ".no-mistakes.yaml"), []byte("review:\n  path_instructions:\n    - path: '*.go'\n      instructions: review error paths\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "main.go"), []byte("package sample\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, ctx, workDir, "add", ".")
	mustGit(t, ctx, workDir, "commit", "-m", "base")
	mustGit(t, ctx, workDir, "branch", "-M", "main")
	padHistory(t, ctx, workDir, padCommits)
	mustGit(t, ctx, workDir, "push", "origin", "main")
	baseSHA := mustGit(t, ctx, workDir, "rev-parse", "HEAD")
	mustGit(t, ctx, workDir, "checkout", "-b", "feature/eval")
	if err := os.WriteFile(filepath.Join(workDir, "main.go"), []byte("package sample\n\nfunc Changed() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, ctx, workDir, "add", "main.go")
	mustGit(t, ctx, workDir, "commit", "-m", "change")
	mustGit(t, ctx, workDir, "push", "origin", "feature/eval")
	headSHA := mustGit(t, ctx, workDir, "rev-parse", "HEAD")

	repo, err := database.InsertRepoWithID("eval-repo", workDir, "https://secret-token@example.test/org/repo", "main")
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
	repoConfigYAML, err := os.ReadFile(filepath.Join(workDir, ".no-mistakes.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	reviewRound, err := database.InsertReviewStepRoundWithProvenance(step.ID, 1, "initial", &findings, nil, headSHA, headSHA, baseSHA, []byte("{}\n"), repoConfigYAML, 50)
	if err != nil {
		t.Fatal(err)
	}
	selected := `["real-bug"]`
	if err := database.SetStepRoundSelection(reviewRound.ID, &selected, db.RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}
	return p, database, run, repo, reviewRound
}

func mustGit(t *testing.T, ctx context.Context, dir string, args ...string) string {
	t.Helper()
	out, err := git.Run(ctx, dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return out
}

func boolPtr(v bool) *bool { return &v }
