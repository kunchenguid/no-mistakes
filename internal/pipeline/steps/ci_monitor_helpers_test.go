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
	// A non-version error must not be blamed on the gh version.
	if strings.Contains(desc, "2.50") || strings.Contains(desc, "pr checks --json") {
		t.Fatalf("finding %q must not blame the gh version for an unrelated error", desc)
	}
}

// TestCICheckReadFailureOutcome_GHVersionHintOnlyForJSONFlagRejection keeps the
// gh >= 2.50 hint for the one case it describes: gh rejecting `pr checks --json`.
func TestCICheckReadFailureOutcome_GHVersionHintOnlyForJSONFlagRejection(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		err      string
		wantHint bool
	}{
		{"old gh unknown flag", "gh pr checks: unknown flag: --json (exit 1)", true},
		{"forgejo-axi unknown json flag", "forgejo-axi pr checks: unknown flag: --json (exit 1)", false},
		{"provider plugin unknown json flag", "provider plugin pr checks: unknown flag: --json", false},
		{"no pull requests found", "gh pr view: no pull requests found for branch 0123abcd (exit 1)", false},
		{"auth failure", "gh: HTTP 401: Bad credentials", false},
		{"unrelated unknown flag", "gh: unknown flag: --repo", false},
		{"invalid json output", "parse gh checks output: invalid character 'o' in literal null", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			outcome := ciCheckReadFailureOutcome(errors.New(tc.err))
			var findings Findings
			if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
				t.Fatalf("unmarshal findings: %v", err)
			}
			desc := findings.Items[0].Description
			if !strings.Contains(desc, tc.err) {
				t.Fatalf("finding %q must include the underlying error", desc)
			}
			if got := strings.Contains(desc, "gh >= 2.50"); got != tc.wantHint {
				t.Fatalf("gh version hint present = %v, want %v in %q", got, tc.wantHint, desc)
			}
		})
	}
}

func TestCIMonitorReadinessChangeNotifiesConsumers(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	var changes [][2]bool
	sctx.CIReadinessChanged = func(ready, declaredNoCI bool) {
		changes = append(changes, [2]bool{ready, declaredNoCI})
	}

	if _, err := logCIMonitorStatus(sctx, ciNoChecksPassedMsg, ciMonitorStatus{}); err != nil {
		t.Fatal(err)
	}
	if err := clearCIMonitorReady(sctx); err != nil {
		t.Fatal(err)
	}

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
