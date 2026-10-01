package steps

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Exercise the emitted findings JSON contract, not the helper's implementation.
func TestCICheckReadFailureOutcome_SelectorBoundaries(t *testing.T) {
	t.Parallel()
	const missingBranch = "gh pr view: no pull requests found for branch "
	cases := []struct {
		name    string
		message string
		wantSHA bool
	}{
		{"seven characters", missingBranch + "abc1234", true},
		{"forty characters", missingBranch + strings.Repeat("a", 40), true},
		{"uppercase quoted token", "gh pr view: NO PULL REQUESTS FOUND FOR BRANCH ('ABC1234').", true},
		{"one hex letter", missingBranch + "123456a", true},
		{"six characters", missingBranch + "abc123", false},
		{"forty one characters", missingBranch + strings.Repeat("a", 41), false},
		{"seven digit PR", missingBranch + "1000000", false},
		{"forty digit selector", missingBranch + strings.Repeat("1", 40), false},
		{"non hex character", missingBranch + "abc123g", false},
		{"branch path", missingBranch + "feature/abc1234", false},
		{"non SHA branch", missingBranch + "feature/foo", false},
		{"SHA without branch failure", "gh pr checks: permission denied for commit abc1234", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outcome := ciCheckReadFailureOutcome(errors.New(tc.message))
			if !outcome.NeedsApproval || outcome.AutoFixable || outcome.Skipped {
				t.Fatalf("unexpected gate outcome: %+v", outcome)
			}
			var findings types.Findings
			if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
				t.Fatal(err)
			}
			if len(findings.Items) != 1 || findings.Items[0].Action != types.ActionAskUser || findings.Items[0].Severity != "warning" {
				t.Fatalf("unexpected findings: %+v", findings)
			}
			desc := findings.Items[0].Description
			if !strings.Contains(desc, tc.message) {
				t.Fatalf("underlying error lost: %q", desc)
			}
			if got := strings.Contains(desc, "commit SHA was used as the PR selector"); got != tc.wantSHA {
				t.Fatalf("SHA diagnostic = %t, want %t: %q", got, tc.wantSHA, desc)
			}
			if got := strings.Contains(desc, "gh >= 2.50"); got == tc.wantSHA {
				t.Fatalf("generic hint = %t, want %t: %q", got, !tc.wantSHA, desc)
			}
			if tc.wantSHA {
				for _, remedy := range []string{"gh pr list --search <sha>", "commits/<sha>/pulls", "gh pr view <number>", "gh pr checks <number>"} {
					if !strings.Contains(desc, remedy) {
						t.Fatalf("missing remedy %q: %q", remedy, desc)
					}
				}
			}
		})
	}
}
