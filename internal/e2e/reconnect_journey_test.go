//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestReconnectSnapshotMatchesPersistedHistory proves criterion 246: after a
// daemon restart the reconnected snapshots and histories match the persisted
// invocation attempts, provider-circuit history, and finding lineages exactly.
//
// The journey first drives a single routed run that produces rich durable
// history: a blocking finding escalates through the fixer cascade
// (fix_fast->fix_balanced->authority_strong), and the fix_fast OpenAI primary
// fails with a classified operational error that opens the OpenAI provider
// circuit run-wide. From that point every OpenAI (codex) candidate is skipped
// and the Anthropic (claude) backup carries the cascade to a resolved verdict.
// That yields a mix of succeeded, failed (OpenAI), and skipped (OpenAI)
// invocation terminals plus a three-tier finding lineage — a maximally varied
// snapshot to compare across a reconnect.
//
// It then aborts the run to a stable terminal status (so daemon recovery has
// no in-flight pipeline attempt to reconcile — those are recovered as
// interrupted by design, which is not what this criterion asserts), snapshots
// the full durable history, restarts the daemon, and re-queries the SAME run
// through a fresh DB open and through the reconnected IPC surface. The
// pre/post-restart snapshots of attempts, lineages, and repairs must be
// byte-for-byte identical, the terminal status and step history unchanged, and
// the reconnected daemon's IPC view consistent with the DB.
func TestReconnectSnapshotMatchesPersistedHistory(t *testing.T) {
	const branch = "reconnect-cascade"
	scenario := writeScenario(t, `actions:
  - match: "Review the code changes and return structured findings with a risk assessment.\n\nContext:\n- branch: reconnect-cascade"
    text: "found a stubborn bug"
    structured:
      findings:
        - id: "reconnect-1"
          severity: error
          file: "cascade.txt"
          line: 1
          description: "reconnect escalating bug"
          action: auto-fix
      risk_level: high
      risk_rationale: "a blocking bug must be fixed"
  - match: "Fix the following"
    model: "luna"
    fail: operational
  - match: "Fix the following"
    model: "sonnet"
    text: "fix_fast backup attempt"
    edits:
      - path: "cascade.txt"
        new: "sonnet fix\n"
    structured:
      summary: "fix_fast backup attempt"
  - match: "Fix the following"
    model: "opus"
    text: "fix_balanced backup attempt"
    edits:
      - path: "cascade.txt"
        new: "opus fix\n"
    structured:
      summary: "fix_balanced backup attempt"
  - match: "Fix the following"
    model: "fable"
    text: "authority backup attempt"
    edits:
      - path: "cascade.txt"
        new: "fable fix\n"
    structured:
      summary: "authority backup attempt"
  - match: "Independently verify whether each of the following"
    effort: "xhigh"
    text: "authority verified resolved"
    structured:
      verdicts:
        - lineage_id: "PROMPT_LINEAGE_ID"
          status: "resolved"
          rationale: "the authority reviewer confirms the fix"
      new_findings: []
  - match: "Independently verify whether each of the following"
    text: "still unresolved at this tier"
    structured:
      verdicts:
        - lineage_id: "PROMPT_LINEAGE_ID"
          status: "unresolved"
          rationale: "not resolved at this tier"
      new_findings: []
`)
	h := NewHarness(t, SetupOpts{Agent: "codex", Scenario: scenario})
	initGate(t, h)
	h.CommitChange(branch, "cascade.txt", "buggy line\n", "add cascade target")
	h.PushToGate(branch)
	run := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 120*time.Second)
	runID := run.ID

	// ---- prove the run produced the intended rich durable history ----
	attempts := h.InvocationAttempts(t, runID)
	repairs := h.FindingRepairs(t, runID)

	// The blocking finding escalated Luna->Terra->Sol under one lineage and
	// resolved only at the top tier.
	byTier := map[int]*db.FindingRepair{}
	lineage := ""
	for _, r := range repairs {
		byTier[r.Tier] = r
		if lineage == "" {
			lineage = r.LineageID
		} else if r.LineageID != lineage {
			t.Fatalf("escalation split across lineages %q and %q; a cascade must stay one lineage", lineage, r.LineageID)
		}
	}
	for tier := 0; tier <= 2; tier++ {
		if byTier[tier] == nil {
			t.Fatalf("missing repair row for tier %d; repairs=%+v", tier, repairs)
		}
	}
	if byTier[0].Verdict != db.RepairVerdictUnresolved || byTier[1].Verdict != db.RepairVerdictUnresolved {
		t.Fatalf("tier 0/1 verdicts = %q/%q, want both unresolved (escalating)", byTier[0].Verdict, byTier[1].Verdict)
	}
	if byTier[2].Status != db.RepairStatusResolved || byTier[2].Verdict != db.RepairVerdictResolved {
		t.Fatalf("tier 2 = status %q verdict %q, want resolved", byTier[2].Status, byTier[2].Verdict)
	}

	// The first fixer candidate is the OpenAI primary that failed operationally
	// and opened the circuit; the failover backup then resolved the tier.
	fixerAttempts := attemptsForPurpose(attempts, types.PurposeStructuredFindingRepair)
	if len(fixerAttempts) == 0 {
		t.Fatalf("no fixer attempts recorded")
	}
	primary := fixerAttempts[0]
	if primary.Terminal == nil || primary.Terminal.Outcome != types.InvocationOutcomeFailed || primary.Terminal.FailureDomain != types.FailureDomainOpenAI {
		t.Fatalf("first fixer candidate terminal = %+v, want a failed OpenAI operational attempt that opens the circuit", primary.Terminal)
	}
	assertCandidate(t, primary, "fix_fast", 0, "luna", types.EffortMedium)

	// The three succeeding fixers are all the Anthropic backups (candidate
	// index 1), because the open OpenAI circuit forced failover at every tier.
	fixers := succeededAttemptsFor(attempts, types.PurposeStructuredFindingRepair)
	if len(fixers) != 3 {
		t.Fatalf("succeeded fixer attempts = %d %v, want 3 Anthropic backups", len(fixers), candidateModels(fixers))
	}
	assertCandidate(t, fixers[0], "fix_fast", 0, "sonnet", types.EffortMedium)
	assertCandidate(t, fixers[1], "fix_balanced", 1, "opus", types.EffortMedium)
	assertCandidate(t, fixers[2], "authority_strong", 2, "fable", types.EffortXHigh)
	for _, f := range fixers {
		if f.Start.Candidate.CandidateIndex != 1 || f.Start.Candidate.Runner != types.RunnerClaude {
			t.Fatalf("succeeded fixer %s ran candidate index %d runner %q, want the Anthropic backup (index 1, claude)", f.Start.Candidate.Model, f.Start.Candidate.CandidateIndex, f.Start.Candidate.Runner)
		}
	}

	// The open OpenAI circuit skipped later same-domain candidates run-wide,
	// each recorded with the openai failure domain.
	skips := circuitSkips(attempts, types.FailureDomainOpenAI)
	if len(skips) == 0 {
		t.Fatalf("no OpenAI circuit skips recorded; the circuit did not stay open run-wide")
	}
	for _, s := range skips {
		if s.Start.Candidate.Runner != types.RunnerCodex {
			t.Fatalf("skipped candidate runner = %q, want codex (the open OpenAI domain)", s.Start.Candidate.Runner)
		}
	}

	// ---- bring the run to a stable terminal status before restarting ----
	h.Respond(runID, types.StepReview, types.ActionAbort)
	terminal := h.WaitForRun(branch, 60*time.Second)
	if !runStatusTerminal(terminal.Status) {
		t.Fatalf("run status after abort = %q, want a terminal status", terminal.Status)
	}

	// ---- snapshot the full durable history from the terminal run ----
	attemptsBefore := normalizeAttempts(h.InvocationAttempts(t, runID))
	repairsBefore := normalizeRepairs(h.FindingRepairs(t, runID))
	runBefore := normalizeRunStatus(h.RunInfo(runID))

	// ---- restart the daemon; every IPC/DB call re-opens, so this reconnects ----
	h.RestartDaemon(t)

	// ---- re-query the SAME run via a fresh DB open and via reconnected IPC ----
	attemptsAfter := normalizeAttempts(h.InvocationAttempts(t, runID))
	repairsAfter := normalizeRepairs(h.FindingRepairs(t, runID))
	runInfoAfter := h.RunInfo(runID)

	assertRowsUnchanged(t, "invocation attempts", attemptsBefore, attemptsAfter)
	assertRowsUnchanged(t, "finding repairs", repairsBefore, repairsAfter)

	if got := normalizeRunStatus(runInfoAfter); got != runBefore {
		t.Fatalf("run terminal status/step history changed across restart:\n before:\n%s\n  after:\n%s", runBefore, got)
	}
	if runInfoAfter.Status != terminal.Status {
		t.Fatalf("run status after restart = %q, want unchanged %q", runInfoAfter.Status, terminal.Status)
	}

	// ---- the reconnected daemon's IPC view is consistent with the DB ----
	d := h.OpenDB(t)
	dbRun, runErr := d.GetRun(runID)
	dbSteps, stepErr := d.GetStepsByRun(runID)
	d.Close()
	if runErr != nil || dbRun == nil {
		t.Fatalf("get run %s from db after restart: %v", runID, runErr)
	}
	if stepErr != nil {
		t.Fatalf("get steps for run %s after restart: %v", runID, stepErr)
	}
	if dbRun.Status != runInfoAfter.Status {
		t.Fatalf("IPC status %q != DB status %q after restart", runInfoAfter.Status, dbRun.Status)
	}
	if len(dbSteps) != len(runInfoAfter.Steps) {
		t.Fatalf("IPC reported %d steps, DB has %d after restart", len(runInfoAfter.Steps), len(dbSteps))
	}
	for i, s := range dbSteps {
		ipcStep := runInfoAfter.Steps[i]
		if s.StepName != ipcStep.StepName || s.StepOrder != ipcStep.StepOrder || s.Status != ipcStep.Status {
			t.Fatalf("step %d IPC {%s,%d,%s} != DB {%s,%d,%s} after restart",
				i, ipcStep.StepName, ipcStep.StepOrder, ipcStep.Status, s.StepName, s.StepOrder, s.Status)
		}
	}

	// ---- and the reconnected daemon lists the run consistently with the DB ----
	found := false
	for _, r := range h.Runs() {
		if r.ID != runID {
			continue
		}
		found = true
		if r.Status != dbRun.Status {
			t.Fatalf("run-list status %q != DB status %q after restart", r.Status, dbRun.Status)
		}
		break
	}
	if !found {
		t.Fatalf("reconnected daemon did not list run %s", runID)
	}
}

// runStatusTerminal reports whether a run has reached a durable terminal state.
func runStatusTerminal(status types.RunStatus) bool {
	switch status {
	case types.RunCompleted, types.RunFailed, types.RunCancelled:
		return true
	default:
		return false
	}
}

// normalizeAttempts renders every invocation attempt as a canonical, fully
// dereferenced field dump so pre/post-restart slices compare byte-for-byte and
// a mismatch names the exact differing row.
func normalizeAttempts(attempts []*db.InvocationAttempt) []string {
	out := make([]string, len(attempts))
	for i, a := range attempts {
		out[i] = normalizeAttempt(a)
	}
	return out
}

func normalizeAttempt(a *db.InvocationAttempt) string {
	terminal := "<nil>"
	terminalAt := "<nil>"
	if a.Terminal != nil {
		terminal = fmt.Sprintf("outcome=%s domain=%s durMS=%d in=%d out=%d cacheR=%d cacheC=%d",
			a.Terminal.Outcome, a.Terminal.FailureDomain, a.Terminal.DurationMS,
			a.Terminal.InputTokens, a.Terminal.OutputTokens,
			a.Terminal.CacheReadTokens, a.Terminal.CacheCreationTokens)
	}
	if a.TerminalAt != nil {
		terminalAt = fmt.Sprintf("%d", *a.TerminalAt)
	}
	s := a.Start
	c := s.Candidate
	return fmt.Sprintf(
		"id=%s purpose=%s role=%s scope={kind=%s run=%s step=%s round=%s util=%s} key=%s cand={profile=%s tier=%d idx=%d runner=%s model=%s effort=%s} startedAt=%d terminal={%s} terminalAt=%s",
		a.ID, s.Purpose, s.Role,
		s.Scope.Kind, s.Scope.RunID, s.Scope.StepResultID, s.Scope.StepRoundID, s.Scope.UtilityScopeID,
		s.CandidateKey,
		c.Profile, c.Tier, c.CandidateIndex, c.Runner, c.Model, c.Effort,
		a.StartedAt, terminal, terminalAt)
}

// normalizeRepairs renders every finding-repair cycle as a canonical field dump
// covering lineage, tier, verdict, status, and fixer/verifier attempt links.
func normalizeRepairs(repairs []*db.FindingRepair) []string {
	out := make([]string, len(repairs))
	for i, r := range repairs {
		out[i] = normalizeRepair(r)
	}
	return out
}

func normalizeRepair(r *db.FindingRepair) string {
	return fmt.Sprintf(
		"id=%s run=%s lineage=%s stepResult=%s stepRound=%s sev=%s action=%s desc=%q file=%s line=%d tier=%d budget=%d fixer=%s verifier=%s verdict=%s rationale=%q status=%s createdAt=%d updatedAt=%d",
		r.ID, r.RunID, r.LineageID, r.StepResultID, r.StepRoundID, r.Severity, r.Action, r.Description,
		r.File, r.Line, r.Tier, r.RemainingBudget, r.FixerAttemptID, r.VerifierAttemptID,
		r.Verdict, r.VerdictRationale, r.Status, r.CreatedAt, r.UpdatedAt)
}

// normalizeRunStatus renders a run's durable identity, terminal status, and
// step history (name/order/status/exit) — the facts criterion 246 requires to
// survive a reconnect unchanged. Wall-clock timestamps are deliberately
// excluded so a benign recovery touch never masquerades as history drift.
func normalizeRunStatus(r *ipc.RunInfo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "id=%s branch=%s head=%s base=%s status=%s blockingUnresolved=%t steps=%d",
		r.ID, r.Branch, r.HeadSHA, r.BaseSHA, r.Status, r.BlockingRepairUnresolved, len(r.Steps))
	for _, s := range r.Steps {
		exit := "<nil>"
		if s.ExitCode != nil {
			exit = fmt.Sprintf("%d", *s.ExitCode)
		}
		fmt.Fprintf(&b, "\n  step name=%s order=%d status=%s exit=%s", s.StepName, s.StepOrder, s.Status, exit)
	}
	return b.String()
}

// assertRowsUnchanged fails naming the first row that differs across a restart.
func assertRowsUnchanged(t *testing.T, label string, before, after []string) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("%s: row count changed across restart: before=%d after=%d\nbefore=%v\nafter=%v",
			label, len(before), len(after), before, after)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("%s: row %d changed across restart:\n before: %s\n  after: %s", label, i, before[i], after[i])
		}
	}
}
