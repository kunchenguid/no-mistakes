//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/quota"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// quotaCandidatesUnderTest is deliberately ordered with codex FIRST: every
// assertion below is that the run's engine came from the measured evidence rather
// than from the candidate list's order.
var quotaCandidatesUnderTest = []string{"codex", "claude"}

// writeFakeQuotaAXI writes an executable stand-in for quota-axi that answers
// `models --json` with a recorded report. The decision logic is the real one; only
// the vendor read is canned, which is what makes the routing assertion
// deterministic on any machine.
func writeFakeQuotaAXI(t *testing.T, report string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "quota-axi")
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" != "models" ]; then
  echo "unsupported command: $1" >&2
  exit 2
fi
cat <<'REPORT'
%s
REPORT
`, report)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake quota-axi: %v", err)
	}
	return path
}

// recordedQuotaReport is the evidence shape under test: claude healthy and codex
// dry, both catalog-attested at the class the pipeline's steps need.
func recordedQuotaReport() string {
	return `{
  "generatedAt": "2026-09-27T17:00:00Z",
  "schemaVersion": 1,
  "catalog": {"version": "2026-09-15", "provenance": "curated"},
  "models": [
    {"provider": "claude", "id": "claude-sonnet-4-5", "label": "Claude Sonnet 4.5", "intelligence": "high",
     "quotaScopes": ["all_models"], "state": {"status": "fresh", "stale": false},
     "effective": {"scope": "all_models", "status": "known", "effectivePercentRemaining": 88,
       "runway": {"status": "through_reset"}, "selection": {"status": "known", "spendPriority": 0.01}}},
    {"provider": "codex", "id": "gpt-5.1-codex", "label": "GPT-5.1-Codex", "intelligence": "high",
     "quotaScopes": ["all_models"], "state": {"status": "fresh", "stale": false},
     "effective": {"scope": "all_models", "status": "known", "effectivePercentRemaining": 0,
       "runway": {"status": "exhausted_now", "usableRunwaySeconds": 0},
       "selection": {"status": "known", "spendPriority": -7.0}}}
  ]
}`
}

// quotaAutoHarness wires a real no-mistakes binary to the quota-auto selection
// mode. Evidence comes from the caller's stand-in for the vendor tool.
func quotaAutoHarness(t *testing.T, quotaAXIPath string, extraConfig string) *Harness {
	t.Helper()
	extra := "agent_candidates: [" + strings.Join(quotaCandidatesUnderTest, ", ") + "]\n"
	if quotaAXIPath != "" {
		extra += "quota_axi_path: " + quotaAXIPath + "\n"
	}
	extra += extraConfig
	return NewHarness(t, SetupOpts{
		Agent:             "claude",
		AgentSelection:    string(types.AgentQuotaAuto),
		GlobalConfigExtra: extra,
	})
}

// launchQuotaAutoRun pushes one committed change and starts a run through the
// real CLI, returning the CLI's own output and error so a caller can assert on a
// refused launch as well as a successful one.
func launchQuotaAutoRun(t *testing.T, h *Harness, branch string) (string, error) {
	t.Helper()
	h.CommitChange(branch, "feature.txt", "quota-auto routing\n", "add routed feature")
	return h.Run("axi", "run", "--intent", "route the pipeline agent by measured provider quota")
}

// TestQuotaAutoRun_RoutesToTheBetterStandingCandidate drives the real pipeline
// against recorded provider evidence: the run must open on the harness the
// evidence favours, record that choice with the evidence behind it, and actually
// invoke that harness.
func TestQuotaAutoRun_RoutesToTheBetterStandingCandidate(t *testing.T) {
	h := quotaAutoHarness(t, writeFakeQuotaAXI(t, recordedQuotaReport()), "")
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/quota-auto"
	if out, err := launchQuotaAutoRun(t, h, branch); err != nil {
		h.dumpDebugState()
		t.Fatalf("launch: %v\n%s", err, out)
	}

	selection, recorded := awaitAgentSelection(t, h, branch, 120*time.Second)
	if !recorded {
		h.dumpDebugState()
		t.Fatal("the run recorded no quota-auto selection")
	}
	if selection.Agent != "claude" {
		t.Fatalf("run opened on %q, want the candidate the evidence favours", selection.Agent)
	}
	if selection.Provider != "claude" || selection.Reason != quota.ReasonRunStart {
		t.Fatalf("selection = %+v", selection)
	}
	for _, want := range []string{`"agent":"claude"`, `"model":"claude-sonnet-4-5"`, `"candidate":"codex@codex"`, "quota exhausted now"} {
		if !strings.Contains(selection.Evidence, want) {
			t.Fatalf("recorded evidence missing %q:\n%s", want, selection.Evidence)
		}
	}
	assertSelectionMatchesItsOwnEvidence(t, selection)

	// The engine that ran is the engine that was recorded: the harness the fake
	// agent was invoked as is visible in its own log, and a run whose evidence
	// never changed keeps that engine for every step.
	run := h.WaitForRun(branch, 120*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run finished %s: %v", run.Status, run.Error)
	}
	invocations := h.AgentInvocations()
	if len(invocations) == 0 {
		t.Fatal("the fake agent was never invoked")
	}
	for _, invocation := range invocations {
		if invocation.Agent != "claude" {
			t.Fatalf("an invocation ran on %q, want the selected harness", invocation.Agent)
		}
	}
}

// TestQuotaAutoRun_LiveEvidencePicksTheBetterStandingCandidate is this feature's
// live validation: it reads the real quota-axi evidence on this machine, and drives
// a real run against exactly that reading to prove the run routes wherever the
// evidence actually points.
//
// The reading is captured by the test and replayed to the daemon rather than read
// by the daemon itself, for one environmental reason: this harness isolates HOME,
// so a daemon's own credential discovery cannot reach the operator's vendor
// sessions. The numbers under test are still this machine's live ones.
//
// The assertion covers both directions, because a machine's live reading is not
// something a test may choose: when the evidence leaves a candidate usable the run
// must route to the best of them, and when it leaves none the run must fail closed
// with the per-candidate report.
func TestQuotaAutoRun_LiveEvidencePicksTheBetterStandingCandidate(t *testing.T) {
	if _, err := exec.LookPath("quota-axi"); err != nil {
		t.Skip("quota-axi is not installed on this machine")
	}
	candidates := make([]quota.Candidate, 0, len(quotaCandidatesUnderTest))
	for _, raw := range quotaCandidatesUnderTest {
		candidate, err := quota.ParseCandidate(raw)
		if err != nil {
			t.Fatalf("parse candidate %q: %v", raw, err)
		}
		candidates = append(candidates, candidate)
	}

	readCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	raw, err := exec.CommandContext(readCtx, "quota-axi", "models", "--json").Output()
	if err != nil {
		t.Fatalf("read live quota evidence: %v", err)
	}
	report, err := quota.ParseReport(raw)
	if err != nil {
		t.Fatalf("decode live quota evidence: %v", err)
	}
	t.Logf("live provider evidence: %s", report.Summary())

	// The run's opening step and the ceiling it may spend are this test's own
	// choices below, so the live decision it is compared against can be computed
	// exactly as the daemon computes it.
	const openingStep = types.StepIntent
	const ceiling = 2 * time.Minute
	expected, selectErr := quota.Select(candidates, report, quota.Request{
		Step:   openingStep,
		Class:  quota.StepClass(openingStep),
		Budget: ceiling,
	})

	h := quotaAutoHarness(t, writeFakeQuotaAXI(t, string(raw)), "agent_timeout: \"2m\"\n")
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/quota-auto-live"
	out, launchErr := launchQuotaAutoRun(t, h, branch)

	selection, recorded := awaitAgentSelection(t, h, branch, 120*time.Second)
	if selectErr != nil {
		// Live evidence leaves nothing routable, which is the fail-closed case: the
		// run must report every candidate rather than starting on a guess.
		if recorded {
			t.Fatalf("live evidence supports no candidate (%v) but the run opened on %+v", selectErr, selection)
		}
		stopped := h.WaitForRun(branch, 60*time.Second)
		message := ""
		switch {
		case stopped != nil && stopped.Error != nil:
			message = *stopped.Error
		case launchErr != nil:
			message = launchErr.Error()
		}
		if !strings.Contains(message, "quota-auto found no eligible agent") {
			h.dumpDebugState()
			t.Fatalf("live run neither routed nor refused per candidate: %q\n%s", message, out)
		}
		for _, candidate := range candidates {
			if !strings.Contains(message, candidate.String()) {
				t.Fatalf("refusal must name %s: %s", candidate, message)
			}
		}
		t.Logf("live evidence left no eligible candidate; the run failed closed with:\n%s", message)
		return
	}

	if !recorded {
		h.dumpDebugState()
		t.Fatalf("live evidence supports %s but the run recorded no selection", expected.Candidate.Agent)
	}
	if selection.Agent != string(expected.Candidate.Agent) || selection.Provider != expected.EvidenceProvider {
		t.Fatalf("run opened on %s/%s, live evidence supports %s/%s", selection.Agent, selection.Provider, expected.Candidate.Agent, expected.EvidenceProvider)
	}
	assertSelectionMatchesItsOwnEvidence(t, selection)
	t.Logf("live routing: %s opened on %s (provider %s) - %s", selection.Step, selection.Agent, selection.Provider, expected.Summary())
}

// assertSelectionMatchesItsOwnEvidence re-derives the ranking from the evidence
// the run recorded, so the assertion needs no second vendor read whose answer
// could have changed in between: the candidate the run chose must be the best of
// the candidates its own record calls eligible.
func assertSelectionMatchesItsOwnEvidence(t *testing.T, selection db.AgentSelection) {
	t.Helper()
	var payload struct {
		Agent      string `json:"agent"`
		Step       string `json:"step"`
		Candidates []struct {
			Candidate string   `json:"candidate"`
			Verdict   string   `json:"verdict"`
			Percent   *int     `json:"percent_remaining"`
			Spend     *float64 `json:"spend_priority"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal([]byte(selection.Evidence), &payload); err != nil {
		t.Fatalf("recorded evidence is not decodable: %v\n%s", err, selection.Evidence)
	}
	if payload.Agent != selection.Agent || payload.Step != selection.Step {
		t.Fatalf("recorded row and its evidence disagree: %+v vs %+v", selection, payload)
	}
	chosen, eligible := 0, 0
	var chosenPercent *int
	var chosenSpend *float64
	for _, candidate := range payload.Candidates {
		if candidate.Verdict != "eligible" {
			continue
		}
		eligible++
		if strings.HasPrefix(candidate.Candidate, selection.Agent+"@") {
			chosen++
			chosenPercent, chosenSpend = candidate.Percent, candidate.Spend
			continue
		}
		if betterStanding(candidate.Spend, candidate.Percent, chosenSpend, chosenPercent) {
			t.Fatalf("run chose %s while %s had the better standing", selection.Agent, candidate.Candidate)
		}
	}
	if chosen != 1 {
		t.Fatalf("%d of the eligible candidates are %s: %s", chosen, selection.Agent, selection.Evidence)
	}
	if eligible == 1 {
		t.Log("only one candidate was eligible, so the choice was forced")
	}
	if chosenPercent == nil {
		t.Fatalf("the chosen candidate carries no routing signals: %s", selection.Evidence)
	}
}

// betterStanding mirrors the documented ranking: spend priority first, then the
// remaining percentage, with a missing signal never outranking a measured one.
func betterStanding(spend *float64, percent *int, thanSpend *float64, thanPercent *int) bool {
	if spend != nil && thanSpend != nil && *spend != *thanSpend {
		return *spend > *thanSpend
	}
	if spend == nil || thanSpend == nil {
		return false
	}
	if percent != nil && thanPercent != nil && *percent != *thanPercent {
		return *percent > *thanPercent
	}
	return false
}

// awaitAgentSelection polls for the branch's opening routing decision, reporting
// whether one was recorded before the deadline. It looks the run up through the
// run list rather than the active-run lookup, because a run with a fast fake agent
// can finish before a caller ever observes it as active.
func awaitAgentSelection(t *testing.T, h *Harness, branch string, timeout time.Duration) (db.AgentSelection, bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	runID := ""
	for time.Now().Before(deadline) {
		if runID == "" {
			for _, run := range h.Runs() {
				if run.Branch == branch {
					runID = run.ID
					break
				}
			}
		}
		if runID != "" {
			selections, err := readAgentSelections(h, runID)
			if err == nil && len(selections) > 0 {
				return selections[0], true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return db.AgentSelection{}, false
}

// waitForAgentSelection polls a run's recorded routing decisions.
func waitForAgentSelection(t *testing.T, h *Harness, runID string) db.AgentSelection {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		selections, err := readAgentSelections(h, runID)
		if err != nil {
			last = err
		} else if len(selections) > 0 {
			return selections[0]
		}
		time.Sleep(50 * time.Millisecond)
	}
	if last != nil {
		t.Fatalf("read agent selections: %v", last)
	}
	t.Fatalf("run %s recorded no quota-auto selection", runID)
	return db.AgentSelection{}
}

func readAgentSelections(h *Harness, runID string) ([]db.AgentSelection, error) {
	database, err := db.Open(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		return nil, err
	}
	defer database.Close()
	return database.ListAgentSelections(runID)
}

// waitForAgentInvocation waits until the fake agent has been called at least once.
func waitForAgentInvocation(t *testing.T, h *Harness, timeout time.Duration) []Invocation {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if invocations := h.AgentInvocations(); len(invocations) > 0 {
			return invocations
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.dumpDebugState()
	t.Fatal("the fake agent was never invoked")
	return nil
}
