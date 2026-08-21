package pipeline

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutor_BindsUncertifiedRangeOntoInitialReview(t *testing.T) {
	database, p, run, repo := setupTest(t)
	if err := database.UpsertUncertifiedPipelineRange(repo.ID, run.Branch, "from-sha", run.HeadSHA, "source-run"); err != nil {
		t.Fatal(err)
	}
	var gotFrom, gotTo, gotSource string
	var fixing bool
	step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		gotFrom, gotTo, gotSource = sctx.UncertifiedFromSHA, sctx.UncertifiedToSHA, sctx.UncertifiedSourceRunID
		fixing = sctx.Fixing
		return &StepOutcome{ReviewApprovedHeadSHA: run.HeadSHA}, nil
	}}
	exec := NewExecutor(database, p, &config.Config{}, nil, []Step{step}, nil)
	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if fixing {
		t.Fatal("initial review ran in fix mode")
	}
	if gotFrom != "from-sha" || gotTo != run.HeadSHA || gotSource != "source-run" {
		t.Fatalf("initial review bound from=%q to=%q source=%q", gotFrom, gotTo, gotSource)
	}
}

func TestBindUncertifiedPipelineRange_CopiesOntoStepContext(t *testing.T) {
	database, _, run, repo := setupTest(t)
	if err := database.UpsertUncertifiedPipelineRange(repo.ID, run.Branch, "from-sha", run.HeadSHA, "source-run"); err != nil {
		t.Fatal(err)
	}
	source, err := database.InsertRun(repo.ID, run.Branch, "older", "base")
	if err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(source.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"r1","severity":"error","description":"prior bug","action":"auto-fix"}]}`
	if _, err := database.InsertStepRound(step.ID, 1, "initial", &findings, nil, 10); err != nil {
		t.Fatal(err)
	}
	if err := database.UpsertUncertifiedPipelineRange(repo.ID, run.Branch, "from-sha", run.HeadSHA, source.ID); err != nil {
		t.Fatal(err)
	}

	sctx := &StepContext{
		Ctx:     context.Background(),
		DB:      database,
		Repo:    repo,
		Run:     run,
		WorkDir: t.TempDir(),
	}
	BindUncertifiedPipelineRange(sctx)
	if sctx.UncertifiedFromSHA != "from-sha" || sctx.UncertifiedToSHA != run.HeadSHA || sctx.UncertifiedSourceRunID != source.ID {
		t.Fatalf("bound range = from=%q to=%q source=%q", sctx.UncertifiedFromSHA, sctx.UncertifiedToSHA, sctx.UncertifiedSourceRunID)
	}
	if len(sctx.UncertifiedPriorRounds) != 1 {
		t.Fatalf("prior rounds = %d, want 1", len(sctx.UncertifiedPriorRounds))
	}
}

func TestBindUncertifiedPipelineRange_MissingFromGateWarnsAndContinues(t *testing.T) {
	database, _, run, repo := setupTest(t)
	if err := database.UpsertUncertifiedPipelineRange(repo.ID, run.Branch, "from-missing", "to-missing", "source-run"); err != nil {
		t.Fatal(err)
	}
	var logs []string
	sctx := &StepContext{
		Ctx:     context.Background(),
		DB:      database,
		Repo:    repo,
		Run:     run,
		WorkDir: t.TempDir(),
		Log:     func(line string) { logs = append(logs, line) },
	}
	BindUncertifiedPipelineRange(sctx)
	if sctx.UncertifiedToSHA != "" || sctx.UncertifiedFromSHA != "" {
		t.Fatalf("missing range was applied: from=%q to=%q", sctx.UncertifiedFromSHA, sctx.UncertifiedToSHA)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "uncertified range from-missing..to-missing not in gate; not applying provenance") {
		t.Fatalf("logs = %q, want skip warning", joined)
	}
}

func TestBindUncertifiedPipelineRange_DoesNotBindWhileFixing(t *testing.T) {
	database, _, run, repo := setupTest(t)
	if err := database.UpsertUncertifiedPipelineRange(repo.ID, run.Branch, "from-sha", run.HeadSHA, "source-run"); err != nil {
		t.Fatal(err)
	}
	sctx := &StepContext{
		Ctx:     context.Background(),
		DB:      database,
		Repo:    repo,
		Run:     run,
		WorkDir: t.TempDir(),
		Fixing:  true,
	}
	BindUncertifiedPipelineRange(sctx)
	if sctx.UncertifiedToSHA != "" {
		t.Fatalf("fixing review bound uncertified range %q", sctx.UncertifiedToSHA)
	}
}

func TestApprovedReview_ClearsUncertifiedRange(t *testing.T) {
	database, p, run, repo := setupTest(t)
	if err := database.UpsertUncertifiedPipelineRange(repo.ID, run.Branch, "from-sha", run.HeadSHA, run.ID); err != nil {
		t.Fatal(err)
	}
	step := &mockStep{name: types.StepReview, outcome: &StepOutcome{ReviewApprovedHeadSHA: run.HeadSHA}}
	exec := NewExecutor(database, p, &config.Config{}, nil, []Step{step}, nil)
	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	got, err := database.GetUncertifiedPipelineRange(repo.ID, run.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("certified review left uncertified range %#v", got)
	}
}

func TestParkedReview_DoesNotClearUncertifiedRange(t *testing.T) {
	database, p, run, repo := setupTest(t)
	if err := database.UpsertUncertifiedPipelineRange(repo.ID, run.Branch, "from-sha", run.HeadSHA, run.ID); err != nil {
		t.Fatal(err)
	}
	step := &mockStep{
		name: types.StepReview,
		outcome: &StepOutcome{
			NeedsApproval:         true,
			Findings:              `{"findings":[{"id":"r1","severity":"error","description":"fix me","action":"ask-user"}]}`,
			ReviewApprovedHeadSHA: run.HeadSHA,
		},
	}
	exec := NewExecutor(database, p, &config.Config{}, nil, []Step{step}, nil)
	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	got, err := database.GetUncertifiedPipelineRange(repo.ID, run.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ToSHA != run.HeadSHA {
		t.Fatalf("parked review cleared uncertified range: %#v", got)
	}
	if err := exec.Respond(types.StepReview, types.ActionAbort, nil); err != nil {
		t.Fatal(err)
	}
	<-done
	got, err = database.GetUncertifiedPipelineRange(repo.ID, run.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ToSHA != run.HeadSHA {
		t.Fatalf("aborted review cleared uncertified range: %#v", got)
	}
}

func TestFailedReview_DoesNotClearUncertifiedRange(t *testing.T) {
	database, p, run, repo := setupTest(t)
	if err := database.UpsertUncertifiedPipelineRange(repo.ID, run.Branch, "from-sha", run.HeadSHA, run.ID); err != nil {
		t.Fatal(err)
	}
	step := newFailStep(types.StepReview, fmt.Errorf("review agent failed"))
	exec := NewExecutor(database, p, &config.Config{}, nil, []Step{step}, nil)
	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err == nil {
		t.Fatal("expected failed review")
	}
	got, err := database.GetUncertifiedPipelineRange(repo.ID, run.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ToSHA != run.HeadSHA {
		t.Fatalf("failed review cleared uncertified range: %#v", got)
	}
}

func TestApprovedReview_ClearsUncertifiedRangeWhenApprovedHeadIsDescendant(t *testing.T) {
	database, p, run, repo := setupTest(t)
	dir := t.TempDir()
	initGitRepo(t, dir)
	fromSHA := currentSHA(t, dir)
	writeTestFile(t, dir, "fixer.txt", "fixer\n")
	execGit(t, dir, "add", ".")
	execGit(t, dir, "commit", "-m", "fixer")
	toSHA := currentSHA(t, dir)
	writeTestFile(t, dir, "later.txt", "later\n")
	execGit(t, dir, "add", ".")
	execGit(t, dir, "commit", "-m", "later pipeline commit")
	approved := currentSHA(t, dir)
	run.HeadSHA = approved
	if err := database.UpsertUncertifiedPipelineRange(repo.ID, run.Branch, fromSHA, toSHA, run.ID); err != nil {
		t.Fatal(err)
	}
	step := &mockStep{name: types.StepReview, outcome: &StepOutcome{ReviewApprovedHeadSHA: approved}}
	exec := NewExecutor(database, p, &config.Config{}, nil, []Step{step}, nil)
	if err := exec.Execute(context.Background(), run, repo, dir); err != nil {
		t.Fatal(err)
	}
	got, err := database.GetUncertifiedPipelineRange(repo.ID, run.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("descendant approved head left uncertified range %#v", got)
	}
}

func TestApprovedReview_DoesNotClearWhenApprovedHeadIsNotDescendant(t *testing.T) {
	database, p, run, repo := setupTest(t)
	dir := t.TempDir()
	initGitRepo(t, dir)
	fromSHA := currentSHA(t, dir)
	writeTestFile(t, dir, "fixer.txt", "fixer\n")
	execGit(t, dir, "add", ".")
	execGit(t, dir, "commit", "-m", "fixer")
	toSHA := currentSHA(t, dir)
	execGit(t, dir, "checkout", "-b", "other", fromSHA)
	writeTestFile(t, dir, "other.txt", "other\n")
	execGit(t, dir, "add", ".")
	execGit(t, dir, "commit", "-m", "unrelated head")
	other := currentSHA(t, dir)
	run.HeadSHA = other
	if err := database.UpsertUncertifiedPipelineRange(repo.ID, run.Branch, fromSHA, toSHA, run.ID); err != nil {
		t.Fatal(err)
	}
	step := &mockStep{name: types.StepReview, outcome: &StepOutcome{ReviewApprovedHeadSHA: other}}
	exec := NewExecutor(database, p, &config.Config{}, nil, []Step{step}, nil)
	if err := exec.Execute(context.Background(), run, repo, dir); err != nil {
		t.Fatal(err)
	}
	got, err := database.GetUncertifiedPipelineRange(repo.ID, run.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.FromSHA != fromSHA || got.ToSHA != toSHA {
		t.Fatalf("non-descendant approved head cleared or rewrote range: %#v", got)
	}
}

func TestPersistUncertifiedPipelineRange_KeepsFromSHAAcrossRunsOnSameLineage(t *testing.T) {
	database, _, runA, repo := setupTest(t)
	dir := t.TempDir()
	initGitRepo(t, dir)
	h0 := currentSHA(t, dir)
	writeTestFile(t, dir, "fix-a.txt", "a\n")
	execGit(t, dir, "add", ".")
	execGit(t, dir, "commit", "-m", "fixer a")
	h1 := currentSHA(t, dir)
	writeTestFile(t, dir, "fix-b.txt", "b\n")
	execGit(t, dir, "add", ".")
	execGit(t, dir, "commit", "-m", "fixer b")
	h2 := currentSHA(t, dir)

	PersistUncertifiedPipelineRange(&StepContext{
		Ctx:     context.Background(),
		DB:      database,
		Repo:    repo,
		Run:     runA,
		WorkDir: dir,
	}, h0, h1)

	runB, err := database.InsertRun(repo.ID, runA.Branch, h2, "base")
	if err != nil {
		t.Fatal(err)
	}
	PersistUncertifiedPipelineRange(&StepContext{
		Ctx:     context.Background(),
		DB:      database,
		Repo:    repo,
		Run:     runB,
		WorkDir: dir,
	}, h1, h2)

	got, err := database.GetUncertifiedPipelineRange(repo.ID, runA.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.FromSHA != h0 || got.ToSHA != h2 || got.SourceRunID != runB.ID {
		t.Fatalf("cross-run persist = %#v, want from=%s to=%s source=%s", got, h0, h2, runB.ID)
	}
}

func TestPersistUncertifiedPipelineRange_KeepsFromSHAOnSameRun(t *testing.T) {
	database, _, run, repo := setupTest(t)
	dir := t.TempDir()
	initGitRepo(t, dir)
	h0 := currentSHA(t, dir)
	writeTestFile(t, dir, "fix-1.txt", "1\n")
	execGit(t, dir, "add", ".")
	execGit(t, dir, "commit", "-m", "fixer 1")
	h1 := currentSHA(t, dir)
	writeTestFile(t, dir, "fix-2.txt", "2\n")
	execGit(t, dir, "add", ".")
	execGit(t, dir, "commit", "-m", "fixer 2")
	h2 := currentSHA(t, dir)

	sctx := &StepContext{
		Ctx:     context.Background(),
		DB:      database,
		Repo:    repo,
		Run:     run,
		WorkDir: dir,
	}
	PersistUncertifiedPipelineRange(sctx, h0, h1)
	PersistUncertifiedPipelineRange(sctx, h1, h2)

	got, err := database.GetUncertifiedPipelineRange(repo.ID, run.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.FromSHA != h0 || got.ToSHA != h2 || got.SourceRunID != run.ID {
		t.Fatalf("same-run persist = %#v, want from=%s to=%s source=%s", got, h0, h2, run.ID)
	}
}

func TestPersistUncertifiedPipelineRange_ReplacesRangeWhenHistoryDiverged(t *testing.T) {
	database, _, runA, repo := setupTest(t)
	dir := t.TempDir()
	initGitRepo(t, dir)
	h0 := currentSHA(t, dir)
	writeTestFile(t, dir, "line-a.txt", "a\n")
	execGit(t, dir, "add", ".")
	execGit(t, dir, "commit", "-m", "line a")
	h1 := currentSHA(t, dir)

	PersistUncertifiedPipelineRange(&StepContext{
		Ctx:     context.Background(),
		DB:      database,
		Repo:    repo,
		Run:     runA,
		WorkDir: dir,
	}, h0, h1)

	execGit(t, dir, "checkout", "-b", "diverged", h0)
	writeTestFile(t, dir, "line-c.txt", "c\n")
	execGit(t, dir, "add", ".")
	execGit(t, dir, "commit", "-m", "line c")
	hC := currentSHA(t, dir)
	writeTestFile(t, dir, "line-d.txt", "d\n")
	execGit(t, dir, "add", ".")
	execGit(t, dir, "commit", "-m", "line d")
	hD := currentSHA(t, dir)

	runB, err := database.InsertRun(repo.ID, runA.Branch, hD, "base")
	if err != nil {
		t.Fatal(err)
	}
	PersistUncertifiedPipelineRange(&StepContext{
		Ctx:     context.Background(),
		DB:      database,
		Repo:    repo,
		Run:     runB,
		WorkDir: dir,
	}, hC, hD)

	got, err := database.GetUncertifiedPipelineRange(repo.ID, runA.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.FromSHA != hC || got.ToSHA != hD || got.SourceRunID != runB.ID {
		t.Fatalf("diverged persist = %#v, want from=%s to=%s source=%s", got, hC, hD, runB.ID)
	}
}

func TestRemapUncertifiedPipelineRangeAfterRebase_RewrittenHeadStaysBindable(t *testing.T) {
	database, _, run, repo := setupTest(t)
	dir := t.TempDir()
	initGitRepo(t, dir)
	base := currentSHA(t, dir)
	execGit(t, dir, "checkout", "-b", "feature")
	writeTestFile(t, dir, "author.txt", "author\n")
	execGit(t, dir, "add", ".")
	execGit(t, dir, "commit", "-m", "author")
	fromSHA := currentSHA(t, dir)
	writeTestFile(t, dir, "fixer.txt", "fixer\n")
	execGit(t, dir, "add", ".")
	execGit(t, dir, "commit", "-m", "fixer")
	toSHA := currentSHA(t, dir)
	if err := database.UpsertUncertifiedPipelineRange(repo.ID, run.Branch, fromSHA, toSHA, run.ID); err != nil {
		t.Fatal(err)
	}

	execGit(t, dir, "branch", "newbase", base)
	execGit(t, dir, "checkout", "newbase")
	writeTestFile(t, dir, "main.txt", "main\n")
	execGit(t, dir, "add", ".")
	execGit(t, dir, "commit", "-m", "main advance")
	execGit(t, dir, "checkout", "feature")
	execGit(t, dir, "rebase", "newbase")
	newHead := currentSHA(t, dir)
	if newHead == toSHA {
		t.Fatal("rebase did not rewrite the uncertified head")
	}

	sctx := &StepContext{
		Ctx:     context.Background(),
		DB:      database,
		Repo:    repo,
		Run:     run,
		WorkDir: dir,
	}
	sctx.Run.HeadSHA = newHead
	RemapUncertifiedPipelineRangeAfterRebase(sctx, toSHA, newHead)

	got, err := database.GetUncertifiedPipelineRange(repo.ID, run.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.FromSHA == fromSHA || got.ToSHA == toSHA {
		t.Fatalf("remap left old SHAs: %#v (old from=%s to=%s)", got, fromSHA, toSHA)
	}
	if got.ToSHA != newHead || got.SourceRunID != run.ID {
		t.Fatalf("remapped range = %#v, want to=%s source=%s", got, newHead, run.ID)
	}

	BindUncertifiedPipelineRange(sctx)
	if sctx.UncertifiedFromSHA != got.FromSHA || sctx.UncertifiedToSHA != got.ToSHA {
		t.Fatalf("bind after remap from=%q to=%q, want from=%q to=%q", sctx.UncertifiedFromSHA, sctx.UncertifiedToSHA, got.FromSHA, got.ToSHA)
	}
}

func TestRemapUncertifiedPipelineRangeAfterRebase_LeavesRangeWhenOldHeadDidNotContainIt(t *testing.T) {
	database, _, run, repo := setupTest(t)
	dir := t.TempDir()
	initGitRepo(t, dir)
	base := currentSHA(t, dir)
	writeTestFile(t, dir, "other.txt", "other\n")
	execGit(t, dir, "add", ".")
	execGit(t, dir, "commit", "-m", "other")
	oldHead := currentSHA(t, dir)
	if err := database.UpsertUncertifiedPipelineRange(repo.ID, run.Branch, "from-missing", "to-missing", run.ID); err != nil {
		t.Fatal(err)
	}

	execGit(t, dir, "checkout", "-b", "rewritten", base)
	writeTestFile(t, dir, "rewrite.txt", "rewrite\n")
	execGit(t, dir, "add", ".")
	execGit(t, dir, "commit", "-m", "rewrite")
	newHead := currentSHA(t, dir)

	RemapUncertifiedPipelineRangeAfterRebase(&StepContext{
		Ctx:     context.Background(),
		DB:      database,
		Repo:    repo,
		Run:     run,
		WorkDir: dir,
	}, oldHead, newHead)

	got, err := database.GetUncertifiedPipelineRange(repo.ID, run.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.FromSHA != "from-missing" || got.ToSHA != "to-missing" {
		t.Fatalf("unrelated range was rewritten: %#v", got)
	}
}

func currentSHA(t *testing.T, dir string) string {
	t.Helper()
	sha, err := git.HeadSHA(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return sha
}
