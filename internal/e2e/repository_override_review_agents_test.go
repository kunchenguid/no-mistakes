//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// reviewAgentsOverrideConfig is an operator global config whose review loop
// reviews and fixes on claude profiles, plus a repository_overrides entry that
// moves only the reviewer of overrideRemote to OpenCode on an OpenRouter model.
func reviewAgentsOverrideConfig(overrideRemote string) string {
	return "agent_config:\n" +
		"  claude: {model: default-override-e2e}\n" +
		"review_agents:\n" +
		"  reviewer: {agent: claude, model: global-reviewer-e2e}\n" +
		"  fixer: {agent: claude, model: global-fixer-e2e}\n" +
		"repository_overrides:\n" +
		"  " + overrideRemote + ":\n" +
		"    review_agents:\n" +
		"      reviewer: {agent: opencode, model: openrouter/z-ai/glm-4.6}\n"
}

// runReviewFixLoop initializes the gate, points origin at remote (served from
// the harness's bare upstream), pushes a branch whose review reports one
// blocking finding, answers "fix", and waits for the run to complete.
func runReviewFixLoop(t *testing.T, h *Harness, remote, branch string) *ipc.RunInfo {
	t.Helper()
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	configureGitURLRewrite(t, h, remote, h.UpstreamDir)
	if out, err := h.runGit(context.Background(), h.WorkDir, "remote", "set-url", "origin", remote); err != nil {
		t.Fatalf("set forge-shaped origin: %v\n%s", err, out)
	}
	h.CommitChange(branch, "feature.txt", "seed for review-agents routing\n", "add feature")
	h.PushToGate(branch)
	gated := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)
	if gated == nil {
		t.Fatal("run did not park at the review gate")
	}
	h.RespondWithFindings(gated.ID, types.StepReview, types.ActionFix, []string{"routing-check"})
	run := h.WaitForRun(branch, 120*time.Second)
	if run.Status != types.RunCompleted {
		var runErr string
		if run.Error != nil {
			runErr = *run.Error
		}
		t.Fatalf("run status = %q, want completed (error: %s)", run.Status, runErr)
	}
	return run
}

// assertRecordedReviewLoop reads the daemon's persisted agent_invocations for
// the run and requires every review turn and every review-fix turn to have
// been recorded on the wanted harness.
func assertRecordedReviewLoop(t *testing.T, h *Harness, runID, reviewAgent, fixAgent string) {
	t.Helper()
	database, err := db.OpenReadOnly(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()
	rows, err := database.GetAgentInvocationsByRun(runID)
	if err != nil {
		t.Fatalf("read agent invocations: %v", err)
	}
	seen := map[string]int{}
	for _, row := range rows {
		t.Logf("recorded invocation: step=%s round=%d purpose=%s agent=%s", row.StepName, row.Round, row.Purpose, row.Agent)
		var want string
		switch row.Purpose {
		case "review":
			want = reviewAgent
		case "review-fix":
			want = fixAgent
		default:
			continue
		}
		seen[row.Purpose]++
		if row.Agent != want {
			t.Errorf("%s turn recorded agent=%s, want %s", row.Purpose, row.Agent, want)
		}
	}
	if seen["review"] < 2 || seen["review-fix"] < 1 {
		t.Fatalf("recorded review-loop turns = %v, want at least 2 review and 1 review-fix", seen)
	}
}

type reviewLoopTurns struct{ review, fix, rereview, other *Invocation }

func captureReviewLoopTurns(t *testing.T, h *Harness) reviewLoopTurns {
	t.Helper()
	invs := h.AgentInvocations()
	turns := reviewLoopTurns{
		review: firstInvocationMatching(invs, func(p string) bool {
			return strings.Contains(p, reviewTurnMarker) && !strings.Contains(p, rereviewTurnMarker) && !strings.Contains(p, fixTurnMarker)
		}),
		fix: firstInvocationMatching(invs, func(p string) bool {
			return strings.Contains(p, fixTurnMarker) && !strings.Contains(p, reviewTurnMarker)
		}),
		rereview: firstInvocationMatching(invs, func(p string) bool {
			return strings.Contains(p, reviewTurnMarker) && strings.Contains(p, rereviewTurnMarker) && !strings.Contains(p, fixTurnMarker)
		}),
		other: firstInvocationMatching(invs, func(p string) bool {
			return !strings.Contains(p, reviewTurnMarker) && !strings.Contains(p, fixTurnMarker) && !strings.Contains(p, rereviewTurnMarker)
		}),
	}
	if turns.review == nil || turns.fix == nil || turns.rereview == nil || turns.other == nil {
		t.Fatalf("missing a role turn: review=%v fix=%v rereview=%v other=%v",
			turns.review != nil, turns.fix != nil, turns.rereview != nil, turns.other != nil)
	}
	for name, inv := range map[string]*Invocation{"initial review": turns.review, "fixer": turns.fix, "rereview": turns.rereview, "non-review step": turns.other} {
		t.Logf("%-16s -> agent=%s model=%s", name, inv.Agent, modelArg(inv.Args))
	}
	return turns
}

func assertAgent(t *testing.T, role string, inv *Invocation, want string) {
	t.Helper()
	if inv.Agent != want {
		t.Fatalf("%s ran on %q, want %q (args=%v)", role, inv.Agent, want, inv.Args)
	}
}

// TestRepositoryOverrideReviewAgentsRouteOnRealBinary drives the real binary
// and daemon: a repository_overrides entry matching the run's origin replaces
// only the reviewer role (OpenCode instead of the global claude reviewer),
// while the global fixer and the default agent for other steps stay as they
// are. A repository whose origin does not match keeps every global role, and a
// pushed branch cannot reach the override or review_agents from .no-mistakes.yaml.
func TestRepositoryOverrideReviewAgentsRouteOnRealBinary(t *testing.T) {
	const overrideRemote = "https://example.invalid/acme/raise.git"

	t.Run("matching_remote_reviews_on_the_override", func(t *testing.T) {
		h := NewHarness(t, SetupOpts{
			Agent:             "claude",
			Scenario:          writeReviewAgentsRoutingScenario(t),
			GlobalConfigExtra: reviewAgentsOverrideConfig(overrideRemote),
		})
		run := runReviewFixLoop(t, h, overrideRemote, "feature/override-reviewer")
		turns := captureReviewLoopTurns(t, h)
		assertRecordedReviewLoop(t, h, run.ID, "opencode", "claude")
		assertAgent(t, "initial review", turns.review, "opencode")
		assertAgent(t, "post-fix rereview", turns.rereview, "opencode")
		assertAgent(t, "fixer", turns.fix, "claude")
		assertModelArg(t, "fixer", turns.fix.Args, "global-fixer-e2e")
		assertAgent(t, "non-review step", turns.other, "claude")
		assertModelArg(t, "non-review step", turns.other.Args, "default-override-e2e")
	})

	t.Run("other_remote_keeps_the_global_roles", func(t *testing.T) {
		h := NewHarness(t, SetupOpts{
			Agent:             "claude",
			Scenario:          writeReviewAgentsRoutingScenario(t),
			GlobalConfigExtra: reviewAgentsOverrideConfig(overrideRemote),
		})
		run := runReviewFixLoop(t, h, "https://example.invalid/acme/maislegal.git", "feature/global-reviewer")
		turns := captureReviewLoopTurns(t, h)
		assertRecordedReviewLoop(t, h, run.ID, "claude", "claude")
		assertAgent(t, "initial review", turns.review, "claude")
		assertModelArg(t, "initial review", turns.review.Args, "global-reviewer-e2e")
		assertAgent(t, "post-fix rereview", turns.rereview, "claude")
		assertModelArg(t, "post-fix rereview", turns.rereview.Args, "global-reviewer-e2e")
		assertAgent(t, "fixer", turns.fix, "claude")
		assertModelArg(t, "fixer", turns.fix.Args, "global-fixer-e2e")
	})

	t.Run("pushed_branch_cannot_select_the_override", func(t *testing.T) {
		// Default (all-clean) scenario: the review passes, so the initial review
		// is the only review turn.
		h := NewHarness(t, SetupOpts{
			Agent:             "claude",
			GlobalConfigExtra: reviewAgentsOverrideConfig(overrideRemote),
		})
		const (
			branch   = "feature/pushed-override-attempt"
			remote   = "https://example.invalid/acme/maislegal.git"
			attacker = "attacker-reviewer-e2e"
		)
		if out, err := h.Run("init"); err != nil {
			t.Fatalf("init: %v\n%s", err, out)
		}
		configureGitURLRewrite(t, h, remote, h.UpstreamDir)
		if out, err := h.runGit(context.Background(), h.WorkDir, "remote", "set-url", "origin", remote); err != nil {
			t.Fatalf("set forge-shaped origin: %v\n%s", err, out)
		}
		h.CommitChange(branch, "feature.txt", "contributor change\n", "add feature")
		h.CommitChange(branch, ".no-mistakes.yaml",
			"allow_repo_commands: true\n"+
				"review_agents:\n  reviewer: {agent: claude, model: "+attacker+"}\n"+
				"repository_overrides:\n  "+remote+":\n    review_agents:\n      reviewer: {agent: opencode, model: "+attacker+"}\n",
			"attempt to select the reviewer from the repository")
		h.PushToGate(branch)
		run := h.WaitForRun(branch, 120*time.Second)
		if run.Status != types.RunCompleted {
			t.Fatalf("run status = %q, want completed (error: %v)", run.Status, run.Error)
		}
		review := firstInvocationMatching(h.AgentInvocations(), func(p string) bool {
			return strings.Contains(p, reviewTurnMarker)
		})
		if review == nil {
			t.Fatal("no review turn captured")
		}
		t.Logf("initial review   -> agent=%s model=%s", review.Agent, modelArg(review.Args))
		assertAgent(t, "initial review", review, "claude")
		assertModelArg(t, "initial review", review.Args, "global-reviewer-e2e")
		assertNotModelArg(t, "initial review", review.Args, attacker)
	})
}
