package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// perfRecordingAgent decorates the step agent to persist one local
// agent_invocations row per invocation: identity, purpose, session mode,
// timing, exit status, and token usage. Recording is local-only and
// best-effort: a failed insert never fails the invocation, and no
// per-invocation record leaves the machine.
type perfRecordingAgent struct {
	inner    agent.Agent
	db       *db.DB
	runID    string
	stepName types.StepName
	// round returns the 1-based round the current invocation belongs to.
	round func() int
}

func (a *perfRecordingAgent) Name() string { return a.inner.Name() }

func (a *perfRecordingAgent) Close() error { return a.inner.Close() }

// SupportsSessionResume forwards the wrapped adapter's session capability.
func (a *perfRecordingAgent) SupportsSessionResume() bool {
	return agent.SupportsSessionResume(a.inner)
}

func (a *perfRecordingAgent) SupportsSessionProvider(provider string) bool {
	return agent.SupportsSessionProvider(a.inner, provider)
}

func (a *perfRecordingAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	attempts := 0
	previous := opts.OnAttempt
	opts.OnAttempt = func(attempt agent.Attempt) {
		if previous != nil {
			previous(attempt)
		}
		attempts++
		attemptOpts := opts
		attemptOpts.Session = attempt.Session
		attemptOpts.SessionFallback = attempt.SessionFallback
		a.record(ctx, attemptOpts, attempt.Agent, attempt.Result, attempt.Err, attempt.StartedAt, attempt.CompletedAt)
	}
	start := time.Now()
	result, err := a.inner.Run(ctx, opts)
	if attempts == 0 {
		a.record(ctx, opts, a.inner.Name(), result, err, start, time.Now())
	}
	return result, err
}

func (a *perfRecordingAgent) record(ctx context.Context, opts agent.RunOpts, agentName string, result *agent.Result, runErr error, startedAt, completedAt time.Time) {
	if a.db == nil {
		return
	}

	purpose := opts.Purpose
	if purpose == "" {
		purpose = string(a.stepName)
	}

	inv := db.AgentInvocation{
		RunID:       a.runID,
		StepName:    string(a.stepName),
		Round:       a.round(),
		Purpose:     purpose,
		Agent:       agentName,
		SessionMode: invocationSessionMode(opts),
		SessionKey:  invocationSessionKey(opts, result),
		StartedAt:   startedAt.Unix(),
		CompletedAt: completedAt.Unix(),
		DurationMS:  completedAt.Sub(startedAt).Milliseconds(),
		ExitStatus:  "ok",
	}
	if result != nil {
		inv.Model = result.Model
		inv.InputTokens = result.Usage.InputTokens
		inv.OutputTokens = result.Usage.OutputTokens
		inv.CacheReadTokens = result.Usage.CacheReadTokens
		inv.CacheCreationTokens = result.Usage.CacheCreationTokens
	}
	if runErr != nil {
		if ctx.Err() != nil || errors.Is(runErr, context.Canceled) {
			inv.ExitStatus = "cancelled"
			inv.FailureCategory = "cancelled"
		} else {
			inv.ExitStatus = "error"
			inv.FailureCategory = classifyInvocationFailure(runErr)
		}
	}

	if _, dbErr := a.db.InsertAgentInvocation(inv); dbErr != nil {
		slog.Warn("failed to record agent invocation", "step", a.stepName, "error", dbErr)
	}
}

func invocationSessionMode(opts agent.RunOpts) string {
	switch {
	case opts.SessionFallback:
		return db.InvocationModeFallback
	case opts.Session == nil:
		return db.InvocationModeCold
	case opts.Session.ID != "":
		return db.InvocationModeResumed
	default:
		return db.InvocationModeStarted
	}
}

// invocationSessionKey fingerprints the session identity so reuse is
// auditable without storing the raw resumable id in the telemetry table.
func invocationSessionKey(opts agent.RunOpts, result *agent.Result) string {
	id := ""
	if result != nil && result.SessionID != "" {
		id = result.SessionID
	} else if opts.Session != nil && opts.Session.ID != "" {
		id = opts.Session.ID
	}
	if id == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:8])
}

// classifyInvocationFailure buckets an invocation error into a
// low-cardinality category. Only the category is stored - never the error
// text, which can embed agent output.
func classifyInvocationFailure(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "parse events") || strings.Contains(msg, "output parse"):
		return "parse"
	case strings.Contains(msg, "exited"):
		return "exit"
	case strings.Contains(msg, "start"):
		return "spawn"
	default:
		return "other"
	}
}
