package steps

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// sessionMockAgent is a scripted agent for review-loop tests. The historical
// session fields were removed with the reviewer/fixer split; this build runs
// every turn cold, so the fake only records calls and returns the scripted
// result.
type sessionMockAgent struct {
	mu      sync.Mutex
	calls   []agent.RunOpts
	respond func(opts agent.RunOpts) *agent.Result
}

func (m *sessionMockAgent) Name() string { return "session-mock" }

func (m *sessionMockAgent) Close() error { return nil }

func (m *sessionMockAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, opts)
	return m.respond(opts), nil
}

func (m *sessionMockAgent) snapshot() []agent.RunOpts {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]agent.RunOpts(nil), m.calls...)
}

// reviewSessionHarness wires a real executor around real steps with a scripted
// agent and a real git worktree.
func reviewSessionHarness(t *testing.T, mock *sessionMockAgent, steps []pipeline.Step, tweaks ...func(*config.Config)) (*pipeline.Executor, *db.DB, *db.Run, *db.Repo, string) {
	t.Helper()
	workDir, baseSHA, headSHA := setupGitRepo(t)
	ensureHermeticOrigin(t, workDir)

	database, err := db.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	repo, err := database.InsertRepo(workDir, "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := database.InsertRun(repo.ID, "refs/heads/feature", headSHA, baseSHA)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}

	cfg := &config.Config{
		Agent:   types.AgentOMP,
		AutoFix: config.AutoFix{Review: 3},
	}
	for _, tweak := range tweaks {
		tweak(cfg)
	}
	exec := pipeline.NewExecutor(database, paths.WithRoot(t.TempDir()), cfg, mock, steps, nil)
	return exec, database, run, repo, workDir
}

func reviewCalls(calls []agent.RunOpts) []agent.RunOpts {
	var out []agent.RunOpts
	for _, c := range calls {
		if c.Purpose == "review" {
			out = append(out, c)
		}
	}
	return out
}

func fixCalls(calls []agent.RunOpts) []agent.RunOpts {
	var out []agent.RunOpts
	for _, c := range calls {
		if c.Purpose == "review-fix" {
			out = append(out, c)
		}
	}
	return out
}

func waitForReviewStatus(t *testing.T, database *db.DB, runID string, want types.StepStatus) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		steps, err := database.GetStepsByRun(runID)
		if err != nil {
			t.Fatalf("get steps: %v", err)
		}
		for _, step := range steps {
			if step.StepName == types.StepReview && step.Status == want {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("review did not reach status %s", want)
}
