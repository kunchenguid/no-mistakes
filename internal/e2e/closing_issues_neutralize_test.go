//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// TestClosingIssueRefsPublishedIntentIsNeutralized drives a real run whose
// intent carries closing keywords (short, cross-repo, and URL forms) without
// --closes: the published Intent keeps the text but never a live closing
// reference, so the PR closes nothing on merge.
func TestClosingIssueRefsPublishedIntentIsNeutralized(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	statePath := setupStatefulGitHub(t, h)
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/closes-neutralize"
	h.CommitChange(branch, "n.txt", "n\n", "add neutralize fixture")
	wt := h.AddWorktree(branch)
	intent := "Refactor the widget\nFixes #12\nResolves: acme/widgets#3\nCloses https://github.com/example/closes/issues/14\nfixed http://github.com/example/closes/pull/15"
	if out, err := h.RunInDir(wt, "axi", "run", "--intent", intent, "--skip", "ci"); err != nil {
		t.Fatalf("axi run: %v\n%s", err, out)
	}
	waitPublished(t, h, branch, "", "neutralize")
	body := livePRBody(t, statePath, branch)
	saveEvidence(t, "14-neutralized-intent-pr-body.md", body)
	for _, want := range []string{
		"Fixes `#12`",
		"Resolves: `acme/widgets#3`",
		"Closes `https://github.com/example/closes/issues/14`",
		"fixed `http://github.com/example/closes/pull/15`",
	} {
		if exactLineCount(body, want) != 1 {
			t.Errorf("neutralized line %q not published exactly once; body:\n%s", want, body)
		}
	}
	for _, live := range []string{
		"Fixes #12",
		"Resolves: acme/widgets#3",
		"Closes https://github.com/example/closes/issues/14",
		"fixed http://github.com/example/closes/pull/15",
		"## Issues",
	} {
		if strings.Contains(body, live) {
			t.Errorf("live closing text %q published without --closes; body:\n%s", live, body)
		}
	}
}
