package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// piReviewReply is one Pi invocation that returns review JSON and reports
// 100 input, 20 output, and 30 cache-read tokens.
func piReviewReply(t *testing.T, review string) string {
	t.Helper()
	text, err := json.Marshal(review)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`{"type":"message_end","message":{"role":"assistant","provider":"xai","model":"grok-4.6","content":[{"type":"text","text":%s}],"usage":{"input":100,"output":20,"cacheRead":30}}}
{"type":"agent_end","messages":[]}
`, text)
}

// installFakePiSequence installs a pi that answers its nth invocation with
// replies[n-1], so one replay can drive a review and its reruns.
func installFakePiSequence(t *testing.T, fakeDir string, replies ...string) {
	t.Helper()
	for i, reply := range replies {
		if err := os.WriteFile(filepath.Join(fakeDir, fmt.Sprintf("reply-%d.jsonl", i+1)), []byte(reply), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := fmt.Sprintf(`#!/bin/sh
cat >/dev/null
dir='%s'
n=$(cat "$dir/calls" 2>/dev/null || echo 0)
n=$((n + 1))
printf '%%s' "$n" > "$dir/calls"
cat "$dir/reply-$n.jsonl"
`, fakeDir)
	if err := os.WriteFile(filepath.Join(fakeDir, "pi"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestReplayTokensCoverEveryReviewAttempt pins a candidate's recorded cost to
// every review turn Review spent, not just the last one: a rerun forced by a
// schema slip costs tokens too, and an attempt whose usage is unknown makes
// the total unknown rather than smaller.
func TestReplayTokensCoverEveryReviewAttempt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	const (
		valid = `{"findings":[],"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"}`
		// Pi's adapter rejects this against the schema; usage from that
		// failed attempt is still reported so the replay cost is honest.
		missingRiskLevel = `{"findings":[],"risk_rationale":"clean","risk_scope":"source-or-external"}`
		// This passes the schema, so Pi returns a Result, but Review's own
		// check refuses the blank rationale and reruns.
		blankRationale = `{"findings":[],"risk_level":"low","risk_rationale":" ","risk_scope":"source-or-external"}`
	)
	for _, tc := range []struct {
		name         string
		first        string
		wantReported bool
		wantInput    int64
		wantOutput   int64
		wantFresh    int64
	}{
		{name: "schema-rejected attempt still reports usage", first: missingRiskLevel, wantReported: true, wantInput: 200, wantOutput: 40, wantFresh: 140},
		{name: "two attempts that both report usage", first: blankRationale, wantReported: true, wantInput: 200, wantOutput: 40, wantFresh: 140},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
			defer sourceDB.Close()

			fakeDir := t.TempDir()
			installFakePiSequence(t, fakeDir, piReviewReply(t, tc.first), piReviewReply(t, valid))
			t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))

			store, err := Open(p.EvalDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if _, err := Capture(ctx, store, p, sourceDB, run.ID); err != nil {
				t.Fatal(err)
			}

			_, evaluations, err := Replay(ctx, store, ReplayOptions{
				Set:       "all",
				Candidate: Candidate{Agent: types.AgentPi, Model: "grok-4.6"},
				Repeats:   1,
			})
			if err != nil {
				t.Fatalf("Replay: %v (evaluations=%#v)", err, evaluations)
			}
			if len(evaluations) != 1 || evaluations[0].Status != "completed" {
				t.Fatalf("evaluations = %#v, want one completed replay", evaluations)
			}
			calls, err := os.ReadFile(filepath.Join(fakeDir, "calls"))
			if err != nil || string(calls) != "2" {
				t.Fatalf("pi invocations = %q (%v), want the rejected review plus one rerun", calls, err)
			}
			got := evaluations[0]
			if got.TokensReported != tc.wantReported || got.InputTokens != tc.wantInput || got.OutputTokens != tc.wantOutput || got.FreshInputTokens != tc.wantFresh {
				t.Fatalf("tokens = reported %v input %d output %d fresh %d, want reported %v input %d output %d fresh %d",
					got.TokensReported, got.InputTokens, got.OutputTokens, got.FreshInputTokens,
					tc.wantReported, tc.wantInput, tc.wantOutput, tc.wantFresh)
			}
		})
	}
}

// retryingAgent models an adapter whose internal retry loop reports each
// attempt through OnAttempt and hands back only the last one, as runWithRetry
// does.
type retryingAgent struct {
	attempts []*agent.Result
	err      error
}

func (*retryingAgent) Name() string { return "claude" }
func (*retryingAgent) Close() error { return nil }
func (a *retryingAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	for _, attempt := range a.attempts {
		if opts.OnAttempt != nil {
			opts.OnAttempt(agent.Attempt{Agent: "claude", Result: attempt, Err: a.err})
		}
	}
	return a.attempts[len(a.attempts)-1], a.err
}

type fixedResultAgent struct {
	result *agent.Result
	err    error
}

func (*fixedResultAgent) Name() string { return "pi" }
func (*fixedResultAgent) Close() error { return nil }
func (a *fixedResultAgent) Run(context.Context, agent.RunOpts) (*agent.Result, error) {
	return a.result, a.err
}

func reportedUsage(input, output, cacheRead int) *agent.Result {
	return &agent.Result{
		Usage:         agent.TokenUsage{InputTokens: input, OutputTokens: output, CacheReadTokens: cacheRead, Reported: true},
		UsageReported: true,
	}
}

// TestObservedAgentCountsEveryAdapterAttempt proves a retry-exhausted review
// is charged for every attempt it burned. The adapter retries below this seam
// and returns only its last attempt, so reading the returned result alone
// would charge four attempts as one and make the candidate read as cheaper
// than one that passed on its first try.
func TestObservedAgentCountsEveryAdapterAttempt(t *testing.T) {
	observed := &observedAgent{inner: &retryingAgent{
		attempts: []*agent.Result{
			reportedUsage(50_000, 100, 5_000),
			reportedUsage(50_000, 100, 5_000),
			reportedUsage(50_000, 100, 5_000),
			reportedUsage(50_000, 100, 5_000),
		},
		err: errors.New("claude structured output rejected"),
	}}
	if _, err := observed.Run(context.Background(), agent.RunOpts{}); err == nil {
		t.Fatal("expected the exhausted turn's error to surface")
	}
	if observed.usageMissing {
		t.Fatal("every attempt reported usage, so the sum is complete")
	}
	if observed.usage.InputTokens != 200_000 || observed.usage.OutputTokens != 400 ||
		observed.usage.CacheReadTokens != 20_000 || observed.freshInputTokens != 180_000 {
		t.Fatalf("usage = %+v fresh = %d, want all four attempts summed", observed.usage, observed.freshInputTokens)
	}
}

// TestObservedAgentMarksUsageIncompleteWhenAnAttemptLacksIt keeps the rule the
// captured baseline uses: one attempt with no reported usage makes the whole
// sum unknown rather than a smaller cost.
func TestObservedAgentMarksUsageIncompleteWhenAnAttemptLacksIt(t *testing.T) {
	observed := &observedAgent{inner: &retryingAgent{
		attempts: []*agent.Result{reportedUsage(50_000, 100, 5_000), nil},
		err:      errors.New("claude exited: status 1"),
	}}
	if _, err := observed.Run(context.Background(), agent.RunOpts{}); err == nil {
		t.Fatal("expected the failed turn's error to surface")
	}
	if !observed.usageMissing {
		t.Fatal("an attempt with no reported usage must make the sum incomplete")
	}
}

// TestObservedAgentSumsASucceedingTurnsUsage covers the adapter that reports
// no attempts at all, where the returned result is the whole turn.
func TestObservedAgentSumsASucceedingTurnsUsage(t *testing.T) {
	observed := &observedAgent{inner: &fixedResultAgent{result: reportedUsage(100, 20, 30)}}
	if _, err := observed.Run(context.Background(), agent.RunOpts{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if observed.usageMissing {
		t.Fatal("a succeeding turn's reported usage must count")
	}
	if observed.usage.InputTokens != 100 || observed.freshInputTokens != 70 {
		t.Fatalf("usage = %+v fresh = %d, want 100 input and 70 fresh", observed.usage, observed.freshInputTokens)
	}
}
