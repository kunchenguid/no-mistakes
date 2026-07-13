package pipeline

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/db"
)

// sessionCall records one invocation the fake adapter received.
type sessionCall struct {
	prompt   string
	session  *agent.SessionRef
	fallback bool
}

// fakeSessionAgent is a session-capable adapter that mints deterministic
// session ids and can be scripted to fail resume attempts.
type fakeSessionAgent struct {
	mu           sync.Mutex
	name         string
	calls        []sessionCall
	nextID       int
	failResumes  map[string]error // session id -> error returned when resumed
	failNext     error            // error returned on the next call regardless
	supportsFlag bool
}

func newFakeSessionAgent() *fakeSessionAgent {
	return &fakeSessionAgent{supportsFlag: true, failResumes: map[string]error{}}
}

func (f *fakeSessionAgent) Name() string {
	if f.name != "" {
		return f.name
	}
	return "fake"
}

func (f *fakeSessionAgent) SupportsSessionResume() bool { return f.supportsFlag }

func (f *fakeSessionAgent) Close() error { return nil }

func (f *fakeSessionAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, sessionCall{prompt: opts.Prompt, session: opts.Session, fallback: opts.SessionFallback})

	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		return nil, err
	}

	if opts.Session == nil {
		return &agent.Result{Text: "cold"}, nil
	}
	if opts.Session.ID != "" {
		if err := f.failResumes[opts.Session.ID]; err != nil {
			return nil, err
		}
		return &agent.Result{Text: "resumed", SessionID: opts.Session.ID, Resumed: true}, nil
	}
	f.nextID++
	return &agent.Result{Text: "started", SessionID: fmt.Sprintf("sess-%d", f.nextID)}, nil
}

func sessionTestDB(t *testing.T) (*db.DB, *db.Run) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	repo, err := d.InsertRepo("/tmp/repo", "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := d.InsertRun(repo.ID, "feature/x", "head", "base")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	return d, run
}

// TestRunSessions_ReviewerReusesOneSession proves N review rounds share one
// durable reviewer session: the first turn starts it, every later turn
// resumes the same identity.
func TestRunSessions_ReviewerReusesOneSession(t *testing.T) {
	d, run := sessionTestDB(t)
	fake := newFakeSessionAgent()
	rs := NewRunSessions(d, run.ID, fake, true)

	for i := 0; i < 4; i++ {
		if _, err := rs.Run(context.Background(), fake, SessionRoleReviewer, agent.RunOpts{Prompt: fmt.Sprintf("review round %d", i+1)}, nil); err != nil {
			t.Fatalf("round %d: %v", i+1, err)
		}
	}

	if len(fake.calls) != 4 {
		t.Fatalf("agent invoked %d times, want 4", len(fake.calls))
	}
	first := fake.calls[0]
	if first.session == nil || first.session.ID != "" {
		t.Fatalf("first turn must start a new session, got %+v", first.session)
	}
	for i, call := range fake.calls[1:] {
		if call.session == nil || call.session.ID != "sess-1" {
			t.Fatalf("turn %d must resume sess-1, got %+v", i+2, call.session)
		}
	}
}

// TestRunSessions_FixerSessionIsDistinctFromReviewer proves the fixer role
// keeps its own durable session that is never the reviewer's, in both
// directions and across interleaved turns.
func TestRunSessions_FixerSessionIsDistinctFromReviewer(t *testing.T) {
	d, run := sessionTestDB(t)
	fake := newFakeSessionAgent()
	rs := NewRunSessions(d, run.ID, fake, true)

	// review -> fix -> rereview -> fix -> rereview
	turns := []SessionRole{SessionRoleReviewer, SessionRoleFixer, SessionRoleReviewer, SessionRoleFixer, SessionRoleReviewer}
	for i, role := range turns {
		if _, err := rs.Run(context.Background(), fake, role, agent.RunOpts{Prompt: fmt.Sprintf("turn %d", i)}, nil); err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
	}

	reviewerIDs := map[string]bool{}
	fixerIDs := map[string]bool{}
	for i, call := range fake.calls {
		id := ""
		if call.session != nil {
			id = call.session.ID
		}
		switch turns[i] {
		case SessionRoleReviewer:
			if id != "" {
				reviewerIDs[id] = true
			}
		case SessionRoleFixer:
			if id != "" {
				fixerIDs[id] = true
			}
		}
	}
	if len(reviewerIDs) != 1 || !reviewerIDs["sess-1"] {
		t.Fatalf("reviewer must resume exactly one session, got %v", reviewerIDs)
	}
	if len(fixerIDs) != 1 || !fixerIDs["sess-2"] {
		t.Fatalf("fixer must resume exactly one distinct session, got %v", fixerIDs)
	}
	for id := range reviewerIDs {
		if fixerIDs[id] {
			t.Fatalf("reviewer and fixer shared session %s", id)
		}
	}
}

// TestRunSessions_ResumeFailureFallsBackToFreshSameRoleSession proves a
// failed resume never skips the turn: the stored identity is dropped and a
// fresh same-role session runs instead, marked as the fallback.
func TestRunSessions_ResumeFailureFallsBackToFreshSameRoleSession(t *testing.T) {
	d, run := sessionTestDB(t)
	fake := newFakeSessionAgent()
	rs := NewRunSessions(d, run.ID, fake, true)

	if _, err := rs.Run(context.Background(), fake, SessionRoleReviewer, agent.RunOpts{Prompt: "initial review"}, nil); err != nil {
		t.Fatalf("initial: %v", err)
	}
	fake.failResumes["sess-1"] = errors.New("session not found")

	result, err := rs.Run(context.Background(), fake, SessionRoleReviewer, agent.RunOpts{Prompt: "rereview"}, nil)
	if err != nil {
		t.Fatalf("rereview must fall back, got error: %v", err)
	}
	if result.SessionID != "sess-2" {
		t.Fatalf("fallback must start a fresh session, got %q", result.SessionID)
	}

	last := fake.calls[len(fake.calls)-1]
	if last.session == nil || last.session.ID != "" || !last.fallback {
		t.Fatalf("fallback call must start fresh and be marked, got %+v", last)
	}
	if last.prompt != "rereview" {
		t.Fatalf("fallback must re-run the same turn, got prompt %q", last.prompt)
	}

	// The next turn resumes the replacement session, not the dead one.
	if _, err := rs.Run(context.Background(), fake, SessionRoleReviewer, agent.RunOpts{Prompt: "third"}, nil); err != nil {
		t.Fatalf("third: %v", err)
	}
	third := fake.calls[len(fake.calls)-1]
	if third.session == nil || third.session.ID != "sess-2" {
		t.Fatalf("post-fallback turn must resume sess-2, got %+v", third.session)
	}
}

// TestRunSessions_FreshSessionFailurePropagates proves a failure that was not
// a resume (nothing to fall back from) surfaces to the caller unchanged.
func TestRunSessions_FreshSessionFailurePropagates(t *testing.T) {
	d, run := sessionTestDB(t)
	fake := newFakeSessionAgent()
	fake.failNext = errors.New("provider down")
	rs := NewRunSessions(d, run.ID, fake, true)

	_, err := rs.Run(context.Background(), fake, SessionRoleReviewer, agent.RunOpts{Prompt: "review"}, nil)
	if err == nil || err.Error() != "provider down" {
		t.Fatalf("fresh-session failure must propagate, got %v", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("no fallback retry expected for fresh-session failure, got %d calls", len(fake.calls))
	}
}

// TestRunSessions_CancelledContextDoesNotRetry proves cancellation is not
// treated as a resume failure worth a fallback invocation.
func TestRunSessions_CancelledContextDoesNotRetry(t *testing.T) {
	d, run := sessionTestDB(t)
	fake := newFakeSessionAgent()
	rs := NewRunSessions(d, run.ID, fake, true)

	if _, err := rs.Run(context.Background(), fake, SessionRoleReviewer, agent.RunOpts{Prompt: "initial"}, nil); err != nil {
		t.Fatalf("initial: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fake.failResumes["sess-1"] = context.Canceled

	_, err := rs.Run(ctx, fake, SessionRoleReviewer, agent.RunOpts{Prompt: "rereview"}, nil)
	if err == nil {
		t.Fatal("cancelled run must fail")
	}
	if len(fake.calls) != 2 {
		t.Fatalf("cancelled resume must not spawn a fallback invocation, got %d calls", len(fake.calls))
	}
}

// TestRunSessions_ColdWhenAgentLacksSessions proves adapters without session
// support run cold (no Session in opts) and correctness is preserved.
func TestRunSessions_ColdWhenAgentLacksSessions(t *testing.T) {
	d, run := sessionTestDB(t)
	fake := newFakeSessionAgent()
	fake.supportsFlag = false
	rs := NewRunSessions(d, run.ID, fake, true)

	result, err := rs.Run(context.Background(), fake, SessionRoleReviewer, agent.RunOpts{Prompt: "review"}, nil)
	if err != nil {
		t.Fatalf("cold run: %v", err)
	}
	if result.Text != "cold" {
		t.Fatalf("expected cold invocation, got %q", result.Text)
	}
	if fake.calls[0].session != nil {
		t.Fatalf("session must not be requested from a non-capable adapter: %+v", fake.calls[0].session)
	}
}

// TestRunSessions_DisabledRunsCold proves session_reuse: false forces the
// existing cold invocation path.
func TestRunSessions_DisabledRunsCold(t *testing.T) {
	d, run := sessionTestDB(t)
	fake := newFakeSessionAgent()
	rs := NewRunSessions(d, run.ID, fake, false)

	if _, err := rs.Run(context.Background(), fake, SessionRoleReviewer, agent.RunOpts{Prompt: "review"}, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if fake.calls[0].session != nil {
		t.Fatalf("disabled session reuse must run cold: %+v", fake.calls[0].session)
	}
}

// TestRunSessions_NilManagerRunsCold proves steps outside the review loop
// (or tests with no manager) run exactly as before.
func TestRunSessions_NilManagerRunsCold(t *testing.T) {
	fake := newFakeSessionAgent()
	var rs *RunSessions

	if _, err := rs.Run(context.Background(), fake, SessionRoleReviewer, agent.RunOpts{Prompt: "review"}, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if fake.calls[0].session != nil {
		t.Fatalf("nil manager must run cold: %+v", fake.calls[0].session)
	}
}

// TestRunSessions_PersistsAcrossManagers proves the minimum session-resume
// metadata survives a daemon process boundary: a new manager for the same run
// resumes the stored identities, and a different run never sees them.
func TestRunSessions_PersistsAcrossManagers(t *testing.T) {
	d, run := sessionTestDB(t)
	fake := newFakeSessionAgent()
	rs := NewRunSessions(d, run.ID, fake, true)
	if _, err := rs.Run(context.Background(), fake, SessionRoleReviewer, agent.RunOpts{Prompt: "review"}, nil); err != nil {
		t.Fatalf("initial: %v", err)
	}

	// Same run, fresh manager (e.g. daemon restart): resumes the identity.
	rs2 := NewRunSessions(d, run.ID, fake, true)
	if _, err := rs2.Run(context.Background(), fake, SessionRoleReviewer, agent.RunOpts{Prompt: "rereview"}, nil); err != nil {
		t.Fatalf("rereview: %v", err)
	}
	second := fake.calls[len(fake.calls)-1]
	if second.session == nil || second.session.ID != "sess-1" {
		t.Fatalf("new manager for same run must resume stored session, got %+v", second.session)
	}

	// A different run must not inherit the identity.
	repo2, err := d.InsertRepo("/tmp/repo2", "https://github.com/test/repo2", "main")
	if err != nil {
		t.Fatalf("insert repo2: %v", err)
	}
	otherRun, err := d.InsertRun(repo2.ID, "feature/y", "h", "b")
	if err != nil {
		t.Fatalf("insert other run: %v", err)
	}
	rs3 := NewRunSessions(d, otherRun.ID, fake, true)
	if _, err := rs3.Run(context.Background(), fake, SessionRoleReviewer, agent.RunOpts{Prompt: "other review"}, nil); err != nil {
		t.Fatalf("other run: %v", err)
	}
	third := fake.calls[len(fake.calls)-1]
	if third.session == nil || third.session.ID != "" {
		t.Fatalf("different run must start its own session, got %+v", third.session)
	}
}

func TestRunSessions_FallbackResumesWithItsActualProvider(t *testing.T) {
	d, run := sessionTestDB(t)
	codex := newFakeSessionAgent()
	codex.name = "codex"
	codex.failNext = errors.New("codex start: executable not found")
	claude := newFakeSessionAgent()
	claude.name = "claude"
	fallback := agent.NewFallback([]agent.Agent{codex, claude})

	rs := NewRunSessions(d, run.ID, fallback, true)
	if _, err := rs.Run(context.Background(), fallback, SessionRoleReviewer, agent.RunOpts{Prompt: "review"}, nil); err != nil {
		t.Fatalf("initial: %v", err)
	}
	stored, err := d.GetRunAgentSessions(run.ID)
	if err != nil {
		t.Fatalf("stored sessions: %v", err)
	}
	if len(stored) != 1 || stored[0].Agent != "claude" || stored[0].SessionID != "sess-1" {
		t.Fatalf("stored session = %+v", stored)
	}

	rs = NewRunSessions(d, run.ID, fallback, true)
	if _, err := rs.Run(context.Background(), fallback, SessionRoleReviewer, agent.RunOpts{Prompt: "rereview"}, nil); err != nil {
		t.Fatalf("rereview: %v", err)
	}
	if len(codex.calls) != 1 {
		t.Fatalf("codex calls = %d, want only its initial failed call", len(codex.calls))
	}
	last := claude.calls[len(claude.calls)-1]
	if last.session == nil || last.session.ID != "sess-1" || last.session.Agent != "claude" {
		t.Fatalf("fallback did not route the stored session to claude: %+v", last.session)
	}
}

// TestRunSessions_AgentChangeDiscardsStoredSession proves a stored identity
// from a different adapter is never fed to the current one.
func TestRunSessions_AgentChangeDiscardsStoredSession(t *testing.T) {
	d, run := sessionTestDB(t)
	if err := d.UpsertRunAgentSession(run.ID, string(SessionRoleReviewer), "codex", "codex-thread"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	fake := newFakeSessionAgent() // Name() == "fake", not "codex"
	rs := NewRunSessions(d, run.ID, fake, true)
	if _, err := rs.Run(context.Background(), fake, SessionRoleReviewer, agent.RunOpts{Prompt: "review"}, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if call := fake.calls[0]; call.session == nil || call.session.ID != "" {
		t.Fatalf("stored session for another agent must be discarded, got %+v", call.session)
	}
}
