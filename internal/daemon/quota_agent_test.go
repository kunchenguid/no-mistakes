package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/quota"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// recordedReader serves a report a test can change between selection points,
// standing in for the tool's live read.
type recordedReader struct {
	report *quota.Report
	err    error
	calls  int
}

func (r *recordedReader) Read(context.Context) (quota.Report, error) {
	r.calls++
	if r.err != nil {
		return quota.Report{}, r.err
	}
	return *r.report, nil
}

func useRecordedEvidence(t *testing.T, report *quota.Report) *recordedReader {
	t.Helper()
	reader := &recordedReader{report: report}
	restore := newQuotaEvidenceReader
	newQuotaEvidenceReader = func(string) quota.Reader { return reader }
	t.Cleanup(func() { newQuotaEvidenceReader = restore })
	return reader
}

// quotaReport builds the evidence shape the routing tests work over.
func quotaReport(claudePercent int, claudeRunway string, codexPercent int, codexRunway string) *quota.Report {
	seconds := func(value int) *int { return &value }
	percent := func(value int) *int { return &value }
	priority := func(value float64) *float64 { return &value }
	row := func(id string, remaining int, spend float64, runway string, runwaySeconds int) quota.ModelEvidence {
		return quota.ModelEvidence{
			ID:               id,
			Label:            id,
			Intelligence:     quota.ClassHigh,
			Measurable:       true,
			Scope:            "all_models",
			PercentRemaining: percent(remaining),
			SpendPriority:    priority(spend),
			RunwayStatus:     runway,
			RunwaySeconds:    seconds(runwaySeconds),
		}
	}
	return &quota.Report{
		GeneratedAt: time.Unix(1_790_000_000, 0).UTC(),
		Providers: map[string]quota.ProviderEvidence{
			"claude": {Provider: "claude", State: "fresh", Models: []quota.ModelEvidence{
				row("claude-sonnet-4-5", claudePercent, 0.5, claudeRunway, 8000),
			}},
			"codex": {Provider: "codex", State: "fresh", Models: []quota.ModelEvidence{
				row("gpt-5.1-codex", codexPercent, 0.2, codexRunway, 4000),
			}},
		},
	}
}

func quotaAutoFixture(t *testing.T) (*RunManager, *db.DB, *db.Run) {
	t.Helper()
	p := paths.WithRoot(t.TempDir())
	database, err := db.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepo("/tmp/quota-repo", "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := database.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	return NewRunManager(database, p, nil), database, run
}

func quotaAutoConfig() *config.Config {
	return &config.Config{
		Agent:           types.AgentQuotaAuto,
		Agents:          []types.AgentName{types.AgentQuotaAuto},
		AgentCandidates: []string{"claude", "codex"},
		QuotaAXIPath:    "/opt/tools/quota-axi",
		AgentTimeout:    30 * time.Minute,
	}
}

// TestNewRunPipelineAgent_QuotaAutoPicksTheBetterStandingProvider is the feature's
// contract end to end inside the daemon: two candidates with different measured
// standing resolve to the better one, and the run records the choice with the
// evidence behind it.
func TestNewRunPipelineAgent_QuotaAutoPicksTheBetterStandingProvider(t *testing.T) {
	m, database, run := quotaAutoFixture(t)
	reader := useRecordedEvidence(t, quotaReport(88, quota.RunwayThroughReset, 0, quota.RunwayExhaustedNow))

	ag, boundary, err := m.newRunPipelineAgent(context.Background(), quotaAutoConfig(), run.ID, types.StepReview, t.TempDir(), fakeLookPath, runenv.Overlay{})
	if err != nil {
		t.Fatalf("newRunPipelineAgent: %v", err)
	}
	defer ag.Close()
	if ag.Name() != string(types.AgentClaude) {
		t.Fatalf("agent = %q, want the provider with headroom", ag.Name())
	}
	if boundary == nil {
		t.Fatal("a quota-auto run must get a step-boundary hook")
	}

	selections, err := database.ListAgentSelections(run.ID)
	if err != nil {
		t.Fatalf("list selections: %v", err)
	}
	if len(selections) != 1 {
		t.Fatalf("selections = %d, want the opening selection recorded", len(selections))
	}
	opening := selections[0]
	if opening.Agent != "claude" || opening.Provider != "claude" || opening.Reason != quota.ReasonRunStart || opening.Step != string(types.StepReview) {
		t.Fatalf("opening selection = %+v", opening)
	}
	for _, want := range []string{`"model":"claude-sonnet-4-5"`, `"verdict":"eligible"`, `"candidate":"codex@codex"`, "quota exhausted now"} {
		if !strings.Contains(opening.Evidence, want) {
			t.Fatalf("evidence missing %q:\n%s", want, opening.Evidence)
		}
	}
	if opening.ReportAt == nil || *opening.ReportAt != 1_790_000_000 {
		t.Fatalf("report time = %v", opening.ReportAt)
	}

	// The provider runs dry mid-run: the boundary moves the run onto the other
	// candidate instead of stranding the steps behind it.
	*reader.report = *quotaReport(0, quota.RunwayExhaustedNow, 70, quota.RunwayThroughReset)
	if err := boundary(context.Background(), types.StepPush); err != nil {
		t.Fatalf("boundary: %v", err)
	}
	if ag.Name() != string(types.AgentCodex) {
		t.Fatalf("agent = %q, want the switch to codex", ag.Name())
	}
	selections, err = database.ListAgentSelections(run.ID)
	if err != nil {
		t.Fatalf("list selections: %v", err)
	}
	if len(selections) != 2 {
		t.Fatalf("selections = %d, want the switch recorded", len(selections))
	}
	if selections[1].Agent != "codex" || selections[1].Reason != quota.ReasonStepBoundary || selections[1].Step != string(types.StepPush) {
		t.Fatalf("switch selection = %+v", selections[1])
	}
	if reader.calls != 2 {
		t.Fatalf("evidence reads = %d, want one per selection point", reader.calls)
	}
}

// TestNewRunPipelineAgent_QuotaAutoFailsClosedWithTheReport proves an unroutable
// run reports every candidate instead of starting on a guess.
func TestNewRunPipelineAgent_QuotaAutoFailsClosedWithTheReport(t *testing.T) {
	m, database, run := quotaAutoFixture(t)
	useRecordedEvidence(t, quotaReport(0, quota.RunwayExhaustedNow, 0, quota.RunwayExhaustedNow))

	_, _, err := m.newRunPipelineAgent(context.Background(), quotaAutoConfig(), run.ID, types.StepReview, t.TempDir(), fakeLookPath, runenv.Overlay{})
	if err == nil {
		t.Fatal("expected the selection to fail")
	}
	for _, want := range []string{"claude@claude", "codex@codex", "quota exhausted now", "no eligible agent"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q: %v", want, err)
		}
	}
	selections, listErr := database.ListAgentSelections(run.ID)
	if listErr != nil {
		t.Fatalf("list selections: %v", listErr)
	}
	if len(selections) != 0 {
		t.Fatalf("a failed selection recorded %d rows, want none", len(selections))
	}
}

// TestNewRunPipelineAgent_QuotaAutoSurfacesAnUnreadableTool proves a run does not
// start when the evidence cannot be read at all: unlike a boundary, the opening
// choice has no harness to fall back on.
func TestNewRunPipelineAgent_QuotaAutoSurfacesAnUnreadableTool(t *testing.T) {
	m, _, run := quotaAutoFixture(t)
	reader := useRecordedEvidence(t, quotaReport(88, quota.RunwayThroughReset, 0, quota.RunwayExhaustedNow))
	reader.err = errors.New(`exec: "quota-axi": executable file not found in $PATH`)

	_, _, err := m.newRunPipelineAgent(context.Background(), quotaAutoConfig(), run.ID, types.StepReview, t.TempDir(), fakeLookPath, runenv.Overlay{})
	if err == nil || !strings.Contains(err.Error(), "executable file not found") {
		t.Fatalf("error = %v, want the read failure", err)
	}
}

// TestNewRunPipelineAgent_QuotaAutoIsInertForEveryOtherMode proves the feature is
// opt-in: an ordinary configuration keeps today's construction, with no hook and
// no evidence read.
func TestNewRunPipelineAgent_QuotaAutoIsInertForEveryOtherMode(t *testing.T) {
	m, _, run := quotaAutoFixture(t)
	reader := useRecordedEvidence(t, quotaReport(88, quota.RunwayThroughReset, 0, quota.RunwayExhaustedNow))

	cfg := &config.Config{Agent: types.AgentClaude, Agents: []types.AgentName{types.AgentClaude}}
	ag, boundary, err := m.newRunPipelineAgent(context.Background(), cfg, run.ID, types.StepReview, t.TempDir(), fakeLookPath, runenv.Overlay{})
	if err != nil {
		t.Fatalf("newRunPipelineAgent: %v", err)
	}
	defer ag.Close()
	if boundary != nil {
		t.Fatal("a pinned agent needs no step-boundary hook")
	}
	if reader.calls != 0 {
		t.Fatalf("evidence reads = %d, want none outside quota-auto", reader.calls)
	}
	if ag.Name() != string(types.AgentClaude) {
		t.Fatalf("agent = %q", ag.Name())
	}
}

// TestNextStepForRun_AnswersTheStepAResumingRunNeeds proves recovery selects for
// the step it is about to re-enter rather than for one it finished before the
// crash: the two ask different things of a candidate.
func TestNextStepForRun_AnswersTheStepAResumingRunNeeds(t *testing.T) {
	_, database, run := quotaAutoFixture(t)
	plan := []pipeline.Step{
		namedStep{types.StepReview},
		namedStep{types.StepTest},
		namedStep{types.StepPush},
	}
	ids := map[types.StepName]string{}
	for _, step := range plan {
		result, err := database.InsertStepResult(run.ID, step.Name())
		if err != nil {
			t.Fatalf("insert step result: %v", err)
		}
		ids[step.Name()] = result.ID
	}

	if got := nextStepForRun(database, run.ID, plan); got != types.StepReview {
		t.Fatalf("fresh run answered %q, want its first step", got)
	}

	// The run crashed after review completed.
	if err := database.CompleteStepWithStatus(ids[types.StepReview], types.StepStatusCompleted, 0, 0, ""); err != nil {
		t.Fatalf("complete step: %v", err)
	}
	if got := nextStepForRun(database, run.ID, plan); got != types.StepTest {
		t.Fatalf("resumed run answered %q, want the step it re-enters", got)
	}

	// A run whose steps are all finished has no step to satisfy; the answer stays
	// deterministic rather than empty.
	for _, step := range []types.StepName{types.StepTest, types.StepPush} {
		if err := database.CompleteStepWithStatus(ids[step], types.StepStatusCompleted, 0, 0, ""); err != nil {
			t.Fatalf("complete step: %v", err)
		}
	}
	if got := nextStepForRun(database, run.ID, plan); got != types.StepReview {
		t.Fatalf("finished run answered %q, want the conservative first step", got)
	}

	// An unreadable plan is the same conservative answer.
	if got := nextStepForRun(database, "run-missing", plan); got != types.StepReview {
		t.Fatalf("unknown run answered %q, want the conservative first step", got)
	}
	if got := nextStepForRun(database, run.ID, nil); got != "" {
		t.Fatalf("empty plan answered %q, want no step", got)
	}
}

// namedStep is the minimum of pipeline.Step that nextStepForRun reads.
type namedStep struct{ name types.StepName }

func (s namedStep) Name() types.StepName { return s.name }

func (s namedStep) Execute(*pipeline.StepContext) (*pipeline.StepOutcome, error) {
	return &pipeline.StepOutcome{}, nil
}
