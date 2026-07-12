package pipeline

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
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
				if s.SessionID != "" && s.Agent != "" &&
					(sessionAgent == nil || agent.SupportsSessionProvider(sessionAgent, s.Agent)) {
					rs.ids[SessionRole(s.Role)] = agent.SessionRef{ID: s.SessionID, Agent: s.Agent}
				}
			}
		}
	}
	return rs
}
func validateSuccessfulSessionAttempt(opts agent.RunOpts) error {
	validationErr := agent.ValidateSuccessfulAttempt(opts)
	if validationErr == nil {
		return nil
	}
	if restoreErr := agent.RestoreFailedAttempt(opts, validationErr); restoreErr != nil {
		validationErr = errors.Join(validationErr, restoreErr)
	}
	return agent.FatalInvocationError(validationErr)
}

// Run executes one turn of the given role, reusing the role's durable
// session when the adapter supports it. logf (optional) receives operator-
// visible notes about session reuse and fallbacks.
func (rs *RunSessions) Run(ctx context.Context, a agent.Agent, role SessionRole, opts agent.RunOpts, logf func(string)) (*agent.Result, error) {
	isolationRole := opts.Role
	if role == SessionRoleFixer {
		isolationRole = types.InvocationRoleFixer
	}
	cleanupIsolation, isolationErr := prepareFixerAttemptIsolation(ctx, isolationRole, &opts)
	if isolationErr != nil {
		return nil, fmt.Errorf("snapshot candidate before session attempt: %w", isolationErr)
	}
	defer cleanupIsolation()
	if rs == nil || !rs.enabled || !agent.SupportsSessionResume(a) {
		if rs != nil && rs.enabled && logf != nil {
			logf(fmt.Sprintf("agent %s does not support session resume; running cold", a.Name()))
		}
		result, err := a.Run(ctx, opts)
		if err == nil {
			err = validateSuccessfulSessionAttempt(opts)
		}
		if agent.IsFatalInvocationError(err) {
			return nil, err
		}
		if restoreErr := agent.RestoreFailedAttempt(opts, err); restoreErr != nil {
			return nil, restoreErr
		}
		return result, err
	}

	stored := rs.id(role)
	storedID := stored.ID
	opts.Session = &stored
	result, err := a.Run(ctx, opts)
	if err == nil {
		if validationErr := validateSuccessfulSessionAttempt(opts); validationErr != nil {
			return nil, validationErr
		}
		rs.remember(role, result.SessionID, sessionProvider(a, result))
		return result, nil
	}
	if restoreErr := agent.RestoreFailedAttempt(opts, err); restoreErr != nil {
		return nil, restoreErr
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
	result, err = a.Run(ctx, opts)
	if err != nil {
		if restoreErr := agent.RestoreFailedAttempt(opts, err); restoreErr != nil {
			return nil, restoreErr
		}
		return nil, err
	}
	if validationErr := validateSuccessfulSessionAttempt(opts); validationErr != nil {
		return nil, validationErr
	}
	rs.remember(role, result.SessionID, sessionProvider(a, result))
	return result, nil
}

// Invoke executes one routed, journaled turn for a durable role session.
func (rs *RunSessions) Invoke(ctx context.Context, invoker agent.Invoker, purpose types.Purpose, scope types.InvocationScope, role SessionRole, opts agent.RunOpts, logf func(string)) (*agent.Result, error) {
	return rs.InvokeRequest(ctx, invoker, role, agent.InvocationRequest{
		Purpose: purpose,
		Scope:   scope,
		Payload: opts,
	}, logf)
}

// InvokeRequest executes a complete routed request in a durable role session.
// Resume and cold-fallback retries mutate only the native session payload:
// semantic purpose, route tier, and durable scope remain exactly unchanged.
func (rs *RunSessions) InvokeRequest(ctx context.Context, invoker agent.Invoker, role SessionRole, request agent.InvocationRequest, logf func(string)) (*agent.Result, error) {
	invoke := func(payload agent.RunOpts) (*agent.Result, error) {
		if invoker == nil {
			return nil, fmt.Errorf("session invoker is nil")
		}
		next := request
		next.Payload = payload
		return invoker.Invoke(ctx, next)
	}
	opts := request.Payload
	if definition, definitionErr := types.PurposeDefinitionFor(request.Purpose); definitionErr == nil {
		cleanupIsolation, isolationErr := prepareFixerAttemptIsolation(ctx, definition.Role, &opts)
		if isolationErr != nil {
			return nil, fmt.Errorf("snapshot candidate before routed session attempt: %w", isolationErr)
		}
		defer cleanupIsolation()
	}
	if rs == nil || !rs.enabled {
		result, err := invoke(opts)
		if err == nil {
			err = validateSuccessfulSessionAttempt(opts)
		}
		if agent.IsFatalInvocationError(err) {
			return nil, err
		}
		if restoreErr := agent.RestoreFailedAttempt(opts, err); restoreErr != nil {
			return nil, restoreErr
		}
		return result, err
	}

	stored := rs.id(role)
	storedID := stored.ID
	opts.Session = &stored
	result, err := invoke(opts)
	if err == nil {
		if validationErr := validateSuccessfulSessionAttempt(opts); validationErr != nil {
			return nil, validationErr
		}
		if result != nil {
			rs.remember(role, result.SessionID, result.Provider)
		}
		return result, nil
	}
	if agent.IsFatalInvocationError(err) {
		return nil, err
	}
	if restoreErr := agent.RestoreFailedAttempt(opts, err); restoreErr != nil {
		return nil, restoreErr
	}
	if storedID == "" || ctx.Err() != nil {
		return nil, err
	}

	if logf != nil {
		logf(fmt.Sprintf("resume of %s session failed (%v); starting a fresh %s session", role, err, role))
	}
	rs.forget(role)
	opts.Session = &agent.SessionRef{}
	opts.SessionFallback = true
	result, err = invoke(opts)
	if err != nil {
		if restoreErr := agent.RestoreFailedAttempt(opts, err); restoreErr != nil {
			return nil, restoreErr
		}
		return nil, err
	}
	if validationErr := validateSuccessfulSessionAttempt(opts); validationErr != nil {
		return nil, validationErr
	}
	if result != nil {
		rs.remember(role, result.SessionID, result.Provider)
	}
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
	if provider == "" || (rs.agent != nil && !agent.SupportsSessionProvider(rs.agent, provider)) {
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
