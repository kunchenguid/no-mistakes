package pipeline

import (
	"context"
	"fmt"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/db"
)

// SessionRole identifies which durable review-loop session an invocation
// belongs to. The reviewer role spans the initial full review and every full
// rereview in a run; the fixer role spans every review-fix turn. The two are
// never mixed, so the reviewer never inherits the fixer's working context.
type SessionRole string

const (
	SessionRoleReviewer SessionRole = "reviewer"
	SessionRoleFixer    SessionRole = "review-fixer"
)

// RunSessions manages the per-run, per-role durable agent sessions of the
// review loop. It is strictly scoped to one run: identities are keyed by
// (run, role), persisted as minimum resume metadata (run, role, agent,
// session id - never prompts or transcripts), and never shared across runs,
// branches, repositories, or roles.
//
// Correctness always wins over reuse: adapters without session support run
// cold, a failed resume drops the identity and re-runs the same turn in a
// fresh same-role session, and any persistence failure degrades to cold
// invocations. A nil *RunSessions runs everything cold, preserving the
// pre-session behavior for steps outside the review loop and for tests.
type RunSessions struct {
	db      *db.DB
	runID   string
	agent   agent.Agent
	enabled bool

	mu  sync.Mutex
	ids map[SessionRole]agent.SessionRef
}

// NewRunSessions creates the manager for one run, loading any persisted
// session identities recorded by a previous process for the same run and
// agent. Identities stored for a different adapter are ignored: a session id
// is only meaningful to the adapter that minted it.
func NewRunSessions(database *db.DB, runID string, sessionAgent agent.Agent, enabled bool) *RunSessions {
	rs := &RunSessions{
		db:      database,
		runID:   runID,
		agent:   sessionAgent,
		enabled: enabled,
		ids:     map[SessionRole]agent.SessionRef{},
	}
	if database != nil {
		if stored, err := database.GetRunAgentSessions(runID); err == nil {
			for _, s := range stored {
				if s.SessionID != "" && agent.SupportsSessionProvider(sessionAgent, s.Agent) {
					rs.ids[SessionRole(s.Role)] = agent.SessionRef{ID: s.SessionID, Agent: s.Agent}
				}
			}
		}
	}
	return rs
}

// Run executes one turn of the given role, reusing the role's durable
// session when the adapter supports it. logf (optional) receives operator-
// visible notes about session reuse and fallbacks.
func (rs *RunSessions) Run(ctx context.Context, a agent.Agent, role SessionRole, opts agent.RunOpts, logf func(string)) (*agent.Result, error) {
	if rs == nil || !rs.enabled || !agent.SupportsSessionResume(a) {
		if rs != nil && rs.enabled && logf != nil {
			logf(fmt.Sprintf("agent %s does not support session resume; running cold", a.Name()))
		}
		return a.Run(ctx, opts)
	}

	stored := rs.id(role)
	storedID := stored.ID
	opts.Session = &stored
	result, err := a.Run(ctx, opts)
	if err == nil {
		rs.remember(role, result.SessionID, sessionProvider(a, result))
		return result, nil
	}
	if storedID == "" || ctx.Err() != nil {
		return nil, err
	}

	// The resume attempt failed. Never skip the turn: drop the dead identity
	// and re-run the same turn in a fresh same-role session.
	if logf != nil {
		logf(fmt.Sprintf("resume of %s session failed (%v); starting a fresh %s session", role, err, role))
	}
	rs.forget(role)
	opts.Session = &agent.SessionRef{}
	opts.SessionFallback = true
	opts.SessionFallbackReason = classifyFallbackReason(err)
	result, err = a.Run(ctx, opts)
	if err != nil {
		return nil, err
	}
	rs.remember(role, result.SessionID, sessionProvider(a, result))
	return result, nil
}

func (rs *RunSessions) id(role SessionRole) agent.SessionRef {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.ids[role]
}

// remember stores the role's latest session identity in memory and persists
// it so the run can resume the session across daemon process boundaries.
// Persistence failures are ignored: reuse degrades, correctness does not.
func (rs *RunSessions) remember(role SessionRole, sessionID, provider string) {
	if sessionID == "" {
		return
	}
	if provider == "" || !agent.SupportsSessionProvider(rs.agent, provider) {
		return
	}
	identity := agent.SessionRef{ID: sessionID, Agent: provider}
	rs.mu.Lock()
	changed := rs.ids[role] != identity
	rs.ids[role] = identity
	rs.mu.Unlock()
	if changed && rs.db != nil {
		_ = rs.db.UpsertRunAgentSession(rs.runID, string(role), provider, sessionID)
	}
}

func sessionProvider(a agent.Agent, result *agent.Result) string {
	if result != nil && result.Provider != "" {
		return result.Provider
	}
	if a == nil {
		return ""
	}
	return a.Name()
}

func (rs *RunSessions) forget(role SessionRole) {
	rs.mu.Lock()
	delete(rs.ids, role)
	rs.mu.Unlock()
	if rs.db != nil {
		_ = rs.db.DeleteRunAgentSession(rs.runID, string(role))
	}
}
