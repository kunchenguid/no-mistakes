package steps

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
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

// sessionMockAgent is a session-capable scripted agent for review-loop tests.
// It mints deterministic session ids ("sess-1", "sess-2", ...) for new
// sessions and echoes resumed ids, recording every invocation.
type sessionMockAgent struct {
	mu     sync.Mutex
	calls  []agent.RunOpts
	nextID int
	// respond picks the reply for one invocation (called under the lock).
	respond func(opts agent.RunOpts) *agent.Result
}

func (m *sessionMockAgent) Name() string { return "session-mock" }

func (m *sessionMockAgent) SupportsSessionResume() bool { return true }

func (m *sessionMockAgent) Close() error { return nil }

func (m *sessionMockAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, opts)

	result := m.respond(opts)
	if opts.Session != nil {
		if opts.Session.ID != "" {
			result.SessionID = opts.Session.ID
			result.Resumed = true
		} else {
			m.nextID++
			result.SessionID = fmt.Sprintf("sess-%d", m.nextID)
		}
	}
	return result, nil
}

func (m *sessionMockAgent) snapshot() []agent.RunOpts {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]agent.RunOpts(nil), m.calls...)
}

// reviewSessionHarness wires a real executor around real steps with a
// session-capable mock agent and real git worktree.
func reviewSessionHarness(t *testing.T, mock *sessionMockAgent, steps []pipeline.Step) (*pipeline.Executor, *db.DB, *db.Run, *db.Repo, string) {
	t.Helper()
	workDir, baseSHA, headSHA := setupGitRepo(t)

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
		Agent:        types.AgentClaude,
		AutoFix:      config.AutoFix{Review: 3},
		SessionReuse: true,
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

// TestReviewLoop_OneReviewerSessionOneFixerSession drives the real review
// step through the executor's auto-fix loop for multiple rounds and proves:
// N review rounds share ONE reviewer session (started once, resumed after),
// N fix rounds share ONE separate fixer session, the two roles never
// exchange identities, and every review round still asks for a full review
// pass of the branch.
func TestReviewLoop_OneReviewerSessionOneFixerSession(t *testing.T) {
	reviewRound := 0
	mock := &sessionMockAgent{}
	mock.respond = func(opts agent.RunOpts) *agent.Result {
		switch opts.Purpose {
		case "review":
			reviewRound++
			if reviewRound <= 2 {
				return &agent.Result{Output: []byte(fmt.Sprintf(
					`{"findings":[{"id":"f-%d","severity":"error","description":"bug %d","action":"auto-fix"}],"summary":"issues","risk_level":"medium","risk_rationale":"bugs"}`,
					reviewRound, reviewRound,
				))}
			}
			return &agent.Result{Output: []byte(`{"findings":[],"summary":"clean","risk_level":"low","risk_rationale":"clean"}`)}
		case "review-fix":
			return &agent.Result{Output: []byte(`{"summary":"fix the bug"}`)}
		default:
			t.Errorf("unexpected agent purpose %q", opts.Purpose)
			return &agent.Result{Output: []byte(`{}`)}
		}
	}

	exec, database, run, repo, workDir := reviewSessionHarness(t, mock, []pipeline.Step{&ReviewStep{}})
	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("execute: %v", err)
	}

	calls := mock.snapshot()
	reviews := reviewCalls(calls)
	fixes := fixCalls(calls)
	if len(reviews) != 3 {
		t.Fatalf("expected 3 review rounds, got %d", len(reviews))
	}
	if len(fixes) != 2 {
		t.Fatalf("expected 2 fix rounds, got %d", len(fixes))
	}

	// One reviewer session: started on round 1, resumed on rounds 2 and 3.
	if reviews[0].Session == nil || reviews[0].Session.ID != "" {
		t.Fatalf("round 1 review must start the reviewer session, got %+v", reviews[0].Session)
	}
	reviewerID := "sess-1"
	for i, call := range reviews[1:] {
		if call.Session == nil || call.Session.ID != reviewerID {
			t.Fatalf("review round %d must resume %s, got %+v", i+2, reviewerID, call.Session)
		}
	}

	// One fixer session, distinct from the reviewer's: started on the first
	// fix turn, resumed on the second.
	if fixes[0].Session == nil || fixes[0].Session.ID != "" {
		t.Fatalf("first fix must start the fixer session, got %+v", fixes[0].Session)
	}
	fixerID := "sess-2"
	if fixes[1].Session == nil || fixes[1].Session.ID != fixerID {
		t.Fatalf("second fix must resume %s, got %+v", fixerID, fixes[1].Session)
	}
	if fixerID == reviewerID {
		t.Fatal("fixer and reviewer must have distinct sessions")
	}
	for i, call := range reviews {
		if call.Session != nil && call.Session.ID == fixerID {
			t.Fatalf("review round %d received the fixer's session identity", i+1)
		}
	}
	for i, call := range fixes {
		if call.Session != nil && call.Session.ID == reviewerID {
			t.Fatalf("fix round %d received the reviewer's session identity", i+1)
		}
	}

	// Every review round, including rereviews inside the resumed session,
	// still demands a full adversarial pass over the branch.
	for i, call := range reviews {
		if !strings.Contains(call.Prompt, "Do a full review pass before returning") {
			t.Fatalf("review round %d prompt lost the full-review demand:\n%s", i+1, call.Prompt)
		}
		if !strings.Contains(call.Prompt, "Review the code changes") {
			t.Fatalf("review round %d prompt is not a full review prompt:\n%s", i+1, call.Prompt)
		}
	}

	// The persisted resume metadata is the minimum: run, role, agent, id.
	sessions, err := database.GetRunAgentSessions(run.ID)
	if err != nil {
		t.Fatalf("get sessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected 2 persisted role sessions, got %d", len(sessions))
	}
	for _, s := range sessions {
		if s.SessionID == "" || s.Agent != "session-mock" {
			t.Fatalf("unexpected persisted session: %+v", s)
		}
	}
}

// TestReviewLoop_ParkRespondFixKeepsRoleSessions parks the review step at an
// ask-user gate, responds with a fix action, and proves the user-driven fix
// turn and the follow-up full rereview keep their role sessions.
func TestReviewLoop_ParkRespondFixKeepsRoleSessions(t *testing.T) {
	reviewRound := 0
	mock := &sessionMockAgent{}
	mock.respond = func(opts agent.RunOpts) *agent.Result {
		switch opts.Purpose {
		case "review":
			reviewRound++
			if reviewRound == 1 {
				return &agent.Result{Output: []byte(
					`{"findings":[{"id":"f-1","severity":"error","description":"needs decision","action":"ask-user"}],"summary":"1 issue","risk_level":"high","risk_rationale":"gate"}`,
				)}
			}
			return &agent.Result{Output: []byte(`{"findings":[],"summary":"clean","risk_level":"low","risk_rationale":"clean"}`)}
		default:
			return &agent.Result{Output: []byte(`{"summary":"apply decision"}`)}
		}
	}

	exec, database, run, repo, workDir := reviewSessionHarness(t, mock, []pipeline.Step{&ReviewStep{}})
	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForReviewStatus(t, database, run.ID, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionFix, []string{"f-1"}); err != nil {
		t.Fatalf("respond: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("executor timed out")
	}

	calls := mock.snapshot()
	reviews := reviewCalls(calls)
	fixes := fixCalls(calls)
	if len(reviews) != 2 || len(fixes) != 1 {
		t.Fatalf("expected 2 reviews + 1 fix, got %d + %d", len(reviews), len(fixes))
	}
	if reviews[1].Session == nil || reviews[1].Session.ID != "sess-1" {
		t.Fatalf("post-park rereview must resume the reviewer session, got %+v", reviews[1].Session)
	}
	if fixes[0].Session == nil || fixes[0].Session.ID != "" {
		t.Fatalf("user-driven fix must start the fixer session, got %+v", fixes[0].Session)
	}
	if !strings.Contains(reviews[1].Prompt, "Do a full review pass before returning") {
		t.Fatalf("post-fix rereview lost the full-review demand:\n%s", reviews[1].Prompt)
	}
}

// TestReviewLoop_OtherStepsStaySessionIsolated proves the reviewer/fixer
// sessions are never lent to other pipeline steps: agent-driven document and
// lint work runs with no session at all.
func TestReviewLoop_OtherStepsStaySessionIsolated(t *testing.T) {
	mock := &sessionMockAgent{}
	mock.respond = func(opts agent.RunOpts) *agent.Result {
		switch opts.Purpose {
		case "review":
			return &agent.Result{Output: []byte(`{"findings":[],"summary":"clean","risk_level":"low","risk_rationale":"clean"}`)}
		default:
			return &agent.Result{Output: []byte(`{"findings":[],"summary":"nothing to do"}`)}
		}
	}

	steps := []pipeline.Step{&ReviewStep{}, &DocumentStep{}, &LintStep{}}
	exec, _, run, repo, workDir := reviewSessionHarness(t, mock, steps)
	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("execute: %v", err)
	}

	for _, call := range mock.snapshot() {
		if call.Purpose == "review" || call.Purpose == "review-fix" {
			continue
		}
		if call.Session != nil {
			t.Fatalf("non-review invocation (purpose %q) must stay session-isolated, got session %+v", call.Purpose, call.Session)
		}
	}
}

func waitForReviewStatus(t *testing.T, database *db.DB, runID string, want types.StepStatus) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		steps, err := database.GetStepsByRun(runID)
		if err == nil {
			for _, s := range steps {
				if s.StepName == types.StepReview && s.Status == want {
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("review step never reached status %q", want)
}
