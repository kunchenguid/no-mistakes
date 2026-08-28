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

type sessionFallbackTimeoutAgent struct {
	calls int
}

func (a *sessionFallbackTimeoutAgent) Name() string { return "session-timeout" }

func (a *sessionFallbackTimeoutAgent) SupportsSessionResume() bool { return true }

func (a *sessionFallbackTimeoutAgent) Close() error { return nil }

func (a *sessionFallbackTimeoutAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	a.calls++
	if opts.Session != nil && opts.Session.ID != "" {
		opts.OnLifecycle(agent.LifecycleEvent{Agent: a.Name(), Phase: agent.LifecyclePhaseStart, PID: 7171})
		opts.OnLifecycle(agent.LifecycleEvent{Agent: a.Name(), Phase: agent.LifecyclePhaseActivity})
		return nil, fmt.Errorf("resume failed")
	}
	opts.OnLifecycle(agent.LifecycleEvent{Agent: a.Name(), Phase: agent.LifecyclePhaseStart, PID: 7272})
	<-ctx.Done()
	return nil, ctx.Err()
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

// TestReviewLoop_IndependentReviewTurnsOneFixerSession drives the real review
// step through the executor's auto-fix loop for multiple rounds and proves:
// every review turn (the initial review and every post-fix rereview) runs
// session-free, N fix rounds share ONE durable fixer session, the review
// turns never receive the fixer's identity, and every review round still asks
// for a full review pass of the branch.
//
// Review turns are deliberately session-free: round N's fixes implement round
// N-1's review findings, so resuming any prior review turn's session would
// seat the prescriber of those fixes as their certifier. The cross-round
// context a rereview legitimately needs travels in the explicit sanitized
// round-history prompt section instead.
func TestReviewLoop_IndependentReviewTurnsOneFixerSession(t *testing.T) {
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

	// Every review turn is session-free: no identity to start, none resumed.
	for i, call := range reviews {
		if call.Session != nil {
			t.Fatalf("review round %d must run session-free, got session %+v", i+1, call.Session)
		}
	}

	// One durable fixer session: started on the first fix turn, resumed on
	// the second.
	if fixes[0].Session == nil || fixes[0].Session.ID != "" {
		t.Fatalf("first fix must start the fixer session, got %+v", fixes[0].Session)
	}
	fixerID := "sess-1"
	if fixes[1].Session == nil || fixes[1].Session.ID != fixerID {
		t.Fatalf("second fix must resume %s, got %+v", fixerID, fixes[1].Session)
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

	// The persisted resume metadata is the minimum, and only the fixer role
	// has any: review turns never mint a durable identity.
	sessions, err := database.GetRunAgentSessions(run.ID)
	if err != nil {
		t.Fatalf("get sessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 persisted role session (fixer), got %d", len(sessions))
	}
	if sessions[0].Role != string(pipeline.SessionRoleFixer) || sessions[0].SessionID == "" || sessions[0].Agent != "session-mock" {
		t.Fatalf("unexpected persisted session: %+v", sessions[0])
	}
}

// TestReviewLoop_RereviewNeverResumesTheSessionThatPrescribedItsFixes is the
// class regression for the pipeline certifying its own fixes: an auto-fix
// round implements the findings the previous review turn prescribed, so the
// rereview that certifies the pipeline-authored result must not resume the
// session holding the prescriber's reasoning. Under the retired
// one-durable-reviewer-session design the rereview resumed exactly that
// session and degenerated into verifying that its own prescription had been
// implemented, letting a defect the pipeline itself introduced pass with zero
// findings.
func TestReviewLoop_RereviewNeverResumesTheSessionThatPrescribedItsFixes(t *testing.T) {
	reviewRound := 0
	mock := &sessionMockAgent{}
	mock.respond = func(opts agent.RunOpts) *agent.Result {
		switch opts.Purpose {
		case "review":
			reviewRound++
			if reviewRound == 1 {
				return &agent.Result{Output: []byte(
					`{"findings":[{"id":"f-1","severity":"error","description":"prescribed design","action":"auto-fix"}],"summary":"1 issue","risk_level":"medium","risk_rationale":"bug"}`,
				)}
			}
			return &agent.Result{Output: []byte(`{"findings":[],"summary":"clean","risk_level":"low","risk_rationale":"clean"}`)}
		case "review-fix":
			return &agent.Result{Output: []byte(`{"summary":"implement the prescription"}`)}
		default:
			t.Errorf("unexpected agent purpose %q", opts.Purpose)
			return &agent.Result{Output: []byte(`{}`)}
		}
	}

	exec, _, run, repo, workDir := reviewSessionHarness(t, mock, []pipeline.Step{&ReviewStep{}})
	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("execute: %v", err)
	}

	reviews := reviewCalls(mock.snapshot())
	if len(reviews) != 2 {
		t.Fatalf("expected initial review + rereview, got %d review calls", len(reviews))
	}
	if reviews[1].Session != nil {
		t.Fatalf("the rereview certifying the fix round must not carry any review-session identity, got %+v", reviews[1].Session)
	}
}

// TestReviewLoop_ParkRespondFixKeepsRoleSessions parks the review step at an
// ask-user gate, responds with a fix action, and proves the user-driven fix
// turn uses the durable fixer session while the follow-up full rereview stays
// session-free.
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
	if reviews[1].Session != nil {
		t.Fatalf("post-park rereview must run session-free, got %+v", reviews[1].Session)
	}
	if fixes[0].Session == nil || fixes[0].Session.ID != "" {
		t.Fatalf("user-driven fix must start the fixer session, got %+v", fixes[0].Session)
	}
	if !strings.Contains(reviews[1].Prompt, "Do a full review pass before returning") {
		t.Fatalf("post-fix rereview lost the full-review demand:\n%s", reviews[1].Prompt)
	}
}

func TestReviewFixerSession_FreshFallbackTimeoutExcludesResumeActivity(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &sessionFallbackTimeoutAgent{}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.ReviewAgentTimeout = 50 * time.Millisecond
	sctx.Config.SessionReuse = true
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"f-1","severity":"error","description":"fix this","action":"auto-fix"}]}`
	if err := sctx.DB.UpsertRunAgentSession(sctx.Run.ID, string(pipeline.SessionRoleFixer), ag.Name(), "stale-session"); err != nil {
		t.Fatalf("store fixer session: %v", err)
	}
	sctx.Sessions = pipeline.NewRunSessions(sctx.DB, sctx.Run.ID, ag, true)

	_, err := (&ReviewStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected the fresh fixer session to time out")
	}
	if ag.calls != 2 {
		t.Fatalf("agent calls = %d, want failed resume and fresh fallback", ag.calls)
	}
	if !strings.Contains(err.Error(), "produced no output at all") {
		t.Fatalf("error = %q, want fresh fallback silence", err)
	}
	if strings.Contains(err.Error(), "last produced output") {
		t.Fatalf("error = %q, dead resume activity must not describe the fresh fallback", err)
	}
	if !strings.Contains(err.Error(), "pid=7272") {
		t.Fatalf("error = %q, want silence attributed to the fresh fixer subprocess", err)
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
