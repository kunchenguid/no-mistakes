package pipeline

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const policyFindings = `{"findings":[
  {"id":"r1","severity":"error","description":"e-auto","action":"auto-fix"},
  {"id":"r2","severity":"error","description":"e-ask","action":"ask-user"},
  {"id":"r3","severity":"warning","description":"w-auto","action":"auto-fix"},
  {"id":"r4","severity":"warning","description":"w-ask","action":"ask-user"},
  {"id":"r5","severity":"info","description":"i-noop","action":"no-op"},
  {"id":"r6","severity":"error","description":"e-unclassified"}
],"summary":"mixed"}`

func selectedIDs(t *testing.T, raw string) []string {
	t.Helper()
	if raw == "" {
		return nil
	}
	parsed, err := types.ParseFindingsJSON(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ids := make([]string, 0, len(parsed.Items))
	for _, f := range parsed.Items {
		ids = append(ids, f.ID)
	}
	return ids
}

func sameIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestGatePolicy_ZeroPolicyMatchesPrePolicyBehavior(t *testing.T) {
	p := gatePolicyFor(types.StepReview, nil)
	if got := selectedIDs(t, p.fixableFindingsJSON(policyFindings)); !sameIDs(got, []string{"r1", "r3"}) {
		t.Fatalf("fixable = %v, want only auto-fix findings at every severity", got)
	}
	if !p.parksOnFindingsJSON(policyFindings) {
		t.Fatalf("ask-user findings must park under the zero policy")
	}
	if !p.fixRoundsRemain(1_000_000) {
		t.Fatalf("the zero policy is unbounded")
	}
	// Non-review steps never pick up the review policy.
	cfg := &config.Config{Review: config.Review{MaxFixRounds: 1, AutoFixAskUser: true, GateSeverity: config.ReviewGateSeverityError}}
	if q := gatePolicyFor(types.StepTest, cfg); q.review || q.maxFixRounds != 0 || q.fixAskUser || q.errorOnly() {
		t.Fatalf("test step policy = %+v, want the zero policy", q)
	}
}

func TestGatePolicy_AutoFixAskUserSelectsAskUserFindings(t *testing.T) {
	cfg := &config.Config{Review: config.Review{AutoFixAskUser: true}}
	p := gatePolicyFor(types.StepReview, cfg)
	// r6 has no action and defaults to ask-user, so it is fixable too; no-op never is.
	if got := selectedIDs(t, p.fixableFindingsJSON(policyFindings)); !sameIDs(got, []string{"r1", "r2", "r3", "r4", "r6"}) {
		t.Fatalf("fixable = %v, want auto-fix and ask-user findings, never no-op", got)
	}
	// Parking is unchanged: once no fix round will run, ask-user findings park.
	if !p.parksOnFindingsJSON(policyFindings) {
		t.Fatalf("ask-user findings must still park when the budget is spent")
	}
}

func TestGatePolicy_GateSeverityErrorSpendsRoundsOnlyOnBlockers(t *testing.T) {
	cfg := &config.Config{Review: config.Review{GateSeverity: config.ReviewGateSeverityError}}
	p := gatePolicyFor(types.StepReview, cfg)
	if got := selectedIDs(t, p.fixableFindingsJSON(policyFindings)); !sameIDs(got, []string{"r1"}) {
		t.Fatalf("fixable = %v, want only error auto-fix findings", got)
	}
	if !p.parksOnFindingsJSON(policyFindings) {
		t.Fatalf("an error finding must park regardless of action")
	}
	warningsOnly := `{"findings":[{"id":"w","severity":"warning","description":"w","action":"ask-user"},{"id":"i","severity":"info","description":"i"}]}`
	if p.parksOnFindingsJSON(warningsOnly) {
		t.Fatalf("warning and info findings must not park under gate_severity: error")
	}
	if got := p.fixableFindingsJSON(warningsOnly); got != "" {
		t.Fatalf("fixable = %q, want nothing: warnings never spend a fix round under gate_severity: error", got)
	}

	both := gatePolicyFor(types.StepReview, &config.Config{Review: config.Review{GateSeverity: config.ReviewGateSeverityError, AutoFixAskUser: true}})
	if got := selectedIDs(t, both.fixableFindingsJSON(policyFindings)); !sameIDs(got, []string{"r1", "r2", "r6"}) {
		t.Fatalf("fixable = %v, want every error finding except no-op", got)
	}
}

func TestFixBudget(t *testing.T) {
	if (fixBudget{used: 5, max: 0}).exhausted() {
		t.Fatalf("max 0 is unbounded")
	}
	if (fixBudget{used: 1, max: 2}).exhausted() || !(fixBudget{used: 2, max: 2}).exhausted() {
		t.Fatalf("budget exhausts exactly at max")
	}
	err := &FixRoundsExhaustedError{Step: types.StepReview, Used: 2, Max: 2}
	if msg := err.Error(); msg[:len(types.FixRoundsExhaustedCode)] != types.FixRoundsExhaustedCode {
		t.Fatalf("error must start with the machine-readable code, got %q", msg)
	}
}
