package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/gatecontext"
	"github.com/kunchenguid/no-mistakes/internal/gateguidance"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/skill"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// canonicalStaleMonitorPhrases are the load-bearing claims of the corrected
// "PR fell behind / conflicted after checks pass" guidance: the live CI monitor
// auto-rebases and re-pushes such a PR, so the agent runs no command and never
// hand-rebases, and `no-mistakes rerun` is only the dead-monitor recovery.
//
// The last two phrases bound that recovery. `rerun` re-runs the gate branch tip
// (daemon.RunManager.HandleRerun), while `axi run` pushes the caller's local
// HEAD, so offering `rerun` as a post-commit retry silently re-validates the
// pre-fix code. Every surface must scope it to an unchanged local HEAD.
//
// The final phrases close the other half of that recovery: newRerunCmd fires
// MethodRerun and returns, so `rerun` alone leaves the recovered run parked at
// its first gate with nobody responding, and something must answer those gates.
// That follow-up `axi run` reattaches only CONDITIONALLY: HandleRerun stamps the
// gate branch tip while activeRunID matches the caller's local HEAD, and
// pipeline fix commits move the gate branch ref, so a surface must state the
// condition and the fallback instead of promising an unconditional reattach.
//
// "before the rerun, not after" pins the ORDER of that fallback, the third
// attempt at this paragraph. Naming the sync as a recovery from a failed
// reattach was unreachable: the rerun's own pending run is the newest run
// branchsync.inspect selects, it carries no push binding, so the state is
// legacy_unbound and `axi sync` refuses. Synchronizing first is what makes the
// gate head equal local HEAD, which is the reattach condition itself. Proven end
// to end by e2e TestAxiStaleMonitorSyncBeforeRerunReattaches and its
// TestAxiStaleMonitorRerunBeforeSyncStrandsTheRecovery counterpart.
var canonicalStaleMonitorPhrases = []string{
	"never hand-rebase",
	"re-pushes",
	"no-mistakes rerun",
	"re-validates the head already pushed to the gate",
	"only for an unchanged local HEAD",
	"returns immediately without driving",
	"answer the recovered run's gates",
	"only while the gate head still equals your local HEAD",
	"branch_sync",
	"no-mistakes axi run",
	"before the rerun, not after",
}

// canonicalCustodyRecoveryPhrases pin the corrected `recover_custody` recovery.
// That state exists only because the gate holds pipeline commits the worktree
// lacks, so the local HEAD `axi run` matches cannot equal the gate tip `rerun`
// stamps: custody recovery has to move the worktree first, and only then is a
// run startable and drivable. Every surface must keep the order and must not
// promise an unconditional `rerun` + `axi run` reattach here.
var canonicalCustodyRecoveryPhrases = []string{
	"no-mistakes axi sync --recover",
	"preserved pipeline head",
	`no-mistakes axi run --intent`,
	"returns immediately without driving",
	"reattaches only while your local HEAD equals that preserved head",
}

// canonicalAbortScopePhrases pin the scope of `abort`/`rerun` as a principle,
// not an enumeration. Listing the legitimate exceptions (dead CI monitor,
// recover_custody) kept colliding with other live surfaces that legitimately
// prescribe abort - blocked_recover_run_active tells the reader to abort, and
// the skill's own before-you-start sanctions abort to discard a run - because
// the exception set is open. What actually distinguishes a wrong abort from a
// right one is whether the caller means to throw that run away.
var canonicalAbortScopePhrases = []string{
	"abort or rerun while a gate awaits your response or a step is actively working",
	"unless you are deliberately discarding that run",
}

var canonicalPreserveGateFixPhrases = []string{
	"post-pipeline",
	"on top",
	"every pipeline fix commit",
}

var canonicalBranchSyncPhrases = []string{
	"branch_sync",
	"no-mistakes axi sync",
	"blocked",
	"reset, stash, merge, rebase, force, or branch replacement",
	// Guarded custody recovery for a terminal run whose pipeline commits were
	// never published (v1.38.1 dogfood catch): the action, its next_action
	// code, and the preservation claim must stay on every guidance surface.
	"recover_custody",
	"no-mistakes axi sync --recover",
	"preserved in the local gate",
	// Cancellation releases a run that never changed the submitted head
	// (v1.44.2 dogfood catch): every surface must name the released state and
	// that it needs no recovery.
	"user_owned",
	"before changing the submitted head",
}

// canonicalFindingSeverityPhrases are the load-bearing claims of the finding
// selection floor: info findings are advisory, they stay out of --findings by
// default, and the pipeline applies the same floor to its own automatic fixing.
var canonicalFindingSeverityPhrases = []string{
	"advisory",
	"--findings",
	"auto_fix.min_severity",
}

func TestFindingSeverityGuidance_SyncedAcrossSurfaces(t *testing.T) {
	surfaces := map[string]string{
		"skill body":               skill.Markdown(),
		"agents guide":             readAgentsGuide(t),
		"live finding-gate string": findingSeverityGuidance,
	}
	for name, content := range surfaces {
		for _, phrase := range canonicalFindingSeverityPhrases {
			if !strings.Contains(content, phrase) {
				t.Errorf("%s is missing the canonical finding-severity guidance phrase %q", name, phrase)
			}
		}
	}
}

const canonicalPipelineAgentPrerequisite = "a supported native agent binary, the `agent: cursor` ACP alias, or an explicit `acp:<target>` through `acpx`"

// TestStaleMonitorGuidance_SyncedAcrossSurfaces guards the repo invariant that
// agent-driving guidance stays in sync across its three surfaces: the skill
// body, the published agents guide, and the live axi help string. The earlier
// wrong wording (telling agents to re-run a stale PR with `axi run`) shipped to
// only one surface; this keeps the corrected guidance present on all three.
func TestStaleMonitorGuidance_SyncedAcrossSurfaces(t *testing.T) {
	surfaces := map[string]string{
		"skill body":      skill.Markdown(),
		"agents guide":    readAgentsGuide(t),
		"axi help string": staleMonitorGuidance,
	}
	assertPhrasesOnEverySurface(t, "stale-monitor", surfaces, canonicalStaleMonitorPhrases)

	// The discarded wrong framing must not creep back into any surface.
	for name, content := range surfaces {
		if strings.Contains(content, "rebase step integrates the latest") {
			t.Errorf("%s still carries the discarded 'rebase step integrates the latest default branch' wording", name)
		}
	}
	// The unconditional-reattach promise is pinned positively instead: an exact
	// historical substring let any reworded promise through, while the reattach
	// condition and the sync-first order in canonicalStaleMonitorPhrases are
	// what a surface actually has to carry.
}

// TestCustodyRecoveryGuidance_SyncedAcrossSurfaces pins the recover_custody
// recovery on the static surfaces and on the live sync help string, which
// previously drifted unguarded into promising a `rerun` + `axi run` reattach
// that cannot work in that state.
func TestCustodyRecoveryGuidance_SyncedAcrossSurfaces(t *testing.T) {
	assertPhrasesOnEverySurface(t, "custody-recovery", map[string]string{
		"skill files":           skill.Bundle(),
		"agents guide":          readAgentsGuide(t),
		"live sync help string": custodyRecoveryGuidance,
	}, canonicalCustodyRecoveryPhrases)

	// The diverged-recovery refusal is a custody-recovery surface too, and the
	// only one that ever offered `rerun` as a third exit. Taking it reactivates
	// the run, after which both exits this same message names are refused with
	// blocked_recover_run_active, so it must offer neither the command nor a
	// reworded version of it.
	diverged := branchsync.BlockedRecoverDivergedMessage("refs/no-mistakes/recover/run-1")
	for _, forbidden := range []string{"rerun", "resume validating"} {
		if strings.Contains(diverged, forbidden) {
			t.Errorf("blocked_recover_diverged offers %q, which forecloses the exits it names: %q", forbidden, diverged)
		}
	}
	for _, want := range []string{"refs/no-mistakes/recover/run-1", "--keep-local", "no files or refs were changed"} {
		if !strings.Contains(diverged, want) {
			t.Errorf("blocked_recover_diverged is missing %q: %q", want, diverged)
		}
	}
}

// TestAbortScopeGuidance_SyncedAcrossSurfaces pins the abort/rerun scope rule,
// including the live `axi abort` help text, so the terminal-outcome
// precondition and its two prescribed-recovery exceptions cannot drift apart.
func TestAbortScopeGuidance_SyncedAcrossSurfaces(t *testing.T) {
	assertPhrasesOnEverySurface(t, "abort-scope", map[string]string{
		"skill body":          skill.Markdown(),
		"agents guide":        readAgentsGuide(t),
		"live axi abort help": newAxiAbortCmd().Long,
	}, canonicalAbortScopePhrases)
}

func assertPhrasesOnEverySurface(t *testing.T, kind string, surfaces map[string]string, phrases []string) {
	t.Helper()
	for name, content := range surfaces {
		normalized := normalizeGuidanceSpace(content)
		for _, phrase := range phrases {
			if !strings.Contains(normalized, normalizeGuidanceSpace(phrase)) {
				t.Errorf("%s is missing the canonical %s guidance phrase %q", name, kind, phrase)
			}
		}
	}
}

// normalizeGuidanceSpace collapses the line wrapping each surface applies, so a
// pinned phrase matches regardless of where a surface breaks its lines.
func normalizeGuidanceSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// TestStaleMonitorGuidance_InChecksPassedOutput ensures the guidance reaches the
// agent at its point of use: the `checks-passed` axi output, where the agent
// decides what to do about the still-monitored PR.
func TestStaleMonitorGuidance_InChecksPassedOutput(t *testing.T) {
	run := &ipc.RunInfo{
		ID:      "run-1",
		Branch:  "feature/x",
		Status:  types.RunRunning, // not terminal: daemon keeps monitoring until merge
		HeadSHA: "abcdef1234567890",
		PRURL:   strptr("https://github.com/user/repo/pull/42"),
		Steps: []ipc.StepResultInfo{
			{StepName: types.StepCI, Status: types.StepStatusRunning},
		},
	}

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := renderDriveResult(cmd, run, true); err != nil {
		t.Fatalf("checks-passed must exit 0, got error: %v", err)
	}

	got := out.String()
	for _, phrase := range canonicalStaleMonitorPhrases {
		if !strings.Contains(got, phrase) {
			t.Errorf("checks-passed output missing stale-monitor guidance phrase %q in:\n%s", phrase, got)
		}
	}
}

func TestPreserveGateFixGuidance_SyncedAcrossSurfaces(t *testing.T) {
	surfaces := map[string]string{
		"skill files":      skill.Bundle(),
		"agents guide":     readAgentsGuide(t),
		"axi run help":     newAxiRunCmd().Long,
		"axi respond help": newAxiRespondCmd().Long,
		"axi abort help":   newAxiAbortCmd().Long,
	}
	for name, content := range surfaces {
		for _, phrase := range canonicalPreserveGateFixPhrases {
			if !strings.Contains(content, phrase) {
				t.Errorf("%s is missing the canonical preserve-gate-fix guidance phrase %q", name, phrase)
			}
		}
	}
}

func TestBranchSyncGuidance_SyncedAcrossStaticAndLiveSurfaces(t *testing.T) {
	surfaces := map[string]string{
		"skill files":        skill.Bundle(),
		"agents guide":       readAgentsGuide(t),
		"live sync guidance": branchSyncAgentGuidance,
	}
	for name, content := range surfaces {
		for _, phrase := range canonicalBranchSyncPhrases {
			if !strings.Contains(content, phrase) {
				t.Errorf("%s is missing branch-sync guidance phrase %q", name, phrase)
			}
		}
	}
}

func TestPipelineAgentPrerequisiteGuidance_SyncedAcrossSurfaces(t *testing.T) {
	surfaces := map[string]string{
		"skill body":   skill.Markdown(),
		"agents guide": readAgentsGuide(t),
		"axi run help": newAxiRunCmd().Long,
	}
	for name, content := range surfaces {
		normalized := strings.Join(strings.Fields(content), " ")
		if !strings.Contains(normalized, canonicalPipelineAgentPrerequisite) {
			t.Errorf("%s is missing the canonical pipeline-agent prerequisite %q", name, canonicalPipelineAgentPrerequisite)
		}
	}
}

func TestGateStepBoundaryGuidance_SyncedAcrossSurfaces(t *testing.T) {
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	_ = emitGateContextRefusal(cmd, gatecontext.Result{Nested: true, RunID: "run-1", Phase: types.StepDocument})
	surfaces := map[string]string{
		"prompt boundary": gateguidance.PromptBoundary("document"),
		"skill body":      skill.Markdown(),
		"agents guide":    readAgentsGuide(t),
		"live refusal":    out.String(),
	}
	phrases := []string{"assigned phase", "outer executor", "push", "PR", "CI"}
	for name, content := range surfaces {
		for _, phrase := range phrases {
			if !strings.Contains(content, phrase) {
				t.Errorf("%s is missing gate-step boundary phrase %q", name, phrase)
			}
		}
	}
	for _, name := range []string{"skill body", "agents guide", "live refusal"} {
		if !strings.Contains(surfaces[name], "nested_gate_context") {
			t.Errorf("%s is missing structured nested-context error code", name)
		}
	}
}

func TestNormalDriveOutputDoesNotFloodBranchSyncGuidance(t *testing.T) {
	got := renderDriveResultForGuidanceTest(t, true, types.RunRunning)
	if strings.Contains(got, branchSyncAgentGuidance) || strings.Contains(got, "branch_sync.next_action") {
		t.Fatalf("ordinary drive output included irrelevant branch-sync guidance:\n%s", got)
	}
}

func TestPreserveGateFixGuidance_InPointOfUseOutputs(t *testing.T) {
	gate := stepView{
		Name:   "review",
		Status: "awaiting_approval",
		FindingsJSON: findingsJSON(t, []types.Finding{
			{ID: "review-1", Severity: "warning", File: "main.go", Action: types.ActionAskUser, Description: "calls os.Exit"},
		}, "1 blocking issue"),
	}
	surfaces := map[string]string{
		"gate output":          axiDoc(gateFields(gate)...),
		"checks-passed output": renderDriveResultForGuidanceTest(t, true, types.RunRunning),
		"failed output":        renderDriveResultForGuidanceTest(t, false, types.RunFailed),
	}
	for name, content := range surfaces {
		for _, phrase := range canonicalPreserveGateFixPhrases {
			if !strings.Contains(content, phrase) {
				t.Errorf("%s is missing the canonical preserve-gate-fix guidance phrase %q in:\n%s", name, phrase, content)
			}
		}
	}
}

func renderDriveResultForGuidanceTest(t *testing.T, ciReady bool, status types.RunStatus) string {
	t.Helper()
	run := &ipc.RunInfo{
		ID:      "run-1",
		Branch:  "feature/x",
		Status:  status,
		HeadSHA: "abcdef1234567890",
		PRURL:   strptr("https://github.com/user/repo/pull/42"),
		Steps: []ipc.StepResultInfo{
			{StepName: types.StepCI, Status: types.StepStatusRunning},
		},
	}

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	err := renderDriveResult(cmd, run, ciReady)
	var exit *exitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("renderDriveResult returned unexpected error: %v", err)
	}
	return out.String()
}

func readAgentsGuide(t *testing.T) string {
	t.Helper()
	// internal/cli -> repo root is two levels up.
	path := filepath.Join("..", "..", "docs", "src", "content", "docs", "guides", "agents.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read agents guide %s: %v", path, err)
	}
	return string(data)
}
