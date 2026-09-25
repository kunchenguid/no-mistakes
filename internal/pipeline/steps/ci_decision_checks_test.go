package steps

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

func TestSplitDecisionChecks(t *testing.T) {
	cfg := config.CI{DecisionChecks: []string{"workflow pin*"}}
	decision, repairable := splitDecisionChecks([]string{"build", "workflow pin / verify", "test"}, cfg)
	if len(decision) != 1 || decision[0] != "workflow pin / verify" {
		t.Fatalf("decision checks = %v", decision)
	}
	if len(repairable) != 2 || repairable[0] != "build" || repairable[1] != "test" {
		t.Fatalf("repairable checks = %v", repairable)
	}
	if d, r := splitDecisionChecks([]string{"build"}, config.CI{}); len(d) != 0 || len(r) != 1 {
		t.Fatalf("an unconfigured repository must leave every check repairable: %v / %v", d, r)
	}
}

// TestCIStep_DecisionCheckLeavesOrdinaryFailuresRepairable keeps the policy
// narrow: only the declared names leave the fix agent's reach.
func TestCIStep_DecisionCheckLeavesOrdinaryFailuresRepairable(t *testing.T) {
	t.Parallel()
	cfg := config.CI{DecisionChecks: []string{"workflow pin*"}}
	names := []string{"unit tests (ubuntu-latest)", "Workflow Pin / verify"}
	decision, repairable := splitDecisionChecks(names, cfg)
	if len(decision) != 1 || decision[0] != "Workflow Pin / verify" {
		t.Fatalf("decision = %v", decision)
	}
	if len(repairable) != 1 || repairable[0] != "unit tests (ubuntu-latest)" {
		t.Fatalf("repairable = %v", repairable)
	}
}
