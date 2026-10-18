//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	postReviewDocPath    = "docs/usage.md"
	postReviewFalseClaim = "Run it with --dry-run to preview every change.\n"
	postReviewCorrection = "Run it to apply every change.\n"
	postReviewFinding    = "documents a --dry-run flag the CLI does not have"
)

// postReviewPassScenario has Document commit a false claim after Review
// approved the change. Only a review turn framed as a post-review pass reports
// it; its fix round corrects the claim and the rereview is clean.
func postReviewPassScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "post-review-pass.yaml")
	content := `actions:
  - match: "Investigate previous review findings"
    edits:
      - path: "` + postReviewDocPath + `"
        new: "` + strings.TrimSuffix(postReviewCorrection, "\n") + `\n"
    structured:
      findings: []
      summary: "drop the nonexistent dry-run flag"
  - match: "Fix-round provenance:"
    structured:
      findings: []
      summary: "fix verified"
      risk_level: low
      risk_rationale: "the documentation now matches the CLI"
      risk_scope: source-or-external
  - match: "Post-review pass:"
    structured:
      findings:
        - id: doc-false-claim
          severity: error
          file: "` + postReviewDocPath + `"
          line: 1
          description: "` + postReviewFinding + `"
          action: auto-fix
      summary: "the documentation commit makes a false claim"
      risk_level: medium
      risk_rationale: "documentation contradicts the code"
      risk_scope: source-or-external
  - match: "Perform the combined documentation and lint housekeeping pass for this change."
    edits:
      - path: "` + postReviewDocPath + `"
        new: "` + strings.TrimSuffix(postReviewFalseClaim, "\n") + `\n"
    structured:
      findings: []
      summary: "document usage"
  - structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no risks detected in the diff"
      risk_scope: source-or-external
      tested:
        - "fakeagent: simulated test run"
      testing_summary: "simulated tests passed"
      scenarios:
        - name: "fakeagent: simulated end-to-end scenario"
          result: pass
          live: true
          evidence: "fakeagent: simulated test run"
          reason: ""
      verdict: go
      artifacts: []
      title: "feat: fakeagent change"
      body: "## Summary\nfakeagent canned PR body"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write post-review pass scenario: %v", err)
	}
	return path
}

func reviewStepInfo(t *testing.T, run *ipc.RunInfo) ipc.StepResultInfo {
	t.Helper()
	for _, s := range run.Steps {
		if s.StepName == types.StepReview {
			return s
		}
	}
	t.Fatal("run has no review step")
	return ipc.StepResultInfo{}
}

// TestPostReviewPassJourney: with review.post_review_pass on the trusted
// default branch, a Document commit made after Review is reviewed before Push
// publishes it, the pass's finding is auto-fixed through the ordinary review
// loop, and the published head is the head Review approved. With the setting
// off, the same false claim ships unreviewed and axi status counts the commit.
func TestPostReviewPassJourney(t *testing.T) {
	for _, tc := range []struct {
		name string
		on   bool
	}{{name: "on", on: true}, {name: "off", on: false}} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: postReviewPassScenario(t)})
			config := "auto_fix:\n  review: 1\n"
			if tc.on {
				config += "review:\n  post_review_pass: true\n"
			}
			pushMainRepoConfig(t, h, config)
			if out, err := h.Run("init"); err != nil {
				t.Fatalf("nm init: %v\n%s", err, out)
			}

			branch := "feature/post-review-" + tc.name
			h.CommitChange(branch, "tool.sh", "#!/bin/sh\necho apply\n", "add tool")
			h.PushToGate(branch)
			run := h.WaitForRun(branch, 180*time.Second)
			if run.Status != types.RunCompleted {
				t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
			}

			published := h.UpstreamBranchSHA(branch)
			if published != run.HeadSHA {
				t.Fatalf("published head %s != run head %s", published, run.HeadSHA)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			doc, err := h.runGit(ctx, h.UpstreamDir, "show", published+":"+postReviewDocPath)
			if err != nil {
				t.Fatalf("read published %s: %v\n%s", postReviewDocPath, err, doc)
			}
			passTurns := 0
			for _, inv := range h.AgentInvocations() {
				if strings.Contains(inv.Prompt, "Post-review pass:") && !strings.Contains(inv.Prompt, "Investigate previous review findings") {
					passTurns++
				}
			}
			review := reviewStepInfo(t, run)
			status, err := h.Run("axi", "status", "--run", run.ID)
			if err != nil {
				t.Fatalf("axi status: %v\n%s", err, status)
			}

			if tc.on {
				if string(doc) != postReviewCorrection {
					t.Fatalf("published %s = %q, want the pass's correction %q", postReviewDocPath, doc, postReviewCorrection)
				}
				// The pass's own turn and the rereview after its fix round.
				if passTurns != 2 {
					t.Fatalf("post-review pass review turns = %d, want 2", passTurns)
				}
				if review.RoundCount != 3 || review.FixRoundCount != 1 {
					t.Fatalf("review rounds = %d (fix %d), want initial + post_review + auto_fix", review.RoundCount, review.FixRoundCount)
				}
				if strings.Contains(status, "post_review_commits") {
					t.Fatalf("a run whose pass certified the published head still reports commits after review:\n%s", status)
				}
			} else {
				if string(doc) != postReviewFalseClaim {
					t.Fatalf("published %s = %q, want the unreviewed Document commit %q", postReviewDocPath, doc, postReviewFalseClaim)
				}
				if passTurns != 0 || review.RoundCount != 1 {
					t.Fatalf("setting off ran a post-review pass: turns=%d review rounds=%d", passTurns, review.RoundCount)
				}
				if !strings.Contains(status, "post_review_commits: 1\n") {
					t.Fatalf("axi status does not count the unreviewed Document commit:\n%s", status)
				}
			}
			t.Logf("%s: published %s; review rounds=%d; axi status:\n%s", tc.name, published, review.RoundCount, status)
		})
	}
}
