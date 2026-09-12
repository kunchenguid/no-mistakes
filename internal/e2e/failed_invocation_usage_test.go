//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestFailedInvocationUsageJourney drives issue #1054 through the stock
// operator surfaces: a push that triggers a real pipeline, then
// `no-mistakes stats --run` / `stats --agents` reading the rows the daemon
// persisted.
//
// Rounds that all end in failure must read differently:
//
//   - a review round whose output the adapter rejected against the schema
//     really did burn tokens, so its row carries them. Before the fix the
//     adapter discarded its parsed usage with the rejected result and the
//     row stored a fabricated 0.
//   - a review round killed by its wall-clock budget never reported usage at
//     all, so its row reads "-" (unknown), never 0.
//   - a review fix round that failed keeps its usage too, and a failure alone
//     is never read as a session fallback.
//
// The aggregate follows the same rule as every other nullable column: one
// invocation without usage blanks that purpose's total rather than silently
// summing the rows it happens to have.
func TestFailedInvocationUsageJourney(t *testing.T) {
	scenario := filepath.Join(t.TempDir(), "failed-invocation-usage.yaml")
	content := `actions:
  # The fixer turn is claimed first: its prompt also carries the branch line
  # the rereview rule below matches on.
  - match: "Previous review findings to address:"
    text: "fix round ran and spent tokens, but cannot produce structured output"
    structured_raw: 'null'
  - match: "branch: usage-on-failed-review-fix"
    text: "one auto-fixable finding"
    structured:
      findings:
        - id: "seeded-finding"
          severity: warning
          file: "fix.txt"
          line: 1
          description: "seeded so the pipeline spends a review fix round"
          action: auto-fix
          review_scope: source
      risk_level: low
      risk_rationale: "seeded finding"
      risk_scope: source-or-external
  - match: "branch: usage-on-rejected-review"
    text: "review ran and spent tokens, but cannot produce structured output"
    structured_raw: 'null'
  - match: "branch: usage-unknown-on-killed-review"
    delay_ms: 60000
    text: "never reached"
    structured:
      findings: []
      risk_level: low
      risk_rationale: "never reached"
      risk_scope: source-or-external
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no risks detected in the diff"
      risk_scope: source-or-external
      tested: ["fakeagent: simulated test run"]
      testing_summary: "simulated tests passed"
      scenarios:
        - name: "fakeagent: simulated end-to-end scenario"
          result: pass
          live: true
          evidence: "fakeagent: simulated test run"
          reason: ""
      verdict: go
      artifacts: []
      title: "feat: fakeagent change"
      body: "## Summary\nfakeagent canned PR body"
`
	if err := os.WriteFile(scenario, []byte(content), 0o644); err != nil {
		t.Fatalf("write scenario: %v", err)
	}

	h := NewHarness(t, SetupOpts{
		Agent:    "grok",
		Scenario: scenario,
		// The killed round below must be ended by the pipeline's budget, not
		// by the fake. Keep it short so the journey stays fast; the timeout
		// path is identical at the production half hour.
		GlobalConfigExtra: strings.Join([]string{
			`agent_timeout: "5s"`,
			`review_agent_timeout: "5s"`,
		}, "\n"),
	})
	// A review fix round only happens when the repository budgets one.
	h.CommitChange("main", ".no-mistakes.yaml", "auto_fix:\n  review: 1\n", "budget one review fix round")
	if out, err := h.runGit(context.Background(), h.WorkDir, "push", "origin", "main"); err != nil {
		t.Fatalf("push trusted repo config: %v\n%s", err, out)
	}
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	// Round 1: the adapter parsed usage and then rejected the output.
	h.CommitChange("usage-on-rejected-review", "rejected.txt", "change\n", "exercise a rejected review round")
	h.PushToGate("usage-on-rejected-review")
	rejected := h.WaitForRun("usage-on-rejected-review", 120*time.Second)
	if rejected.Status != types.RunFailed {
		t.Fatalf("rejected-review run status = %s, want failed (error=%v)", rejected.Status, deref(rejected.Error))
	}

	rejectedStats, err := h.Run("stats", "--run", rejected.ID)
	if err != nil {
		t.Fatalf("stats --run %s: %v\n%s", rejected.ID, err, rejectedStats)
	}
	t.Logf("stock `no-mistakes stats --run` surface after a rejected review round:\n%s", rejectedStats)

	rows := tokenRows(t, rejectedStats, "review")
	if len(rows) == 0 {
		t.Fatalf("no review token rows recorded:\n%s", rejectedStats)
	}
	for _, row := range rows {
		if row.rawIn == "0" || row.rawOut == "0" {
			t.Fatalf("a failed review round reported a fabricated zero (in=%s out=%s):\n%s", row.rawIn, row.rawOut, rejectedStats)
		}
		if row.rawIn == "-" || row.rawOut == "-" {
			t.Fatalf("a failed review round dropped the usage the adapter parsed (in=%s out=%s):\n%s", row.rawIn, row.rawOut, rejectedStats)
		}
	}

	agentsAfterRejected, err := h.Run("stats", "--agents")
	if err != nil {
		t.Fatalf("stats --agents: %v\n%s", err, agentsAfterRejected)
	}
	t.Logf("stock `no-mistakes stats --agents` surface while every review row carries usage:\n%s", agentsAfterRejected)
	if in := aggregateInputTokens(t, agentsAfterRejected, "review"); in == "-" || in == "0" {
		t.Fatalf("review aggregate lost the failed rounds' tokens (IN TOK = %s):\n%s", in, agentsAfterRejected)
	}

	// Round 2: the round was killed before the adapter ever saw usage.
	h.CommitChange("usage-unknown-on-killed-review", "killed.txt", "change\n", "exercise a killed review round")
	h.PushToGate("usage-unknown-on-killed-review")
	killed := h.WaitForRun("usage-unknown-on-killed-review", 120*time.Second)
	if killed.Status != types.RunFailed {
		t.Fatalf("killed-review run status = %s, want failed (error=%v)", killed.Status, deref(killed.Error))
	}

	killedStats, err := h.Run("stats", "--run", killed.ID)
	if err != nil {
		t.Fatalf("stats --run %s: %v\n%s", killed.ID, err, killedStats)
	}
	t.Logf("stock `no-mistakes stats --run` surface after a killed review round:\n%s", killedStats)

	killedRows := tokenRows(t, killedStats, "review")
	if len(killedRows) == 0 {
		t.Fatalf("no review token rows recorded for the killed round:\n%s", killedStats)
	}
	for _, row := range killedRows {
		if row.rawIn != "-" || row.rawOut != "-" {
			t.Fatalf("a round that never reported usage was recorded as a number (in=%s out=%s):\n%s", row.rawIn, row.rawOut, killedStats)
		}
	}
	if !strings.Contains(killedStats, `"-" means the field was not reported`) {
		t.Fatalf("the run report did not explain what \"-\" means:\n%s", killedStats)
	}

	agentsAfterKilled, err := h.Run("stats", "--agents")
	if err != nil {
		t.Fatalf("stats --agents: %v\n%s", err, agentsAfterKilled)
	}
	t.Logf("stock `no-mistakes stats --agents` surface once one review row lacks usage:\n%s", agentsAfterKilled)
	if in := aggregateInputTokens(t, agentsAfterKilled, "review"); in != "-" {
		t.Fatalf("review aggregate summed a partially-known set as if it were complete (IN TOK = %s):\n%s", in, agentsAfterKilled)
	}

	// Round 3: a fix-mode turn, which is never retried, so its single failed
	// round is the only record of what the fix cost. Failed turns now return a
	// result, so the session label must still follow what the adapter reported
	// about the session rather than being inferred from the failure.
	h.CommitChange("usage-on-failed-review-fix", "fix.txt", "change\n", "exercise a failed review fix round")
	h.PushToGate("usage-on-failed-review-fix")
	fixRun := h.WaitForRun("usage-on-failed-review-fix", 120*time.Second)
	if fixRun.Status != types.RunFailed {
		t.Fatalf("review-fix run status = %s, want failed (error=%v)", fixRun.Status, deref(fixRun.Error))
	}

	fixStats, err := h.Run("stats", "--run", fixRun.ID)
	if err != nil {
		t.Fatalf("stats --run %s: %v\n%s", fixRun.ID, err, fixStats)
	}
	t.Logf("stock `no-mistakes stats --run` surface after a failed review fix round:\n%s", fixStats)

	fixRows := tokenRows(t, fixStats, "review")
	var sawFixRound bool
	for _, row := range fixRows {
		if row.purpose != "review-fix" {
			continue
		}
		sawFixRound = true
		if row.rawIn == "0" || row.rawIn == "-" {
			t.Fatalf("a failed review fix round lost its usage (in=%s):\n%s", row.rawIn, fixStats)
		}
		if row.session == "fallback" {
			t.Fatalf("a failed round with no session evidence was recorded as a session fallback:\n%s", fixStats)
		}
	}
	if !sawFixRound {
		t.Fatalf("the pipeline never ran a review fix round:\n%s", fixStats)
	}
}

// tokenRow is one rendered line of the per-run token table.
type tokenRow struct {
	step    string
	purpose string
	session string
	rawIn   string
	rawOut  string
}

// tokenRows reads the per-invocation token table `no-mistakes stats --run`
// prints. The table is the operator-facing report this change alters, so the
// assertions read the rendered columns rather than the database behind them.
func tokenRows(t *testing.T, out, step string) []tokenRow {
	t.Helper()
	const (
		// STEP ROUND PURPOSE SESSION ΔIN ΔOUT ΔCACHERD IN OUT CACHERD CACHEWR FRESHIN REASON
		columns  = 13
		rawInCol = 7
	)
	var rows []tokenRow
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != columns || fields[0] != step {
			continue
		}
		rows = append(rows, tokenRow{
			step:    fields[0],
			purpose: fields[2],
			session: fields[3],
			rawIn:   fields[rawInCol],
			rawOut:  fields[rawInCol+1],
		})
	}
	return rows
}

// aggregateInputTokens reads the IN TOK cell of one purpose's row in the
// `no-mistakes stats --agents` aggregate table.
func aggregateInputTokens(t *testing.T, out, purpose string) string {
	t.Helper()
	const (
		// PURPOSE COUNT AVG TOTAL COLD STARTED RESUMED FALLBACK ERRORS IN OUT CACHERD CACHEWR FRESHIN REASON
		columns = 15
		inCol   = 9
	)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != columns || fields[0] != purpose {
			continue
		}
		return fields[inCol]
	}
	t.Fatalf("no %s aggregate row in:\n%s", purpose, out)
	return ""
}
