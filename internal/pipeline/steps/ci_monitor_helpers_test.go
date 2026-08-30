package steps

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestCICheckReadFailureOutcome_ProviderNeutral guards the parked finding text:
// it must give provider-agnostic remediation for every supported SCM, not an
// unconditional instruction to install or upgrade `gh`, which is GitHub-only.
func TestCICheckReadFailureOutcome_ProviderNeutral(t *testing.T) {
	t.Parallel()
	outcome := ciCheckReadFailureOutcome(errors.New("glab mr checks: failed to read checks"))
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("findings = %+v, want exactly one finding", findings.Items)
	}
	if findings.Items[0].Action != types.ActionAskUser {
		t.Fatalf("finding action = %q, want ask-user", findings.Items[0].Action)
	}
	desc := findings.Items[0].Description
	if !strings.Contains(desc, "provider CLI or credentials") {
		t.Fatalf("finding %q must give provider-neutral remediation, not a GitHub-only one", desc)
	}
	// The old text instructed verifying gh unconditionally ("verify gh supports
	// 'pr checks --json'") even for GitLab/Bitbucket/Azure errors. gh may only be
	// named as the conditional GitHub-specific clause, never the general remedy.
	if strings.Contains(desc, "verify gh supports") {
		t.Fatalf("finding %q must not instruct verifying gh for a non-GitHub provider", desc)
	}
	// The underlying provider error must survive into the finding.
	if !strings.Contains(desc, "glab mr checks: failed to read checks") {
		t.Fatalf("finding %q must include the underlying provider error", desc)
	}
	// And the GitHub-specific diagnostic is still present for gh-style errors.
	if !strings.Contains(desc, "pr checks --json") || !strings.Contains(desc, "2.50") {
		t.Fatalf("finding %q must keep the GitHub gh version/flag diagnostic", desc)
	}
}

func TestCIMonitorReadinessChangeNotifiesConsumers(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	var changes [][2]bool
	sctx.CIReadinessChanged = func(ready, declaredNoCI bool) {
		changes = append(changes, [2]bool{ready, declaredNoCI})
	}

	logCIMonitorStatus(sctx, ciNoChecksPassedMsg, "")
	clearCIMonitorReady(sctx)

	want := [][2]bool{{true, true}, {false, false}}
	if len(changes) != len(want) {
		t.Fatalf("readiness changes = %v, want %v", changes, want)
	}
	for i := range want {
		if changes[i] != want[i] {
			t.Errorf("readiness change %d = %v, want %v", i, changes[i], want[i])
		}
	}
}
