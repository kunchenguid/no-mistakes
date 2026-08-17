package pipeline

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestValidationCheckpointRequiresExactMechanicalEvidence(t *testing.T) {
	database, p, run, _ := setupTest(t)
	workDir := t.TempDir()
	initGitRepo(t, workDir)
	head, err := gitHeadForCheckpointTest(workDir)
	if err != nil {
		t.Fatal(err)
	}
	run.HeadSHA = head
	run.BaseSHA = strings.Repeat("b", 40)
	if err := database.UpdateRunHeadSHA(run.ID, run.HeadSHA); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunReviewApprovedHeadSHA(run.ID, run.HeadSHA); err != nil {
		t.Fatal(err)
	}
	approved := run.HeadSHA
	run.ReviewApprovedHeadSHA = &approved

	logDir := p.RunLogDir(run.ID)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range validationStepNames {
		result, err := database.InsertStepResult(run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := database.StartStep(result.ID); err != nil {
			t.Fatal(err)
		}
		logPath := filepath.Join(logDir, string(name)+".log")
		if err := os.WriteFile(logPath, []byte("evidence for "+name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := database.InsertStepRound(result.ID, 1, "initial", nil, nil, 10); err != nil {
			t.Fatal(err)
		}
		if err := database.CompleteStep(result.ID, 0, 10, logPath); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []types.StepName{types.StepPush, types.StepPR, types.StepCI} {
		if _, err := database.InsertStepResult(run.ID, name); err != nil {
			t.Fatal(err)
		}
	}
	evidenceDir := p.RunEvidenceDir("", run.ID)
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(evidenceDir, "result.txt")
	if err := os.WriteFile(artifact, []byte("passed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{TrustedConfigSHA: strings.Repeat("c", 40), Commands: config.Commands{Test: "go test ./..."}}
	checkpoint, err := PersistValidationCheckpoint(context.Background(), database, p, cfg, run, workDir)
	if err != nil {
		t.Fatalf("PersistValidationCheckpoint() error = %v", err)
	}
	if checkpoint == nil || checkpoint.RunID != run.ID || len(checkpoint.EvidenceHashes) < len(validationStepNames)*2+1 {
		t.Fatalf("checkpoint = %#v", checkpoint)
	}
	if err := ValidateValidationCheckpoint(context.Background(), database, p, cfg, run, checkpoint); err != nil {
		t.Fatalf("ValidateValidationCheckpoint() error = %v", err)
	}

	t.Run("artifact changed", func(t *testing.T) {
		if err := os.WriteFile(artifact, []byte("different\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := ValidateValidationCheckpoint(context.Background(), database, p, cfg, run, checkpoint); err == nil {
			t.Fatal("changed evidence artifact reused validation")
		}
	})
}

func TestValidationCheckpointRejectsChangedHeadBaseAndConfig(t *testing.T) {
	checkpoint := &db.ValidationCheckpoint{
		RunID:          "run",
		Version:        validationCheckpointVersion,
		ValidatedSHA:   strings.Repeat("a", 40),
		BaseSHA:        strings.Repeat("b", 40),
		ConfigHash:     strings.Repeat("c", 64),
		IntentHash:     strings.Repeat("e", 64),
		EvidenceHashes: map[string]string{"artifact-manifest": strings.Repeat("d", 64)},
	}
	run := &db.Run{ID: "run", HeadSHA: checkpoint.ValidatedSHA, BaseSHA: checkpoint.BaseSHA}

	for _, tc := range []struct {
		name string
		edit func(*db.Run, *db.ValidationCheckpoint)
	}{
		{name: "head", edit: func(run *db.Run, _ *db.ValidationCheckpoint) { run.HeadSHA = strings.Repeat("e", 40) }},
		{name: "base", edit: func(run *db.Run, _ *db.ValidationCheckpoint) { run.BaseSHA = strings.Repeat("e", 40) }},
		{name: "malformed hash", edit: func(_ *db.Run, cp *db.ValidationCheckpoint) { cp.ConfigHash = "not-a-hash" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotRun := *run
			gotCheckpoint := *checkpoint
			tc.edit(&gotRun, &gotCheckpoint)
			if err := validateCheckpointEnvelope(&gotRun, &gotCheckpoint); err == nil {
				t.Fatal("divergent checkpoint envelope was accepted")
			}
		})
	}
}

func TestExecutorPersistsCheckpointAtDeliveryBoundary(t *testing.T) {
	database, p, _, repo := setupTest(t)
	workDir := t.TempDir()
	initGitRepo(t, workDir)
	head, err := gitHeadForCheckpointTest(workDir)
	if err != nil {
		t.Fatal(err)
	}
	base := strings.Repeat("b", 40)
	run, err := database.InsertRun(repo.ID, "checkpoint", head, base)
	if err != nil {
		t.Fatal(err)
	}
	steps := make([]Step, 0, len(types.AllSteps()))
	for _, name := range types.AllSteps() {
		step := newPassStep(name)
		if name == types.StepReview {
			step.outcome.ReviewApprovedHeadSHA = head
		}
		steps = append(steps, step)
	}
	executor := NewExecutor(database, p, &config.Config{}, nil, steps, nil)
	if err := executor.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	checkpoint, err := database.GetValidationCheckpoint(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint == nil || checkpoint.ValidatedSHA != head || checkpoint.BaseSHA != base {
		t.Fatalf("checkpoint = %#v", checkpoint)
	}
}

func TestExecutorRetryReusesValidationAndRunsOnlyDelivery(t *testing.T) {
	database, p, _, repo := setupTest(t)
	workDir := t.TempDir()
	initGitRepo(t, workDir)
	head, err := gitHeadForCheckpointTest(workDir)
	if err != nil {
		t.Fatal(err)
	}
	base := head
	cfg := &config.Config{}

	source, err := database.InsertRun(repo.ID, "delivery-retry", head, base)
	if err != nil {
		t.Fatal(err)
	}
	sourceSteps := make([]Step, 0, len(types.AllSteps()))
	for _, name := range types.AllSteps() {
		step := newPassStep(name)
		if name == types.StepReview {
			step.outcome.ReviewApprovedHeadSHA = head
		}
		if name == types.StepPush {
			step.err = fmt.Errorf("temporary remote failure")
		}
		sourceSteps = append(sourceSteps, step)
	}
	if err := NewExecutor(database, p, cfg, nil, sourceSteps, nil).Execute(context.Background(), source, repo, workDir); err == nil {
		t.Fatal("source run unexpectedly completed")
	}

	target, err := database.InsertRun(repo.ID, "delivery-retry", head, base)
	if err != nil {
		t.Fatal(err)
	}
	reusedFrom, err := PrepareValidationReuse(context.Background(), database, p, cfg, target, workDir)
	if err != nil {
		t.Fatalf("PrepareValidationReuse() error = %v", err)
	}
	if reusedFrom != source.ID {
		t.Fatalf("reused from %q, want %q", reusedFrom, source.ID)
	}

	called := map[types.StepName]*mockStep{}
	targetSteps := make([]Step, 0, len(types.AllSteps()))
	for _, name := range types.AllSteps() {
		step := newPassStep(name)
		called[name] = step
		targetSteps = append(targetSteps, step)
	}
	if err := NewExecutor(database, p, cfg, nil, targetSteps, nil).Execute(context.Background(), target, repo, workDir); err != nil {
		t.Fatalf("retry Execute() error = %v", err)
	}
	for _, name := range validationStepNames {
		if got := called[name].callCount(); got != 0 {
			t.Errorf("validation step %s called %d times, want 0", name, got)
		}
	}
	for _, name := range []types.StepName{types.StepPush, types.StepPR, types.StepCI} {
		if got := called[name].callCount(); got != 1 {
			t.Errorf("delivery step %s called %d times, want 1", name, got)
		}
	}
}

func gitHeadForCheckpointTest(dir string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

type failedCheckpointFixture struct {
	database *db.DB
	p        *paths.Paths
	repo     *db.Repo
	source   *db.Run
	cfg      *config.Config
	workDir  string
	head     string
	base     string
}

func newFailedCheckpointFixture(t *testing.T) *failedCheckpointFixture {
	t.Helper()
	database, p, _, repo := setupTest(t)
	workDir := t.TempDir()
	initGitRepo(t, workDir)
	head, err := gitHeadForCheckpointTest(workDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Commands: config.Commands{Test: "go test ./..."}}
	source, err := database.InsertRun(repo.ID, "invalidation", head, head)
	if err != nil {
		t.Fatal(err)
	}
	steps := make([]Step, 0, len(types.AllSteps()))
	for _, name := range types.AllSteps() {
		step := newPassStep(name)
		if name == types.StepReview {
			step.outcome.ReviewApprovedHeadSHA = head
		}
		if name == types.StepPush {
			step.err = fmt.Errorf("temporary delivery failure")
		}
		steps = append(steps, step)
	}
	if err := NewExecutor(database, p, cfg, nil, steps, nil).Execute(context.Background(), source, repo, workDir); err == nil {
		t.Fatal("source run unexpectedly completed")
	}
	source, err = database.GetRun(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	return &failedCheckpointFixture{database: database, p: p, repo: repo, source: source, cfg: cfg, workDir: workDir, head: head, base: head}
}

func (f *failedCheckpointFixture) target(t *testing.T, head, base string, intent *db.RunIntent) *db.Run {
	t.Helper()
	run, err := f.database.InsertRunWithIntent(f.repo.ID, f.source.Branch, head, base, intent)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestValidationReuseFailsClosedAtEveryInvalidationBoundary(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *failedCheckpointFixture) (*db.Run, *config.Config)
	}{
		{name: "validated commit changed by rebase", mutate: func(t *testing.T, f *failedCheckpointFixture) (*db.Run, *config.Config) {
			writeTestFile(t, f.workDir, "rebase.txt", "new base\n")
			execGit(t, f.workDir, "add", ".")
			execGit(t, f.workDir, "commit", "-m", "rebased")
			head, _ := gitHeadForCheckpointTest(f.workDir)
			return f.target(t, head, f.base, nil), f.cfg
		}},
		{name: "base changed", mutate: func(t *testing.T, f *failedCheckpointFixture) (*db.Run, *config.Config) {
			return f.target(t, f.head, strings.Repeat("d", 40), nil), f.cfg
		}},
		{name: "pipeline config changed", mutate: func(t *testing.T, f *failedCheckpointFixture) (*db.Run, *config.Config) {
			changed := *f.cfg
			changed.Commands.Test = "go test -race ./..."
			return f.target(t, f.head, f.base, nil), &changed
		}},
		{name: "provenance capture changed", mutate: func(t *testing.T, f *failedCheckpointFixture) (*db.Run, *config.Config) {
			changed := *f.cfg
			changed.CaptureEvalProvenance = !changed.CaptureEvalProvenance
			return f.target(t, f.head, f.base, nil), &changed
		}},
		{name: "replay global config changed", mutate: func(t *testing.T, f *failedCheckpointFixture) (*db.Run, *config.Config) {
			changed := *f.cfg
			changed.ReplayGlobalYAML = []byte("agent: codex\n")
			return f.target(t, f.head, f.base, nil), &changed
		}},
		{name: "replay repo config changed", mutate: func(t *testing.T, f *failedCheckpointFixture) (*db.Run, *config.Config) {
			changed := *f.cfg
			changed.ReplayRepoYAML = []byte("no_ci: true\n")
			return f.target(t, f.head, f.base, nil), &changed
		}},
		{name: "intent changed", mutate: func(t *testing.T, f *failedCheckpointFixture) (*db.Run, *config.Config) {
			return f.target(t, f.head, f.base, &db.RunIntent{Summary: "different acceptance criteria", Source: db.RunIntentSourceAgent}), f.cfg
		}},
		{name: "build changed", mutate: func(t *testing.T, f *failedCheckpointFixture) (*db.Run, *config.Config) {
			target := f.target(t, f.head, f.base, nil)
			changed := "different-build"
			target.NoMistakesBuildSHA = &changed
			return target, f.cfg
		}},
		{name: "terminal head unverified", mutate: func(t *testing.T, f *failedCheckpointFixture) (*db.Run, *config.Config) {
			if err := f.database.UpdateRunErrorStatus(f.source.ID, "uncertain terminal head", types.RunFailed); err != nil {
				t.Fatal(err)
			}
			return f.target(t, f.head, f.base, nil), f.cfg
		}},
		{name: "custody returned", mutate: func(t *testing.T, f *failedCheckpointFixture) (*db.Run, *config.Config) {
			if err := f.database.SetRunCustodyReturned(f.source.ID); err != nil {
				t.Fatal(err)
			}
			f.source, _ = f.database.GetRun(f.source.ID)
			return f.target(t, f.head, f.base, nil), f.cfg
		}},
		{name: "checkpoint absent", mutate: func(t *testing.T, f *failedCheckpointFixture) (*db.Run, *config.Config) {
			if err := f.database.DeleteValidationCheckpoint(f.source.ID); err != nil {
				t.Fatal(err)
			}
			return f.target(t, f.head, f.base, nil), f.cfg
		}},
		{name: "checkpoint malformed", mutate: func(t *testing.T, f *failedCheckpointFixture) (*db.Run, *config.Config) {
			checkpoint, err := f.database.GetValidationCheckpoint(f.source.ID)
			if err != nil {
				t.Fatal(err)
			}
			checkpoint.ConfigHash = "malformed"
			if err := f.database.PutValidationCheckpoint(checkpoint); err != nil {
				t.Fatal(err)
			}
			return f.target(t, f.head, f.base, nil), f.cfg
		}},
		{name: "step evidence changed", mutate: func(t *testing.T, f *failedCheckpointFixture) (*db.Run, *config.Config) {
			steps, _ := f.database.GetStepsByRun(f.source.ID)
			if err := f.database.SetStepFindings(steps[types.StepTest.Order()-1].ID, `{"findings":[{"description":"tampered"}]}`); err != nil {
				t.Fatal(err)
			}
			return f.target(t, f.head, f.base, nil), f.cfg
		}},
		{name: "log evidence changed", mutate: func(t *testing.T, f *failedCheckpointFixture) (*db.Run, *config.Config) {
			path := filepath.Join(f.p.RunLogDir(f.source.ID), string(types.StepTest)+".log")
			if err := os.WriteFile(path, []byte("tampered log\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return f.target(t, f.head, f.base, nil), f.cfg
		}},
		{name: "review authority changed", mutate: func(t *testing.T, f *failedCheckpointFixture) (*db.Run, *config.Config) {
			if err := f.database.UpdateRunReviewApprovedHeadSHA(f.source.ID, strings.Repeat("d", 40)); err != nil {
				t.Fatal(err)
			}
			return f.target(t, f.head, f.base, nil), f.cfg
		}},
		{name: "dirty target worktree", mutate: func(t *testing.T, f *failedCheckpointFixture) (*db.Run, *config.Config) {
			if err := os.WriteFile(filepath.Join(f.workDir, "late-agent-edit.txt"), []byte("dirty\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return f.target(t, f.head, f.base, nil), f.cfg
		}},
		{name: "multiple failed delivery steps", mutate: func(t *testing.T, f *failedCheckpointFixture) (*db.Run, *config.Config) {
			steps, err := f.database.GetStepsByRun(f.source.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.database.UpdateStepStatus(steps[types.StepPR.Order()-1].ID, types.StepStatusFailed); err != nil {
				t.Fatal(err)
			}
			return f.target(t, f.head, f.base, nil), f.cfg
		}},
		{name: "artifact evidence changed", mutate: func(t *testing.T, f *failedCheckpointFixture) (*db.Run, *config.Config) {
			dir := f.p.RunEvidenceDir(f.cfg.Test.Evidence.LocalRoot, f.source.ID)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "late.txt"), []byte("late evidence\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return f.target(t, f.head, f.base, nil), f.cfg
		}},
		{name: "newer superseding run exists", mutate: func(t *testing.T, f *failedCheckpointFixture) (*db.Run, *config.Config) {
			superseding := f.target(t, f.head, f.base, nil)
			if err := f.database.UpdateRunErrorStatus(superseding.ID, "newer failed attempt", types.RunFailed); err != nil {
				t.Fatal(err)
			}
			return f.target(t, f.head, f.base, nil), f.cfg
		}},
		{name: "concurrent run exists", mutate: func(t *testing.T, f *failedCheckpointFixture) (*db.Run, *config.Config) {
			_ = f.target(t, f.head, f.base, nil)
			return f.target(t, f.head, f.base, nil), f.cfg
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newFailedCheckpointFixture(t)
			target, cfg := tc.mutate(t, fixture)
			if source, err := PrepareValidationReuse(context.Background(), fixture.database, fixture.p, cfg, target, fixture.workDir); err == nil || source != "" {
				t.Fatalf("invalidated validation was reused from %q, error %v", source, err)
			}
			steps, err := fixture.database.GetStepsByRun(target.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(steps) != 0 {
				t.Fatalf("failed-closed target has %d prepared steps, want ordinary empty full-run state", len(steps))
			}
		})
	}
}

type dirtyFailingDeliveryStep struct{ name types.StepName }

func (s *dirtyFailingDeliveryStep) Name() types.StepName { return s.name }
func (s *dirtyFailingDeliveryStep) Execute(sctx *StepContext) (*StepOutcome, error) {
	if err := os.WriteFile(filepath.Join(sctx.WorkDir, "agent-edit.txt"), []byte("uncommitted\n"), 0o644); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("agent delivery repair failed")
}

func TestAgentAppliedDirtyChangeInvalidatesCheckpoint(t *testing.T) {
	fixture := newFailedCheckpointFixture(t)
	// Rearm the failed source and replace push with a delivery action that
	// leaves an uncommitted agent edit before failing again.
	steps, err := fixture.database.GetStepsByRun(fixture.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.database.UpdateRunStatus(fixture.source.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := fixture.database.UpdateStepStatus(steps[types.StepPush.Order()-1].ID, types.StepStatusPending); err != nil {
		t.Fatal(err)
	}
	execSteps := make([]Step, 0, len(types.AllSteps()))
	for _, name := range types.AllSteps() {
		if name == types.StepPush {
			execSteps = append(execSteps, &dirtyFailingDeliveryStep{name: name})
		} else {
			execSteps = append(execSteps, newPassStep(name))
		}
	}
	reloaded, _ := fixture.database.GetRun(fixture.source.ID)
	if err := NewExecutor(fixture.database, fixture.p, fixture.cfg, nil, execSteps, nil).ResumeDelivery(context.Background(), reloaded, fixture.repo, fixture.workDir); err == nil {
		t.Fatal("dirty delivery unexpectedly completed")
	}
	checkpoint, err := fixture.database.GetValidationCheckpoint(fixture.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint != nil {
		t.Fatal("dirty agent change left validation checkpoint reusable")
	}
}

func TestResumeDeliveryAfterCrashAtRecoverableBoundaries(t *testing.T) {
	tests := []struct {
		step   types.StepName
		status types.StepStatus
	}{
		{types.StepPush, types.StepStatusPending},
		{types.StepPush, types.StepStatusRunning},
		{types.StepPR, types.StepStatusPending},
		{types.StepPR, types.StepStatusRunning},
		{types.StepCI, types.StepStatusPending},
	}
	for _, tc := range tests {
		t.Run(string(tc.step)+"_"+string(tc.status), func(t *testing.T) {
			fixture := newFailedCheckpointFixture(t)
			steps, err := fixture.database.GetStepsByRun(fixture.source.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := fixture.database.UpdateRunStatus(fixture.source.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			for _, completed := range []types.StepName{types.StepPush, types.StepPR} {
				if completed.Order() >= tc.step.Order() {
					break
				}
				if err := fixture.database.UpdateStepStatus(steps[completed.Order()-1].ID, types.StepStatusCompleted); err != nil {
					t.Fatal(err)
				}
			}
			if err := fixture.database.UpdateStepStatus(steps[tc.step.Order()-1].ID, tc.status); err != nil {
				t.Fatal(err)
			}
			reloaded, _ := fixture.database.GetRun(fixture.source.ID)
			called := map[types.StepName]*mockStep{}
			execSteps := make([]Step, 0, len(types.AllSteps()))
			for _, name := range types.AllSteps() {
				step := newPassStep(name)
				called[name] = step
				execSteps = append(execSteps, step)
			}
			if err := NewExecutor(fixture.database, fixture.p, fixture.cfg, nil, execSteps, nil).ResumeDelivery(context.Background(), reloaded, fixture.repo, fixture.workDir); err != nil {
				t.Fatalf("ResumeDelivery() error = %v", err)
			}
			for _, name := range validationStepNames {
				if got := called[name].callCount(); got != 0 {
					t.Errorf("validation step %s ran %d times after crash", name, got)
				}
			}
			for _, name := range []types.StepName{types.StepPush, types.StepPR, types.StepCI} {
				want := 0
				if name.Order() >= tc.step.Order() {
					want = 1
				}
				if got := called[name].callCount(); got != want {
					t.Errorf("delivery step %s ran %d times, want %d", name, got, want)
				}
			}
		})
	}
}

func TestResumeDeliveryAfterCrashBetweenReusePersistenceAndExecute(t *testing.T) {
	fixture := newFailedCheckpointFixture(t)
	target := fixture.target(t, fixture.head, fixture.base, nil)
	if _, err := PrepareValidationReuse(context.Background(), fixture.database, fixture.p, fixture.cfg, target, fixture.workDir); err != nil {
		t.Fatal(err)
	}
	called := map[types.StepName]*mockStep{}
	var steps []Step
	for _, name := range types.AllSteps() {
		step := newPassStep(name)
		called[name] = step
		steps = append(steps, step)
	}
	if err := NewExecutor(fixture.database, fixture.p, fixture.cfg, nil, steps, nil).ResumeDelivery(context.Background(), target, fixture.repo, fixture.workDir); err != nil {
		t.Fatal(err)
	}
	for _, name := range validationStepNames {
		if got := called[name].callCount(); got != 0 {
			t.Errorf("validation step %s ran %d times after persisted reuse crash", name, got)
		}
	}
	for _, name := range []types.StepName{types.StepPush, types.StepPR, types.StepCI} {
		if got := called[name].callCount(); got != 1 {
			t.Errorf("delivery step %s ran %d times, want 1", name, got)
		}
	}
}

func TestPreparedRowsWithoutCheckpointRunFullValidation(t *testing.T) {
	fixture := newFailedCheckpointFixture(t)
	target := fixture.target(t, fixture.head, fixture.base, nil)
	if _, err := PrepareValidationReuse(context.Background(), fixture.database, fixture.p, fixture.cfg, target, fixture.workDir); err != nil {
		t.Fatal(err)
	}
	if err := fixture.database.DeleteValidationCheckpoint(target.ID); err != nil {
		t.Fatal(err)
	}
	called := map[types.StepName]*mockStep{}
	var steps []Step
	for _, name := range types.AllSteps() {
		step := newPassStep(name)
		if name == types.StepReview {
			step.outcome.ReviewApprovedHeadSHA = fixture.head
		}
		called[name] = step
		steps = append(steps, step)
	}
	if err := NewExecutor(fixture.database, fixture.p, fixture.cfg, nil, steps, nil).Execute(context.Background(), target, fixture.repo, fixture.workDir); err != nil {
		t.Fatal(err)
	}
	for _, name := range types.AllSteps() {
		if got := called[name].callCount(); got != 1 {
			t.Errorf("step %s executed %d times, want full validation", name, got)
		}
	}
}

func TestSkipPlanNeverCreatesReusableCheckpoint(t *testing.T) {
	database, p, _, repo := setupTest(t)
	workDir := t.TempDir()
	initGitRepo(t, workDir)
	head, _ := gitHeadForCheckpointTest(workDir)
	run, err := database.InsertRun(repo.ID, "skipped-delivery", head, head)
	if err != nil {
		t.Fatal(err)
	}
	var steps []Step
	for _, name := range types.AllSteps() {
		step := newPassStep(name)
		if name == types.StepReview {
			step.outcome.ReviewApprovedHeadSHA = head
		}
		steps = append(steps, step)
	}
	executor := NewExecutor(database, p, &config.Config{}, nil, steps, nil)
	executor.SetSkippedSteps([]types.StepName{types.StepPR})
	if err := executor.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := database.GetValidationCheckpoint(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint != nil {
		t.Fatal("run with altered skip plan created reusable validation")
	}
}

type dirtyPassingStep struct{ name types.StepName }

func (s *dirtyPassingStep) Name() types.StepName { return s.name }
func (s *dirtyPassingStep) Execute(sctx *StepContext) (*StepOutcome, error) {
	if err := os.WriteFile(filepath.Join(sctx.WorkDir, "uncommitted-lint-output.txt"), []byte("dirty\n"), 0o644); err != nil {
		return nil, err
	}
	return &StepOutcome{}, nil
}

func TestDirtyValidationBoundaryDoesNotCreateCheckpoint(t *testing.T) {
	database, p, _, repo := setupTest(t)
	workDir := t.TempDir()
	initGitRepo(t, workDir)
	head, _ := gitHeadForCheckpointTest(workDir)
	run, err := database.InsertRun(repo.ID, "dirty-boundary", head, head)
	if err != nil {
		t.Fatal(err)
	}
	var steps []Step
	for _, name := range types.AllSteps() {
		var step Step = newPassStep(name)
		if name == types.StepReview {
			step.(*mockStep).outcome.ReviewApprovedHeadSHA = head
		}
		if name == types.StepLint {
			step = &dirtyPassingStep{name: name}
		}
		steps = append(steps, step)
	}
	if err := NewExecutor(database, p, &config.Config{}, nil, steps, nil).Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := database.GetValidationCheckpoint(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint != nil {
		t.Fatal("dirty validation boundary created checkpoint")
	}
}

func TestFailedCheckpointRefreshDeletesOlderAuthority(t *testing.T) {
	fixture := newFailedCheckpointFixture(t)
	logPath := filepath.Join(fixture.p.RunLogDir(fixture.source.ID), string(types.StepTest)+".log")
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	if _, err := PersistValidationCheckpoint(context.Background(), fixture.database, fixture.p, fixture.cfg, fixture.source, fixture.workDir); err == nil {
		t.Fatal("stale checkpoint refresh unexpectedly succeeded")
	}
	checkpoint, err := fixture.database.GetValidationCheckpoint(fixture.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint != nil {
		t.Fatal("failed refresh retained older validation authority")
	}
}

func TestAuthoritativeRerunIntentHasStableHash(t *testing.T) {
	intent := "preserve these exact acceptance criteria"
	agentSource := db.RunIntentSourceAgent
	rerunSource := db.RunIntentSourceRerun
	first := &db.Run{Intent: &intent, IntentSource: &agentSource}
	retry := &db.Run{Intent: &intent, IntentSource: &rerunSource}
	if runIntentHash(first) != runIntentHash(retry) {
		t.Fatal("inherited authoritative intent changed checkpoint identity")
	}
}

func TestArtifactRootSymlinkFailsClosed(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "evidence")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := hashArtifactTree(context.Background(), link); err == nil {
		t.Fatal("symlink evidence root was accepted")
	}
}

func TestEvidenceHashAndCopyHonorCancellation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "evidence.txt"), []byte("evidence\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := hashArtifactTree(ctx, root); err == nil {
		t.Fatal("cancelled evidence hash completed")
	}
	if err := copyRegularTree(ctx, root, filepath.Join(t.TempDir(), "copy")); err == nil {
		t.Fatal("cancelled evidence copy completed")
	}
}

func TestResumeDeliveryFinalizesCompletedBoundaryAndRejectsVolatileCI(t *testing.T) {
	t.Run("all delivery complete", func(t *testing.T) {
		fixture := newFailedCheckpointFixture(t)
		results, _ := fixture.database.GetStepsByRun(fixture.source.ID)
		if err := fixture.database.UpdateRunStatus(fixture.source.ID, types.RunRunning); err != nil {
			t.Fatal(err)
		}
		for _, name := range []types.StepName{types.StepPush, types.StepPR, types.StepCI} {
			if err := fixture.database.UpdateStepStatus(results[name.Order()-1].ID, types.StepStatusCompleted); err != nil {
				t.Fatal(err)
			}
		}
		run, _ := fixture.database.GetRun(fixture.source.ID)
		if err := NewExecutor(fixture.database, fixture.p, fixture.cfg, nil, passingSteps(), nil).ResumeDelivery(context.Background(), run, fixture.repo, fixture.workDir); err != nil {
			t.Fatal(err)
		}
		if run.Status != types.RunCompleted {
			t.Fatalf("status = %s, want completed", run.Status)
		}
	})

	t.Run("running CI monitor", func(t *testing.T) {
		fixture := newFailedCheckpointFixture(t)
		results, _ := fixture.database.GetStepsByRun(fixture.source.ID)
		if err := fixture.database.UpdateRunStatus(fixture.source.ID, types.RunRunning); err != nil {
			t.Fatal(err)
		}
		for _, name := range []types.StepName{types.StepPush, types.StepPR} {
			if err := fixture.database.UpdateStepStatus(results[name.Order()-1].ID, types.StepStatusCompleted); err != nil {
				t.Fatal(err)
			}
		}
		if err := fixture.database.UpdateStepStatus(results[types.StepCI.Order()-1].ID, types.StepStatusRunning); err != nil {
			t.Fatal(err)
		}
		run, _ := fixture.database.GetRun(fixture.source.ID)
		if err := NewExecutor(fixture.database, fixture.p, fixture.cfg, nil, passingSteps(), nil).ResumeDelivery(context.Background(), run, fixture.repo, fixture.workDir); err == nil {
			t.Fatal("volatile running CI monitor resumed")
		}
	})

	t.Run("worktree dirtied after startup proof", func(t *testing.T) {
		fixture := newFailedCheckpointFixture(t)
		results, _ := fixture.database.GetStepsByRun(fixture.source.ID)
		if err := fixture.database.UpdateRunStatus(fixture.source.ID, types.RunRunning); err != nil {
			t.Fatal(err)
		}
		if err := fixture.database.UpdateStepStatus(results[types.StepPush.Order()-1].ID, types.StepStatusPending); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fixture.workDir, "late-drift.txt"), []byte("drift\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		run, _ := fixture.database.GetRun(fixture.source.ID)
		if err := NewExecutor(fixture.database, fixture.p, fixture.cfg, nil, passingSteps(), nil).ResumeDelivery(context.Background(), run, fixture.repo, fixture.workDir); err == nil {
			t.Fatal("dirty worktree resumed after startup proof")
		}
	})
}

func passingSteps() []Step {
	steps := make([]Step, 0, len(types.AllSteps()))
	for _, name := range types.AllSteps() {
		steps = append(steps, newPassStep(name))
	}
	return steps
}
