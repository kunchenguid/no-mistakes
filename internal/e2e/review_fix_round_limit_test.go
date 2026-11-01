//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The opt-in review.max_fix_rounds budget (issue #683), driven through the
// real binary, daemon, and `axi` CLI against a fake reviewer that keeps
// reporting a finding after every fix. The oracle is the documented contract
// in docs reference/repo-config.md#reviewmax_fix_rounds: automatic and
// explicit fixes share one budget, the exhausted gate parks with the remaining
// findings plus a review-fix-rounds-exhausted notice, further fixes are
// refused with fix_rounds_exhausted, --yes stands aside, the value is
// trusted-default-branch-only and pinned per run, 0/unset is unlimited, and
// negative or non-integer values fail configuration loading.

const fixRoundLimitFixerMarker = "Investigate previous review findings"

// fixRoundLimitScenario returns a fake-agent scenario. The initial review
// reports one finding; every fixer turn appends a line to service.txt (so
// each fix commits a real change); every rereview either still reports a
// finding (persistent) or comes back clean.
func fixRoundLimitScenario(t *testing.T, persistent bool) string {
	t.Helper()
	rereview := "[]"
	if persistent {
		rereview = `
        - id: still-broken
          severity: warning
          file: service.txt
          description: "Service is still broken after the fix"
          action: auto-fix`
	}
	body := fmt.Sprintf(`actions:
  - match: "Fix-round provenance:"
    structured:
      findings: %s
      summary: "rereview"
      risk_level: low
      risk_rationale: "rereviewed"
      risk_scope: source-or-external
  - match: %q
    edits:
      - path: service.txt
        old: "end\n"
        new: "fixed\nend\n"
    structured:
      findings: []
      summary: "fix applied"
  - match: "Review the code changes and return structured findings"
    structured:
      findings:
        - id: broken-service
          severity: warning
          file: service.txt
          description: "Service is broken"
          action: auto-fix
      summary: "one finding"
      risk_level: medium
      risk_rationale: "broken service"
      risk_scope: source-or-external
  - structured:
      findings: []
      summary: "clean"
      risk_level: low
      risk_rationale: "none"
      risk_scope: source-or-external
      tested: ["fixture"]
      testing_summary: "fixture"
      scenarios:
        - name: "fixture"
          result: pass
          live: true
          evidence: "fixture"
          reason: ""
      verdict: go
      artifacts: []
      title: "fix: service"
      body: "Service fixed"
`, rereview, fixRoundLimitFixerMarker)
	path := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// newFixRoundLimitHarness commits trustedConfig to the default branch (the
// only source review.max_fix_rounds is read from), pushes it, and inits.
func newFixRoundLimitHarness(t *testing.T, persistent bool, trustedConfig string) *Harness {
	t.Helper()
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: fixRoundLimitScenario(t, persistent)})
	if trustedConfig != "" {
		h.setTrustedConfig(trustedConfig, "trusted review budget")
	}
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	return h
}

func (h *Harness) setTrustedConfig(content, message string) {
	h.t.Helper()
	h.CommitChange("main", ".no-mistakes.yaml", content, message)
	if out, err := h.runGit(context.Background(), h.WorkDir, "push", "origin", "main"); err != nil {
		h.t.Fatalf("push trusted repo config: %v\n%s", err, out)
	}
}

func (h *Harness) fixerTurns() int {
	n := 0
	for _, inv := range h.AgentInvocations() {
		if strings.Contains(inv.Prompt, fixRoundLimitFixerMarker) {
			n++
		}
	}
	return n
}

func (h *Harness) startFeature(branch string) {
	h.t.Helper()
	h.CommitChange(branch, "service.txt", "broken service\nend\n", "service")
}

func axiStatus(t *testing.T, h *Harness) string {
	t.Helper()
	out, err := h.Run("axi", "status")
	if err != nil {
		t.Fatalf("axi status: %v\n%s", err, out)
	}
	return out
}

func reviewFindingIDs(t *testing.T, h *Harness, branch string) []string {
	t.Helper()
	run := h.ActiveRun(branch)
	if run == nil {
		t.Fatalf("no active run for %s", branch)
	}
	step, ok := findStep(run.Steps, types.StepReview)
	if !ok || step.FindingsJSON == nil {
		t.Fatalf("review step has no findings: %+v", step)
	}
	findings, err := types.ParseFindingsJSON(*step.FindingsJSON)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, f := range findings.Items {
		if f.ID == "review-fix-rounds-exhausted" {
			continue
		}
		ids = append(ids, f.ID)
	}
	return ids
}

func TestReviewFixRoundLimit_ExplicitFixExhaustsAndPinsTheBudget(t *testing.T) {
	h := newFixRoundLimitHarness(t, true, "review:\n  max_fix_rounds: 1\n")
	branch := "feature/round-limit-explicit"
	h.startFeature(branch)

	initial, err := h.Run("axi", "run", "--intent", "Fix the broken service")
	if err != nil || !strings.Contains(initial, "broken-service") {
		t.Fatalf("initial review gate: %v\n%s", err, initial)
	}
	if strings.Contains(initial, "review-fix-rounds-exhausted") {
		t.Fatalf("the initial review spent a fix round:\n%s", initial)
	}
	t.Logf("EVIDENCE initial park (no fix spent yet):\n%s", initial)

	fixed, err := h.Run("axi", "respond", "--action", "fix", "--findings", "broken-service")
	if err != nil {
		t.Fatalf("first explicit fix: %v\n%s", err, fixed)
	}
	waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusFixReview, 90*time.Second)
	parked := axiStatus(t, h)
	for _, want := range []string{"review-fix-rounds-exhausted", "1/1", "Service is still broken after the fix", "budget is exhausted"} {
		if !strings.Contains(parked+fixed, want) {
			t.Fatalf("exhausted park is missing %q:\nrespond:\n%s\nstatus:\n%s", want, fixed, parked)
		}
	}
	t.Logf("EVIDENCE exhausted park after 1/1 explicit fix:\nrespond:\n%s\nstatus:\n%s", fixed, parked)
	if got := h.fixerTurns(); got != 1 {
		t.Fatalf("fixer turns = %d, want 1", got)
	}

	// Raising the trusted limit mid-run must not extend this run's budget.
	h.setTrustedConfig("review:\n  max_fix_rounds: 5\n", "raise budget mid-run")
	h.Checkout(branch)

	ids := reviewFindingIDs(t, h, branch)
	refused, err := h.Run("axi", "respond", "--action", "fix", "--findings", strings.Join(ids, ","))
	if err == nil && !strings.Contains(refused, "fix_rounds_exhausted") {
		t.Fatalf("second fix was not refused:\n%s", refused)
	}
	if !strings.Contains(refused, "fix_rounds_exhausted") {
		t.Fatalf("refusal does not name fix_rounds_exhausted: %v\n%s", err, refused)
	}
	t.Logf("EVIDENCE second fix refused (config raised to 5 mid-run): err=%v\n%s", err, refused)
	time.Sleep(2 * time.Second)
	if got := h.fixerTurns(); got != 1 {
		t.Fatalf("a refused fix still ran the fixer: turns = %d", got)
	}
	run := h.ActiveRun(branch)
	if step, _ := findStep(run.Steps, types.StepReview); step.Status != types.StepStatusFixReview {
		t.Fatalf("refused fix released the gate: review status = %s", step.Status)
	}

	approved, err := h.Run("axi", "respond", "--action", "approve")
	if err != nil {
		t.Fatalf("explicit approve: %v\n%s", err, approved)
	}
	final := h.WaitForRun(branch, 120*time.Second)
	if final.Status != types.RunCompleted {
		t.Fatalf("run after explicit approve = %s (error=%s)", final.Status, deref(final.Error))
	}
	t.Logf("EVIDENCE explicit approve completes the run: status=%s\n%s", final.Status, approved)
}

func explicitFixAll(t *testing.T, h *Harness, branch string) string {
	t.Helper()
	ids := reviewFindingIDs(t, h, branch)
	out, err := h.Run("axi", "respond", "--action", "fix", "--findings", strings.Join(ids, ","))
	if err != nil {
		t.Fatalf("explicit fix %v: %v\n%s", ids, err, out)
	}
	waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusFixReview, 90*time.Second)
	return out
}

// An automatic fix alone can exhaust the budget (it caps auto_fix.review),
// and `axi run --yes` - which would otherwise approve a fix_review gate -
// stands aside on the exhausted gate instead of approving it.
func TestReviewFixRoundLimit_AutomaticFixExhaustsAndYesStandsAside(t *testing.T) {
	h := newFixRoundLimitHarness(t, true, "auto_fix:\n  review: 3\nreview:\n  max_fix_rounds: 1\n")
	branch := "feature/round-limit-yes"
	h.startFeature(branch)

	out, err := h.Run("axi", "run", "--yes", "--intent", "Fix the broken service")
	t.Logf("EVIDENCE axi run --yes with auto_fix.review=3, max_fix_rounds=1: err=%v\n%s", err, out)
	if !strings.Contains(out, "total fix-round budget exhausted; --yes leaves the remaining findings awaiting an explicit response") {
		t.Fatalf("--yes did not stand aside on the exhausted gate:\n%s", out)
	}
	if !strings.Contains(out, "1/1") {
		t.Fatalf("exhausted notice does not report 1/1 fix rounds:\n%s", out)
	}
	if got := h.fixerTurns(); got != 1 {
		t.Fatalf("fixer turns = %d, want 1 (auto_fix.review=3 capped by max_fix_rounds=1)", got)
	}
	run := h.ActiveRun(branch)
	if run == nil {
		t.Fatal("run ended instead of parking")
	}
	if step, _ := findStep(run.Steps, types.StepReview); step.Status != types.StepStatusFixReview {
		t.Fatalf("review not parked after --yes: %s", step.Status)
	}
}

// Automatic and explicit fixes spend one shared budget: auto_fix.review=1
// plus one explicit fix exhausts max_fix_rounds=2.
func TestReviewFixRoundLimit_AutomaticAndExplicitShareTheBudget(t *testing.T) {
	h := newFixRoundLimitHarness(t, true, "auto_fix:\n  review: 1\nreview:\n  max_fix_rounds: 2\n")
	branch := "feature/round-limit-shared"
	h.startFeature(branch)

	first, err := h.Run("axi", "run", "--intent", "Fix the broken service")
	if err != nil {
		t.Fatalf("axi run: %v\n%s", err, first)
	}
	waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusFixReview, 90*time.Second)
	if strings.Contains(first, "review-fix-rounds-exhausted") || h.fixerTurns() != 1 {
		t.Fatalf("after the automatic fix (1/2) the budget must not be exhausted; fixer turns=%d:\n%s", h.fixerTurns(), first)
	}
	t.Logf("EVIDENCE park after automatic fix 1/2 (no exhaustion notice):\n%s", first)

	second := explicitFixAll(t, h, branch)
	status := axiStatus(t, h)
	if !strings.Contains(second+status, "review-fix-rounds-exhausted") || !strings.Contains(second+status, "2/2") {
		t.Fatalf("automatic + explicit fix did not exhaust 2/2:\nrespond:\n%s\nstatus:\n%s", second, status)
	}
	if got := h.fixerTurns(); got != 2 {
		t.Fatalf("fixer turns = %d, want 2", got)
	}
	t.Logf("EVIDENCE exhausted 2/2 after one automatic and one explicit fix:\n%s", second)

	ids := reviewFindingIDs(t, h, branch)
	refused, err := h.Run("axi", "respond", "--action", "fix", "--findings", strings.Join(ids, ","))
	if err == nil || !strings.Contains(refused, "fix_rounds_exhausted: Review used 2/2") {
		t.Fatalf("third fix not refused: err=%v\n%s", err, refused)
	}
	if got := h.fixerTurns(); got != 2 {
		t.Fatalf("refused fix ran the fixer: turns=%d", got)
	}
	t.Logf("EVIDENCE third fix refused: err=%v\n%s", err, refused)
}

// Without review.max_fix_rounds, explicit fixes stay unlimited.
func TestReviewFixRoundLimit_UnsetIsUnlimited(t *testing.T) {
	h := newFixRoundLimitHarness(t, true, "")
	branch := "feature/round-limit-unset"
	h.startFeature(branch)
	if out, err := h.Run("axi", "run", "--intent", "Fix the broken service"); err != nil || !strings.Contains(out, "broken-service") {
		t.Fatalf("initial review gate: %v\n%s", err, out)
	}
	for i := 1; i <= 3; i++ {
		out := explicitFixAll(t, h, branch)
		if strings.Contains(out, "fix_rounds_exhausted") || strings.Contains(out, "review-fix-rounds-exhausted") {
			t.Fatalf("explicit fix %d hit a budget with the setting unset:\n%s", i, out)
		}
	}
	if got := h.fixerTurns(); got != 3 {
		t.Fatalf("fixer turns = %d, want 3", got)
	}
	status := axiStatus(t, h)
	if strings.Contains(status, "review-fix-rounds-exhausted") {
		t.Fatalf("unset budget parked as exhausted:\n%s", status)
	}
	t.Logf("EVIDENCE three explicit fixes accepted with max_fix_rounds unset; status:\n%s", status)
}

// The pushed branch cannot set the limit, even with allow_repo_commands.
func TestReviewFixRoundLimit_PushedBranchCannotSetTheLimit(t *testing.T) {
	h := newFixRoundLimitHarness(t, true, "allow_repo_commands: true\n")
	branch := "feature/round-limit-untrusted"
	h.startFeature(branch)
	h.CommitChange(branch, ".no-mistakes.yaml", "allow_repo_commands: true\nreview:\n  max_fix_rounds: 1\n", "branch tries to set a review budget")
	if out, err := h.Run("axi", "run", "--intent", "Fix the broken service"); err != nil || !strings.Contains(out, "broken-service") {
		t.Fatalf("initial review gate: %v\n%s", err, out)
	}
	for i := 1; i <= 2; i++ {
		out := explicitFixAll(t, h, branch)
		if strings.Contains(out, "fix_rounds_exhausted") || strings.Contains(out, "review-fix-rounds-exhausted") {
			t.Fatalf("pushed-branch max_fix_rounds was honored on fix %d:\n%s", i, out)
		}
	}
	if got := h.fixerTurns(); got != 2 {
		t.Fatalf("fixer turns = %d, want 2", got)
	}
	status := axiStatus(t, h)
	if strings.Contains(status, "review-fix-rounds-exhausted") {
		t.Fatalf("pushed-branch budget parked as exhausted:\n%s", status)
	}
	t.Logf("EVIDENCE branch-only max_fix_rounds: 1 ignored; two explicit fixes accepted; status:\n%s", status)
}

// A clean, fully covered final rereview passes normally at the limit.
func TestReviewFixRoundLimit_CleanFinalRereviewPasses(t *testing.T) {
	h := newFixRoundLimitHarness(t, false, "review:\n  max_fix_rounds: 1\n")
	branch := "feature/round-limit-clean"
	h.startFeature(branch)
	if out, err := h.Run("axi", "run", "--intent", "Fix the broken service"); err != nil || !strings.Contains(out, "broken-service") {
		t.Fatalf("initial review gate: %v\n%s", err, out)
	}
	out, err := h.Run("axi", "respond", "--action", "fix", "--findings", "broken-service")
	if err != nil {
		t.Fatalf("explicit fix: %v\n%s", err, out)
	}
	run := h.WaitForRun(branch, 120*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("clean final rereview did not pass: %s (error=%s)\n%s", run.Status, deref(run.Error), out)
	}
	if strings.Contains(out, "review-fix-rounds-exhausted") || strings.Contains(out, "fix_rounds_exhausted") {
		t.Fatalf("clean final rereview reported exhaustion:\n%s", out)
	}
	t.Logf("EVIDENCE 1/1 fix with clean rereview completes: status=%s\n%s", run.Status, out)
}

// Negative and non-integer values fail configuration loading.
func TestReviewFixRoundLimit_InvalidValuesFailClosed(t *testing.T) {
	for _, tc := range []struct{ name, value, want string }{
		{"negative", "-1", "review.max_fix_rounds must be non-negative"},
		{"fractional", "1.5", "review.max_fix_rounds must be a non-negative integer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newFixRoundLimitHarness(t, true, "review:\n  max_fix_rounds: "+tc.value+"\n")
			branch := "feature/round-limit-invalid-" + tc.name
			h.startFeature(branch)
			out, err := h.Run("axi", "run", "--intent", "Fix the broken service")
			if !strings.Contains(out, tc.want) {
				t.Fatalf("invalid max_fix_rounds %s did not fail with %q: err=%v\n%s", tc.value, tc.want, err, out)
			}
			if h.fixerTurns() != 0 || len(h.AgentInvocations()) != 0 {
				invs := h.AgentInvocations()
				t.Fatalf("an agent launched despite invalid trusted config: %d invocations", len(invs))
			}
			t.Logf("EVIDENCE max_fix_rounds: %s rejected: err=%v\n%s", tc.value, err, out)
		})
	}
}

// The TUI's yolo resolver, like --yes, leaves the exhausted gate parked
// instead of approving it (it would otherwise approve a fix_review gate).
func TestReviewFixRoundLimit_TUIYoloStandsAside(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the TUI driver uses pty.fork(), which is Unix-only")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed; it drives the TUI through a pty")
	}
	driver, err := filepath.Abs(filepath.Join("testdata", "tui_yolo_driver.py"))
	if err != nil {
		t.Fatal(err)
	}
	h := newFixRoundLimitHarness(t, true, "review:\n  max_fix_rounds: 1\n")
	branch := "feature/round-limit-tui"
	h.startFeature(branch)
	if out, err := h.Run("axi", "run", "--intent", "Fix the broken service"); err != nil || !strings.Contains(out, "broken-service") {
		t.Fatalf("initial review gate: %v\n%s", err, out)
	}
	if out := explicitFixAll(t, h, branch); !strings.Contains(out+axiStatus(t, h), "review-fix-rounds-exhausted") {
		t.Fatalf("gate not exhausted before yolo:\n%s", out)
	}
	gated := h.ActiveRun(branch)

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", driver, h.NMBin, h.WorkDir, "y", "6", "8")
	cmd.Env = os.Environ()
	screen, err := cmd.Output()
	if err != nil {
		t.Fatalf("drive the TUI: %v\n%s", err, screen)
	}
	rendered := string(screen)
	if strings.Contains(rendered, "zero-sized grid") || !strings.Contains(rendered, branch) {
		t.Fatalf("the TUI never rendered the run:\n%s", rendered)
	}
	if !strings.Contains(rendered, "end yolo") {
		t.Fatalf("yolo mode never engaged in the TUI:\n%s", rendered)
	}
	frame := lastScreenFrame(rendered)
	t.Logf("EVIDENCE TUI screen after pressing yolo at the exhausted gate:\n%s", frame)
	time.Sleep(2 * time.Second)
	after := h.RunInfo(gated.ID)
	step, _ := findStep(after.Steps, types.StepReview)
	if step.Status != types.StepStatusFixReview || after.Status == types.RunCompleted {
		t.Fatalf("yolo resolved the exhausted gate: review=%s run=%s", step.Status, after.Status)
	}
	if got := h.fixerTurns(); got != 1 {
		t.Fatalf("yolo started another fix: turns=%d", got)
	}
}

// protectedFixScenario is fixRoundLimitScenario's persistent reviewer with a
// fixer that also edits a protected file, so the one allowed fix round's
// commit is refused by protected_paths.
func protectedFixScenario(t *testing.T) string {
	t.Helper()
	body := fmt.Sprintf(`actions:
  - match: "Fix-round provenance:"
    structured:
      findings:
        - id: still-broken
          severity: warning
          file: service.txt
          description: "Service is still broken after the fix"
          action: auto-fix
      summary: "rereview"
      risk_level: low
      risk_rationale: "rereviewed"
      risk_scope: source-or-external
  - match: %q
    edits:
      - path: service.txt
        old: "end\n"
        new: "fixed\nend\n"
      - path: secret.txt
        old: "token\n"
        new: "rotated-token\n"
    structured:
      findings: []
      summary: "fix applied"
  - match: "Review the code changes and return structured findings"
    structured:
      findings:
        - id: broken-service
          severity: warning
          file: service.txt
          description: "Service is broken"
          action: auto-fix
      summary: "one finding"
      risk_level: medium
      risk_rationale: "broken service"
      risk_scope: source-or-external
  - structured:
      findings: []
      summary: "clean"
      risk_level: low
      risk_rationale: "none"
      risk_scope: source-or-external
`, fixRoundLimitFixerMarker)
	path := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// When the last allowed fix round's commit is refused by protected_paths,
// approve and fix are both refused, and each refusal and the gate help point
// only at the responses that remain (skip or abort), never at each other.
// Oracle: docs reference/repo-config.md#protected_paths ("When the refused
// commit was Review's last allowed fix round ... only skip or abort remain").
func TestReviewFixRoundLimit_ProtectedPathRefusalOnTheLastRoundOffersOnlySkipOrAbort(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: protectedFixScenario(t)})
	h.CommitChange("main", "secret.txt", "token\n", "protected file")
	h.setTrustedConfig("protected_paths:\n  - \"secret.txt\"\nreview:\n  max_fix_rounds: 1\n", "trusted protected budget")
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	branch := "feature/round-limit-protected"
	h.startFeature(branch)

	if out, err := h.Run("axi", "run", "--intent", "Fix the broken service"); err != nil || !strings.Contains(out, "broken-service") {
		t.Fatalf("initial review gate: %v\n%s", err, out)
	}
	fixed, _ := h.Run("axi", "respond", "--action", "fix", "--findings", "broken-service")

	deadline := time.Now().Add(90 * time.Second)
	var findings string
	for time.Now().Before(deadline) {
		if run := h.ActiveRun(branch); run != nil {
			if step, ok := findStep(run.Steps, types.StepReview); ok && step.FindingsJSON != nil &&
				strings.Contains(*step.FindingsJSON, "protected-path-refusal") && reviewGateParked(step.Status) {
				findings = *step.FindingsJSON
				break
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	if findings == "" {
		h.dumpDebugState()
		t.Fatalf("review never parked on a protected-path refusal:\n%s", fixed)
	}
	status := axiStatus(t, h)
	t.Logf("EVIDENCE gate after the refused last fix round:\nrespond:\n%s\nstatus:\n%s", fixed, status)
	if !strings.Contains(findings, "review-fix-rounds-exhausted") {
		t.Fatalf("refused last fix round did not exhaust the budget; findings:\n%s", findings)
	}
	if !strings.Contains(status, "skip") || !strings.Contains(status, "abort") || strings.Contains(status, "--action fix` to retry") {
		t.Fatalf("gate help does not offer only skip or abort:\n%s", status)
	}

	approve, err := h.Run("axi", "respond", "--action", "approve")
	t.Logf("EVIDENCE approve on protected+exhausted gate: err=%v\n%s", err, approve)
	if err == nil || !strings.Contains(approve, "skip this step or abort the run") || strings.Contains(approve, "use fix to retry") {
		t.Fatalf("approve refusal does not point at skip/abort: err=%v\n%s", err, approve)
	}
	ids := reviewFindingIDs(t, h, branch)
	fix, err := h.Run("axi", "respond", "--action", "fix", "--findings", strings.Join(ids, ","))
	t.Logf("EVIDENCE fix on protected+exhausted gate: err=%v\n%s", err, fix)
	if err == nil || !strings.Contains(fix, "fix_rounds_exhausted") || !strings.Contains(fix, "skip this step or abort the run") || strings.Contains(fix, "explicitly approve") {
		t.Fatalf("fix refusal does not point at skip/abort: err=%v\n%s", err, fix)
	}
	if got := h.fixerTurns(); got != 1 {
		t.Fatalf("a refused response ran the fixer: turns=%d", got)
	}

	// Skip stays available: it releases the gate rather than being refused.
	// Skipping Review then fails Push closed (pipeline-steps.md: "Pre-skipping
	// or later skipping Review leaves no approval binding, so Push fails closed
	// unless Push is also skipped"), which is unchanged by the budget.
	skipped, err := h.Run("axi", "respond", "--action", "skip")
	t.Logf("EVIDENCE skip on protected+exhausted gate: err=%v\n%s", err, skipped)
	if strings.Contains(skipped, "fix_rounds_exhausted") || strings.Contains(skipped, "cannot skip") {
		t.Fatalf("skip was refused: %v\n%s", err, skipped)
	}
	final := h.WaitForRun(branch, 60*time.Second)
	if step, _ := findStep(final.Steps, types.StepReview); reviewGateParked(step.Status) || step.Status == types.StepStatusCompleted {
		t.Fatalf("skip did not release the review gate as skipped: %s", step.Status)
	}
	if !strings.Contains(deref(final.Error), "review skip prevents push") {
		t.Fatalf("skipped Review did not end through the documented push guard: %s (error=%s)", final.Status, deref(final.Error))
	}
	if got := h.fixerTurns(); got != 1 {
		t.Fatalf("skip ran the fixer: turns=%d", got)
	}
}

func reviewGateParked(status types.StepStatus) bool {
	return status == types.StepStatusAwaitingApproval || status == types.StepStatusFixReview
}
