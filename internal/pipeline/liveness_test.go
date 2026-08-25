package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
)

func TestWatchSilence_ActivityReArmsTheClock(t *testing.T) {
	t.Parallel()
	l := newInvocationLiveness()
	ctx, cancel := watchSilence(context.Background(), 120*time.Millisecond, l)
	defer cancel()

	// Keep the invocation alive past the budget with periodic activity, then
	// go quiet: the watchdog must fire only after a full silent budget.
	deadline := time.Now().Add(3 * 120 * time.Millisecond)
	for time.Now().Before(deadline) {
		l.record(agent.ActivityStdout)
		select {
		case <-ctx.Done():
			t.Fatalf("watchdog fired during active period (cause: %v)", context.Cause(ctx))
		case <-time.After(30 * time.Millisecond):
		}
	}
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not fire after a full silent budget")
	}
	var ate *AgentTimeoutError
	if !errors.As(context.Cause(ctx), &ate) {
		t.Fatalf("cause = %v, want AgentTimeoutError", context.Cause(ctx))
	}
	if ate.Budget != 120*time.Millisecond {
		t.Fatalf("budget = %s, want 120ms", ate.Budget)
	}
	if !strings.Contains(ate.Evidence, "stdout bytes") {
		t.Fatalf("evidence = %q, want stdout activity named", ate.Evidence)
	}
}

func TestWatchSilence_QuietInvocationFiresAfterFullBudget(t *testing.T) {
	t.Parallel()
	l := newInvocationLiveness()
	start := time.Now()
	ctx, cancel := watchSilence(context.Background(), 80*time.Millisecond, l)
	defer cancel()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not fire")
	}
	elapsed := time.Since(start)
	if elapsed < 70*time.Millisecond {
		t.Fatalf("watchdog fired after %s, before the 80ms budget elapsed", elapsed)
	}
	var ate *AgentTimeoutError
	if !errors.As(context.Cause(ctx), &ate) {
		t.Fatalf("cause = %v, want AgentTimeoutError", context.Cause(ctx))
	}
	if !strings.Contains(ate.Evidence, "no stdout, lifecycle, or session-event activity observed") {
		t.Fatalf("evidence = %q, want genuinely-quiet wording", ate.Evidence)
	}
}

func TestWatchSilence_ParentCancellationIsNotATimeout(t *testing.T) {
	t.Parallel()
	parent, parentCancel := context.WithCancel(context.Background())
	l := newInvocationLiveness()
	ctx, cancel := watchSilence(parent, time.Hour, l)
	defer cancel()
	parentCancel()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("parent cancellation did not propagate")
	}
	if asAgentTimeout(context.Cause(ctx)) != nil {
		t.Fatalf("parent cancellation misread as agent timeout: %v", context.Cause(ctx))
	}
}

func TestLivenessEvidence_NamesEveryKindSeen(t *testing.T) {
	t.Parallel()
	l := newInvocationLiveness()
	l.record(agent.ActivityLifecycle)
	l.record(agent.ActivitySession)
	evidence := l.evidence()
	if !strings.Contains(evidence, "process lifecycle") || !strings.Contains(evidence, "pi session events") {
		t.Fatalf("evidence = %q, want lifecycle and session named", evidence)
	}
	if strings.Contains(evidence, "stdout bytes") {
		t.Fatalf("evidence = %q, stdout was never observed and must not be claimed", evidence)
	}
	// Most recent first: the session event is newer than the process start.
	if strings.Index(evidence, "pi session events") > strings.Index(evidence, "process lifecycle") {
		t.Fatalf("evidence = %q, want most recent activity first", evidence)
	}
}

func TestAgentTimeoutError_MessageAndStepLabel(t *testing.T) {
	t.Parallel()
	ate := &AgentTimeoutError{Budget: 30 * time.Minute, Evidence: "last activity: pi session events 30m0s ago"}
	msg := ate.Error()
	for _, want := range []string{"agent timed out after 30m0s", "agent silent for 30m0s", "pi session events"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message %q missing %q", msg, want)
		}
	}
	if !errors.Is(ate, ErrAgentTimeout) {
		t.Fatal("AgentTimeoutError must unwrap to ErrAgentTimeout")
	}
	stepErr := ate.StepError("agent review", "review agent")
	for _, want := range []string{"agent review timed out after 30m0s", "review agent silent for 30m0s", "pi session events"} {
		if !strings.Contains(stepErr.Error(), want) {
			t.Fatalf("step message %q missing %q", stepErr.Error(), want)
		}
	}
	if !errors.Is(stepErr, ErrAgentTimeout) {
		t.Fatal("step-labeled timeout must stay errors.Is-compatible with ErrAgentTimeout")
	}
}

func TestBindAgentLiveness_NestedSeamSharesOneOwner(t *testing.T) {
	t.Parallel()
	parent := context.Background()
	outer, outerCancel, outerApplied, outerLiveness := bindAgentLiveness(parent, time.Hour)
	defer outerCancel()
	if outerApplied != time.Hour || outerLiveness == nil {
		t.Fatalf("outer bind = (%s, %v), want watchdog installed", outerApplied, outerLiveness != nil)
	}
	inner, innerCancel, innerApplied, innerLiveness := bindAgentLiveness(outer, time.Hour)
	defer innerCancel()
	if innerLiveness != outerLiveness {
		t.Fatal("nested seam stacked a second liveness owner instead of sharing one monotonic clock")
	}
	if innerApplied != 0 {
		t.Fatalf("nested seam applied its own budget %s on top of the outer owner", innerApplied)
	}
	if _, ok := inner.Deadline(); ok {
		t.Fatal("liveness-governed context must not carry a fixed deadline")
	}
}

func TestBindAgentLiveness_ExistingDeadlineStaysAHardBound(t *testing.T) {
	t.Parallel()
	parent, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	ctx, _, applied, liveness := bindAgentLiveness(parent, time.Minute)
	if liveness != nil || applied != 0 {
		t.Fatalf("existing deadline must be honored unchanged, got liveness=%v applied=%s", liveness != nil, applied)
	}
	if ctx != parent {
		t.Fatal("existing-deadline parent must pass through unchanged")
	}
}
