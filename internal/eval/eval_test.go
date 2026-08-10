package eval

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	if _, err := os.Stat(filepath.Join(captured.Dir, "branch.bundle")); err != nil {
		t.Fatalf("case bundle missing: %v", err)
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

func TestCaptureUsesSourceRunsPinnedTrustedConfiguration(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
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
	if len(cases) != 1 || cases[0].TrustedConfigSHA != *run.TrustedConfigSHA {
		t.Fatalf("trusted config pin = %#v, want %s", cases, *run.TrustedConfigSHA)
	}
	repoConfig, err := os.ReadFile(filepath.Join(cases[0].Dir, "config", "repo-config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(repoConfig), "advanced-only") {
		t.Fatalf("capture used advanced default-branch config: %s", repoConfig)
	}
}

func TestReplayRestoresCaseIntoAnIsolatedWorktree(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
	defer sourceDB.Close()

	fake := filepath.Join(t.TempDir(), "claude")
	const reply = `{"type":"assistant","message":{"usage":{"input_tokens":12,"output_tokens":3},"content":[{"type":"text","text":"clean"}]}}
{"type":"result","subtype":"success","is_error":false,"structured_output":{"findings":[],"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"},"usage":{"input_tokens":12,"output_tokens":3}}
`
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n[ \"$NM_HOME\" = \""+p.Root()+"\" ] && touch \""+p.Root()+"/shared-home-used\"\ncat >/dev/null\ncat <<'EOF'\n"+reply+"EOF\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.ConfigFile(), []byte("agent_path_override:\n  claude: "+fake+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
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

func TestConfidenceIntervalRequiresMultipleIndependentCases(t *testing.T) {
	rows := []Evaluation{{CaseID: "only", Candidate: "claude+test", Status: "completed", ExpectedPark: boolPtr(true), CandidateParked: true}}
	if got := confidenceInterval("claude+test", rows); got != nil {
		t.Fatalf("single-case confidence interval = %#v, want unavailable", got)
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
	if err := database.UpdateRunTrustedConfigSHA(run.ID, baseSHA); err != nil {
		t.Fatal(err)
	}
	run.TrustedConfigSHA = &baseSHA
	step, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"real-bug","severity":"error","file":"main.go","line":3,"description":"bug","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`
	reviewRound, err := database.InsertReviewStepRound(step.ID, 1, "initial", &findings, nil, headSHA, 50)
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
