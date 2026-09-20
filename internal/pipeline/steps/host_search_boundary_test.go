package steps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

// assertRoleNeutralHostSearchBoundary checks the host-search boundary the
// shared workspace-boundary preamble delivers to every pipeline agent. It
// asserts on the emitted prompt - the generated interface the daemon hands a
// real agent - not on implementation source.
//
// The contract was added after a real Test run spent its whole 30-minute budget
// on an agent-authored `find / -maxdepth 4` that blocked at 0% CPU in a macOS
// directory-service automount under /home, so no scenario ever ran. The exact
// observed command shape must be disallowed, bounded repository/evidence reads
// must stay allowed, and the missing-tool fallback must stay role-neutral:
// only the Test step has scenarios and an "untested" state.
func assertRoleNeutralHostSearchBoundary(t *testing.T, prompt string) {
	t.Helper()
	normalized := strings.Join(strings.Fields(prompt), " ")
	for _, want := range []string{
		// Whole-root searches are disallowed by name, including the observed shape.
		"Do not search the host filesystem",
		"Never run a filesystem-wide search such as `find /` or `mdfind /`",
		"never hunt the machine for an installed tool",
		"does not make a whole-root search bounded",
		// Bounded reads that must survive the new rule.
		"Bounded searches inside the worktree",
		"external evidence path a prompt explicitly names",
		"repository-local path you were given remain fine",
		// Role-neutral missing-tool fallback.
		"not on PATH and no repository-local path is supplied",
		"report the missing tool and the work it blocked in your normal result",
		"with the concrete reason, and stop there",
	} {
		if !strings.Contains(normalized, want) {
			t.Errorf("emitted pipeline prompt missing role-neutral host-search boundary %q:\n%s", want, prompt)
		}
	}
}

// assertTestScenarioUntestedFallback checks the Test-only half of the missing
// tool rule. The Test prompt owns it because only the Test result schema has
// scenarios and an "untested" state.
func assertTestScenarioUntestedFallback(t *testing.T, prompt string) {
	t.Helper()
	normalized := strings.Join(strings.Fields(prompt), " ")
	for _, want := range []string{
		"not on PATH and has no repository-local path",
		`report the affected scenario as "untested"`,
		"instead of searching the machine for the tool",
	} {
		if !strings.Contains(normalized, want) {
			t.Errorf("emitted test prompt missing scenario untested fallback %q:\n%s", want, prompt)
		}
	}
}

// TestReviewPromptCarriesBoundedHostSearchBoundary proves the Review prompt
// surface forwards the host-search boundary from its authoritative owner,
// agent.WorktreeSteering, when the agent is wrapped exactly as the daemon wraps
// every pipeline agent. The daemon test
// TestNewPipelineAgent_SteersHostSearchBoundary owns the other half: that the
// production wiring actually applies the wrapper.
func TestReviewPromptCarriesBoundedHostSearchBoundary(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	findingsJSON, _ := json.Marshal(cleanReviewFindings())
	inner := &mockAgent{
		name: "review",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: findingsJSON}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, inner, dir, baseSHA, headSHA, config.Commands{})
	// Same wiring as internal/daemon/manager.go: the run's agent is wrapped so
	// the workspace-boundary preamble leads every prompt it sends.
	sctx.Agent = agent.WithSteering(inner, sctx.EvidenceDir)

	if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(inner.calls) != 1 {
		t.Fatalf("expected 1 review call, got %d", len(inner.calls))
	}
	prompt := inner.calls[0].Prompt
	if !strings.HasPrefix(prompt, agent.WorktreeSteering(sctx.EvidenceDir)) {
		t.Fatalf("review prompt does not lead with the workspace-boundary preamble from agent.WorktreeSteering:\n%s", prompt)
	}
	assertRoleNeutralHostSearchBoundary(t, prompt)
	// Review has no scenarios, so the Test-only fallback must not leak in.
	if strings.Contains(prompt, `report the affected scenario as "untested"`) {
		t.Errorf("review prompt carries the Test-only scenario untested fallback:\n%s", prompt)
	}
}

// TestTestPromptCarriesBoundedHostSearchBoundary is the Test half of the same
// forwarding proof: the evidence turn that drives live scenarios, and that hung
// in the incident, must carry the shared boundary plus its own scenario-shaped
// missing-tool fallback.
func TestTestPromptCarriesBoundedHostSearchBoundary(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	inner := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, inner, dir, baseSHA, headSHA, config.Commands{})
	sctx.Agent = agent.WithSteering(inner, sctx.EvidenceDir)

	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(inner.calls) != 1 {
		t.Fatalf("expected 1 evidence call, got %d", len(inner.calls))
	}
	prompt := inner.calls[0].Prompt
	if !strings.HasPrefix(prompt, agent.WorktreeSteering(sctx.EvidenceDir)) {
		t.Fatalf("test prompt does not lead with the workspace-boundary preamble from agent.WorktreeSteering:\n%s", prompt)
	}
	assertRoleNeutralHostSearchBoundary(t, prompt)
	assertTestScenarioUntestedFallback(t, prompt)
}
