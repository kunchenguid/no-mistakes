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

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/config"
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
	if !captured.Labels.HasGold() || len(captured.Labels.Findings) != 1 {
		t.Fatalf("gold labels = %#v, want one recorded user-fix finding", captured.Labels)
	}
	gold := captured.Labels.Findings[0]
	if gold.Kind != GoldTruePositive || gold.Source != goldSourceUserFix || gold.ID != "real-bug" || gold.Description != "bug" {
		t.Fatalf("true-positive gold = %#v, want recorded user-fix for real-bug", gold)
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
	if got.Status != "completed" || got.GoldCount != 1 || got.TruePositive != 0 || got.FalseNegative != 1 || got.Pending != 0 {
		t.Fatalf("replay outcome = %#v, want a completed miss of the true-positive gold", got)
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

// TestReplayPinsCandidateModelAndEffortOnTheHarness is the end-to-end half of
// the unified abstraction on the eval side: a candidate's model and effort reach
// the harness through the same agentcfg mapping the pipeline uses, so an
// effort-aware comparison actually runs at the effort it reports.
func TestReplayPinsCandidateModelAndEffortOnTheHarness(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
	defer sourceDB.Close()

	fakeDir := t.TempDir()
	tuningArgsPath := filepath.Join(fakeDir, "tuning-args.txt")
	fake := filepath.Join(fakeDir, "claude")
	const reply = `{"type":"result","subtype":"success","is_error":false,"structured_output":{"findings":[],"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"},"usage":{"input_tokens":1,"output_tokens":1}}
`
	var script string
	if runtime.GOOS == "windows" {
		fake += ".cmd"
		// Do not expand %* through cmd.exe: the later JSON-schema argument
		// contains quoting and metacharacters that make echoing the complete
		// command line unreliable. The profile args are deliberately first, so
		// record only those four scalar arguments.
		script = "@echo off\r\n>\"" + tuningArgsPath + "\" echo %~1 %~2\r\n>>\"" + tuningArgsPath + "\" echo %~3 %~4\r\nmore >nul\r\necho " + strings.ReplaceAll(strings.TrimSpace(reply), "\n", "\r\necho ") + "\r\n"
	} else {
		script = "#!/bin/sh\nprintf '%s %s\\n%s %s\\n' \"$1\" \"$2\" \"$3\" \"$4\" > \"" + tuningArgsPath + "\"\ncat >/dev/null\ncat <<'EOF'\n" + reply + "EOF\n"
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

	candidate := Candidate{Agent: types.AgentClaude, Model: "test", Effort: agentcfg.EffortHigh}
	session, evaluations, err := Replay(ctx, store, ReplayOptions{Set: "all", Candidate: candidate, Repeats: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(evaluations) != 1 || evaluations[0].Status != "completed" {
		t.Fatalf("replay = %#v", evaluations)
	}
	if session.Candidate != "claude,model=test,effort=high" {
		t.Fatalf("session candidate = %q, want the canonical spelling", session.Candidate)
	}
	tuningArgs, err := os.ReadFile(tuningArgsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--model test", "--effort high"} {
		if !strings.Contains(string(tuningArgs), want) {
			t.Fatalf("candidate harness tuning args %q do not carry %q", tuningArgs, want)
		}
	}
}

// TestCaptureStripsEveryHarnessPinFromThePinnedConfig keeps a replay from
// inheriting the capturing machine's own model or effort: the candidate is the
// only thing that may decide what the harness runs as.
func TestCaptureStripsEveryHarnessPinFromThePinnedConfig(t *testing.T) {
	pinned := []byte("agent: codex\nagent_args_override:\n  codex:\n    - -m\n    - gpt-5.4\nagent_config:\n  codex:\n    model: gpt-5.4\n    effort: high\nlog_level: warn\n")
	neutral, err := agentNeutralGlobalConfig(pinned)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"agent:", "agent_args_override", "agent_config"} {
		if strings.Contains(string(neutral), key) {
			t.Errorf("pinned config still carries %q: %s", key, neutral)
		}
	}
	if !strings.Contains(string(neutral), "log_level") {
		t.Errorf("pinned config lost unrelated settings: %s", neutral)
	}
	cfg, err := config.LoadGlobalFromBytes(neutral)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AgentConfig != nil {
		t.Fatalf("neutral config resolves an agent profile: %#v", cfg.AgentConfig)
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

func TestScoreCandidateMatchesSameFindingID(t *testing.T) {
	labels := Labels{Findings: []FindingGold{{
		ID:          "error-handling",
		Kind:        GoldTruePositive,
		File:        "old.go",
		Description: "drops an HTTP error",
	}}}
	candidate := `{"findings":[{"id":"error-handling","file":"new.go","description":"drops a database error"}]}`

	score := ScoreCandidate(labels, candidate)
	if score.TruePositive != 1 || score.FalseNegative != 0 || score.Pending != 0 {
		t.Fatalf("score = %#v, want same finding ID matched", score)
	}
}

func TestScoreCandidateMatchesNormalizedFileAndDescription(t *testing.T) {
	labels := Labels{Findings: []FindingGold{{ID: "review-1", Kind: GoldTruePositive, File: " internal/eval/score.go ", Description: "Drops   an HTTP Error"}}}
	candidate := `{"findings":[{"id":"different","file":"internal/eval/score.go","description":"drops an http error"}]}`

	score := ScoreCandidate(labels, candidate)
	if score.TruePositive != 1 || score.FalseNegative != 0 || score.Pending != 0 {
		t.Fatalf("score = %#v, want conservative file-and-description match", score)
	}
}

func TestScoreCandidateDoesNotMatchFindingsWithoutFiles(t *testing.T) {
	labels := Labels{Findings: []FindingGold{{ID: "gold", Kind: GoldTruePositive, Description: "drops an HTTP error"}}}
	candidate := `{"findings":[{"id":"candidate","description":"drops an http error"}]}`

	score := ScoreCandidate(labels, candidate)
	if score.TruePositive != 0 || score.FalseNegative != 1 || score.Pending != 1 {
		t.Fatalf("score = %#v, want file-less findings left unmatched", score)
	}
}

func TestSummarizeEvaluationsScoresFindingGoldAndLeavesUnmatchedPending(t *testing.T) {
	summary := SummarizeEvaluations([]Evaluation{
		{CaseID: "fix-gold", Candidate: "claude+test", Status: "completed", HasFindingGold: true, GoldCount: 1, TruePositive: 1},
		{CaseID: "fix-gold", Candidate: "claude+test", Status: "completed", HasFindingGold: true, GoldCount: 1, FalseNegative: 1},
		{CaseID: "approve-unlabeled", Candidate: "claude+test", Status: "completed", Pending: 2},
		{CaseID: "approve-unlabeled", Candidate: "claude+test", Status: "completed"},
	})

	if summary.Labeled != 2 || summary.TruePositive != 1 || summary.FalseNegative != 1 || summary.FalsePositive != 0 || summary.Pending != 2 {
		t.Fatalf("summary = %#v, want TP/FN gold plus queued unmatched findings", summary)
	}
	if got := summary.Recall(); got != 0.5 {
		t.Fatalf("recall = %v, want 0.5", got)
	}
}

func TestSummarizeEvaluationsKeepsExplicitInvalidOnlyScoresLabeled(t *testing.T) {
	summary := SummarizeEvaluations([]Evaluation{{
		Candidate:      "claude+test",
		Status:         "completed",
		HasFindingGold: true,
		FalsePositive:  1,
	}})

	if summary.Labeled != 1 || summary.FalsePositive != 1 {
		t.Fatalf("summary = %#v, want explicit-invalid-only evaluation retained", summary)
	}
}

func TestFailedLabeledReplayCountsAsFalseNegativeAndBlocksFrontier(t *testing.T) {
	summary := SummarizeEvaluations([]Evaluation{
		{Candidate: "claude+test", Status: "completed", GoldCount: 1, TruePositive: 1},
		{Candidate: "claude+test", Status: "failed", GoldCount: 1, FalseNegative: 1},
	})
	if summary.Labeled != 2 || summary.TruePositive != 1 || summary.FalseNegative != 1 || summary.Recall() != 0.5 {
		t.Fatalf("summary = %#v, want failed labeled replay counted as a false-negative", summary)
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
	labels := Labels{Version: labelsVersion}
	if err := writeJSON(filepath.Join(caseDir, "labels.json"), labels); err != nil {
		t.Fatal(err)
	}
	c := Case{Manifest: Manifest{ID: "candidate-findings", SourceRunID: "run", SourceRoundID: "round"}, Labels: labels, Dir: caseDir}
	if err := store.registerCase(c); err != nil {
		t.Fatal(err)
	}
	labelsBefore, err := os.ReadFile(filepath.Join(caseDir, "labels.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.persistEvaluation(c, Evaluation{
		ID:           "evaluation",
		SessionID:    "session",
		CaseID:       c.ID,
		Candidate:    "claude+test",
		Repeat:       1,
		Status:       "completed",
		Pending:      3,
		FindingCount: 3,
	}); err != nil {
		t.Fatal(err)
	}

	queued, err := store.pendingFindingCounts()
	if err != nil {
		t.Fatal(err)
	}
	if queued[c.ID] != 3 {
		t.Fatalf("queued candidate findings = %d, want 3 derived from the recorded evaluation", queued[c.ID])
	}
	// The queue derives from the evaluations table; a replay must never rewrite
	// the case's labels, or re-running the same eval run double-appends state.
	labelsAfter, err := os.ReadFile(filepath.Join(caseDir, "labels.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(labelsBefore) != string(labelsAfter) {
		t.Fatalf("persisting an evaluation rewrote labels.json:\nbefore: %s\nafter: %s", labelsBefore, labelsAfter)
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
	rows := []Evaluation{{CaseID: "only", Candidate: "claude+test", Status: "completed", GoldCount: 1, TruePositive: 1}}
	if got := confidenceInterval("claude+test", rows); got != nil {
		t.Fatalf("single-case confidence interval = %#v, want unavailable", got)
	}
}

func TestConfidenceIntervalRepresentsUniformSampleUncertainty(t *testing.T) {
	rows := []Evaluation{
		{CaseID: "one", Status: "completed", GoldCount: 1, TruePositive: 1},
		{CaseID: "two", Status: "completed", GoldCount: 1, TruePositive: 1},
	}
	got := confidenceInterval("claude+test", rows)
	if got == nil || got.Lower <= 0 || got.Lower >= 1 || got.Upper < 0.999 {
		t.Fatalf("uniform-success confidence interval = %#v, want finite-sample uncertainty ending at 1", got)
	}
}

func TestConfidenceIntervalIncludesFailedLabeledReplays(t *testing.T) {
	rows := []Evaluation{
		{CaseID: "passed", Status: "completed", GoldCount: 1, TruePositive: 1},
		{CaseID: "failed", Status: "failed", GoldCount: 1, FalseNegative: 1},
	}
	got := confidenceInterval("claude+test", rows)
	if got == nil || got.Cases != 2 || got.Lower >= 0.5 || got.Upper <= 0.5 {
		t.Fatalf("confidence interval with scored failure = %#v, want interval around 50%% over two cases", got)
	}
}

func TestRenderReportNamesCaseLevelRecallRange(t *testing.T) {
	output := RenderReport([]CandidateReport{
		{
			Cohort:     "cohort",
			Summary:    EvaluationSummary{Candidate: "claude+test", Total: 2, Labeled: 2, TruePositive: 2},
			Confidence: &Interval{Lower: 0.34, Upper: 1, Cases: 2},
		},
	})
	if !strings.Contains(output, "case-level recall range: 34.0%-100.0% over 2 case(s)") {
		t.Fatalf("report recall range = %q", output)
	}
}

func TestRenderReportKeepsInvalidOnlyScoreWithoutClaimingRecall(t *testing.T) {
	cost := 10.0
	output := RenderReport([]CandidateReport{{
		Cohort:        "cohort",
		Summary:       EvaluationSummary{Candidate: "claude+test", Total: 1, Labeled: 1, FalsePositive: 1},
		AverageTokens: &cost,
	}})
	if !strings.Contains(output, "false-positive 1") || !strings.Contains(output, "recall: unavailable (no true-issue gold)") {
		t.Fatalf("invalid-only report = %q, want FP score with unavailable recall", output)
	}
	if strings.Contains(output, "0/0 gold issues") || strings.Contains(output, "recall-vs-cost frontier: true") {
		t.Fatalf("invalid-only report claims recall evidence: %q", output)
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
		{Cohort: "a", Summary: EvaluationSummary{Labeled: 1, TruePositive: 1}, AverageTokens: &expensive},
		{Cohort: "b", Summary: EvaluationSummary{Labeled: 1, TruePositive: 1}, AverageTokens: &cheap},
	}
	markFrontier(reports)
	if !reports[0].OnFrontier || !reports[1].OnFrontier {
		t.Fatalf("different cohorts dominated each other: %#v", reports)
	}
}

func TestCaptureDoesNotLabelSkipOrApproveAsPass(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status types.StepStatus
	}{
		{name: "approve-with-findings", status: types.StepStatusCompleted},
		{name: "skip", status: types.StepStatusSkipped},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			if err := sourceDB.UpdateStepStatus(steps[0].ID, tc.status); err != nil {
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
			if len(cases) != 1 || cases[0].Labels.HasGold() {
				t.Fatalf("captured labels = %#v, want unlabeled pending gold", cases)
			}
			all := mustSetSummary(t, store, "all")
			if all.GoldCases != 0 || all.Unlabeled != 1 {
				t.Fatalf("all summary = %#v, want unlabeled / pending, not a pass", all)
			}
		})
	}
}

func TestCaptureWritesFalseNegativeGoldForUserAddedFinding(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, reviewRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	userFindings := `{"findings":[{"id":"real-bug","severity":"error","file":"main.go","line":3,"description":"bug","action":"ask-user","review_scope":"source"},{"id":"user-1","severity":"warning","file":"main.go","line":1,"description":"missing audit","action":"auto-fix","source":"user"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`
	if err := sourceDB.SetStepRoundUserFindings(reviewRound.ID, &userFindings); err != nil {
		t.Fatal(err)
	}
	selected := `["real-bug","user-1"]`
	if err := sourceDB.SetStepRoundSelection(reviewRound.ID, &selected, db.RoundSelectionSourceUser); err != nil {
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
	if len(cases) != 1 || len(cases[0].Labels.Findings) != 2 {
		t.Fatalf("captured gold = %#v, want accepted finding plus human-added miss", cases)
	}
	byID := map[string]FindingGold{}
	for _, gold := range cases[0].Labels.Findings {
		byID[gold.ID] = gold
	}
	if got := byID["real-bug"]; got.Kind != GoldTruePositive || got.Source != goldSourceUserFix {
		t.Fatalf("selected agent finding gold = %#v, want true-positive", got)
	}
	if got := byID["user-1"]; got.Kind != GoldFalseNegative || got.Source != goldSourceUserAdded || got.Description != "missing audit" {
		t.Fatalf("user-added gold = %#v, want false-negative miss", got)
	}
}

func TestCaptureWritesUserAddedGoldWithoutSelectionSource(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, reviewRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	userFindings := `{"findings":[{"id":"user-1","severity":"warning","file":"main.go","line":1,"description":"missing audit","action":"auto-fix","source":"user"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`
	if err := sourceDB.SetStepRoundUserFindings(reviewRound.ID, &userFindings); err != nil {
		t.Fatal(err)
	}
	if err := sourceDB.SetStepRoundSelection(reviewRound.ID, nil, ""); err != nil {
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
	if len(cases) != 1 || len(cases[0].Labels.Findings) != 1 {
		t.Fatalf("captured gold = %#v, want independent human-added miss", cases)
	}
	if got := cases[0].Labels.Findings[0]; got.ID != "user-1" || got.Kind != GoldFalseNegative || got.Source != goldSourceUserAdded {
		t.Fatalf("user-added gold = %#v, want false-negative without selection evidence", got)
	}
}

func TestCaptureLeavesUnknownSelectedFindingUnlabeled(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, reviewRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	selected := `["user-added-write-was-lost"]`
	if err := sourceDB.SetStepRoundSelection(reviewRound.ID, &selected, db.RoundSelectionSourceUser); err != nil {
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
	if len(cases) != 1 || cases[0].Labels.HasGold() {
		t.Fatalf("captured labels = %#v, want unknown selection left unlabeled", cases)
	}
}

func TestCaptureAndReportScoresMatchingCandidateAsTruePositive(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	installFakeReviewAgent(t, p, `{"findings":[{"id":"other","severity":"error","file":"main.go","line":3,"description":"bug","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`)

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := Capture(ctx, store, p, sourceDB, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, evaluations, err := Replay(ctx, store, ReplayOptions{Set: "labeled", Candidate: Candidate{Agent: types.AgentClaude, Model: "test"}, Repeats: 1}); err != nil {
		t.Fatal(err)
	} else if len(evaluations) != 1 || evaluations[0].TruePositive != 1 || evaluations[0].FalseNegative != 0 || evaluations[0].Pending != 0 {
		t.Fatalf("replay scores = %#v, want true-positive match on the same issue", evaluations)
	}
	reports, err := Report(store)
	if err != nil {
		t.Fatal(err)
	}
	output := RenderReport(reports)
	if !strings.Contains(output, "true-positive 1, false-negative 0, false-positive 0, pending 0") || !strings.Contains(output, "recall: 100.0%") {
		t.Fatalf("report = %q, want true-positive recall", output)
	}
	if strings.Contains(output, "park") || strings.Contains(output, "verdict") || strings.Contains(output, "agreement") {
		t.Fatalf("report still uses park/pass accuracy language: %q", output)
	}
}

func TestCaptureAndReportLeavesUnmatchedCandidateFindingsPending(t *testing.T) {
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
	if err := sourceDB.UpdateStepStatus(steps[0].ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	installFakeReviewAgent(t, p, `{"findings":[{"id":"new-issue","severity":"error","file":"main.go","line":3,"description":"unexpected later issue","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"new","risk_scope":"source-or-external"}`)

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cases, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 || cases[0].Labels.HasGold() {
		t.Fatalf("approve capture = %#v, want unlabeled gold", cases)
	}
	if _, evaluations, err := Replay(ctx, store, ReplayOptions{Set: "all", Candidate: Candidate{Agent: types.AgentClaude, Model: "test"}, Repeats: 1}); err != nil {
		t.Fatal(err)
	} else if len(evaluations) != 1 || evaluations[0].FalsePositive != 0 || evaluations[0].Pending != 1 {
		t.Fatalf("replay scores = %#v, want unmatched finding queued, not a false-positive", evaluations)
	}
	reports, err := Report(store)
	if err != nil {
		t.Fatal(err)
	}
	output := RenderReport(reports)
	if !strings.Contains(output, "unlabeled / pending") || !strings.Contains(output, "queued unmatched candidate findings: 1") {
		t.Fatalf("report = %q, want unlabeled pending, not a pass or false-positive", output)
	}
	if strings.Contains(output, "false-positive 1") || strings.Contains(output, "park") || strings.Contains(output, "verdict") {
		t.Fatalf("report punished or passed an unlabeled approve case: %q", output)
	}
}

func TestParseCandidateRequiresAgentAndModel(t *testing.T) {
	for _, input := range []string{
		"",
		"claude",
		"claude,model=",
		"claude,=sonnet",
		"claude,model",
		"claude,model=a,model=b",
		"claude,model=sonnet,temperature=0",
		"claude,model=sonnet,effort=turbo",
		"rovodev,model=x",
		"antigravity,model=x",
		"opencode,model=gpt-5",
		"cursor,model=gpt-5,effort=high",
		"nope,model=x",
	} {
		if _, err := ParseCandidate(input); err == nil {
			t.Errorf("ParseCandidate(%q) succeeded, want error", input)
		}
	}
	candidate, err := ParseCandidate(" codex,model=gpt-5.4 ")
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Agent != types.AgentCodex || candidate.Model != "gpt-5.4" || candidate.Effort != "" {
		t.Fatalf("candidate = %#v", candidate)
	}
}

// TestParseCandidateRejectsTheReplacedSpelling keeps the removed agent+model
// syntax from failing with a generic parse error: the message has to name the
// replacement, because every older invocation and doc snippet uses it.
func TestParseCandidateRejectsTheReplacedSpelling(t *testing.T) {
	_, err := ParseCandidate("codex+gpt-5.4")
	if err == nil {
		t.Fatal("ParseCandidate(codex+gpt-5.4) succeeded, want a migration error")
	}
	if !strings.Contains(err.Error(), "model=") {
		t.Fatalf("error %v does not show the replacement spelling", err)
	}
	// A model id containing a plus is still a legitimate new-form candidate and
	// must not be mistaken for the replaced spelling.
	candidate, err := ParseCandidate("claude,model=some+model")
	if err != nil {
		t.Fatalf("ParseCandidate with a plus in the model = %v", err)
	}
	if candidate.Model != "some+model" {
		t.Fatalf("candidate = %#v", candidate)
	}
}

// TestParseCandidateCarriesTheEffortAxis is the eval half of the unified
// abstraction: effort is expressible for a candidate exactly as it is in
// agent_config, and it stays part of the candidate identity so two efforts of
// one model never collapse into a single reported candidate.
func TestParseCandidateCarriesTheEffortAxis(t *testing.T) {
	candidate, err := ParseCandidate("codex,model=gpt-5.4,effort=low")
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Effort != agentcfg.EffortLow {
		t.Fatalf("candidate effort = %q, want low", candidate.Effort)
	}
	if got, want := candidate.Profile(), (agentcfg.Profile{Model: "gpt-5.4", Effort: agentcfg.EffortLow}); got != want {
		t.Fatalf("candidate profile = %#v, want %#v", got, want)
	}
	if got := candidate.String(); got != "codex,model=gpt-5.4,effort=low" {
		t.Fatalf("candidate string = %q", got)
	}
	other, err := ParseCandidate("codex,model=gpt-5.4,effort=high")
	if err != nil {
		t.Fatal(err)
	}
	if other.String() == candidate.String() {
		t.Fatal("two efforts of one model share a candidate identity")
	}
}

// TestParseCandidatePinsACPModelThroughAcpx records the closed gap: an ACP
// target used to be refused outright because no-mistakes had no way to enforce
// its model.
func TestParseCandidatePinsACPModelThroughAcpx(t *testing.T) {
	for _, input := range []string{"cursor,model=gpt-5", "acp:custom,model=gpt-5"} {
		candidate, err := ParseCandidate(input)
		if err != nil {
			t.Fatalf("ParseCandidate(%q) = %v, want an accepted candidate", input, err)
		}
		if candidate.Model != "gpt-5" {
			t.Fatalf("candidate = %#v", candidate)
		}
	}
}

func setupCapturedRun(t *testing.T, ctx context.Context) (*paths.Paths, *db.DB, *db.Run, *db.Repo, *db.StepRound) {
	t.Helper()
	return setupCapturedRunWithFindings(t, ctx, `{"findings":[{"id":"real-bug","severity":"error","file":"main.go","line":3,"description":"bug","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`)
}

func setupCapturedRunWithFindings(t *testing.T, ctx context.Context, findings string) (*paths.Paths, *db.DB, *db.Run, *db.Repo, *db.StepRound) {
	t.Helper()
	return setupCapturedRunWithHistoryAndFindings(t, ctx, 0, findings)
}

// setupCapturedRunWithHistory builds the fixture run. padCommits adds that many
// commits of incompressible content to the default branch BEFORE the reviewed
// branch is cut, so the padding is real ancestry of every commit a case pins -
// which is what makes duplicated history measurable.
func setupCapturedRunWithHistory(t *testing.T, ctx context.Context, padCommits int) (*paths.Paths, *db.DB, *db.Run, *db.Repo, *db.StepRound) {
	t.Helper()
	return setupCapturedRunWithHistoryAndFindings(t, ctx, padCommits, `{"findings":[{"id":"real-bug","severity":"error","file":"main.go","line":3,"description":"bug","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`)
}

func setupCapturedRunWithHistoryAndFindings(t *testing.T, ctx context.Context, padCommits int, findings string) (*paths.Paths, *db.DB, *db.Run, *db.Repo, *db.StepRound) {
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

func mustInspectSets(t *testing.T, store *Store) []SetSummary {
	t.Helper()
	summaries, err := InspectSets(store)
	if err != nil {
		t.Fatal(err)
	}
	return summaries
}

func installFakeReviewAgent(t *testing.T, p *paths.Paths, findingsJSON string) {
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
		script = "#!/bin/sh\n[ \"$NM_HOME\" = \"" + p.Root() + "\" ] && touch \"" + p.Root() + "/shared-home-used\"\ncat >/dev/null\ncat <<'EOF'\n" + reply + "EOF\n"
	}
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(fake)+string(os.PathListSeparator)+os.Getenv("PATH"))
}
