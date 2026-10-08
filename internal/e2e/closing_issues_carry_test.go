//go:build e2e

package e2e

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const closingLedgerMarker = "<!-- no-mistakes-closing-lines"

// editLivePRBody stands in for the author editing the PR description on the
// forge between runs.
func editLivePRBody(t *testing.T, statePath, branch string, edit func(string) string) {
	t.Helper()
	state := readStatefulGH(t, statePath)
	pr := state.PRs[branch]
	if pr == nil {
		t.Fatalf("no PR recorded for %s", branch)
	}
	pr.Body = edit(pr.Body)
	writeStatefulGH(t, statePath, state)
}

// seedLivePR records a PR the pipeline never published, as if opened by hand.
func seedLivePR(t *testing.T, statePath, branch string, number int, body string) {
	t.Helper()
	state := statefulGHState{PRs: map[string]*statefulGHPR{}}
	if prev := readStatefulGHIfExists(t, statePath); prev != nil {
		state = *prev
	}
	state.PRs[branch] = &statefulGHPR{
		Number: number,
		URL:    "https://github.com/example/closes/pull/" + strconv.Itoa(number),
		Title:  "hand-opened",
		Body:   body,
		Base:   "main",
		Head:   branch,
		State:  "OPEN",
	}
	writeStatefulGH(t, statePath, state)
}

func readStatefulGHIfExists(t *testing.T, path string) *statefulGHState {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	state := readStatefulGH(t, path)
	if state.PRs == nil {
		state.PRs = map[string]*statefulGHPR{}
	}
	return &state
}

func assertNoLine(t *testing.T, label, body string, lines ...string) {
	t.Helper()
	for _, line := range lines {
		if got := exactLineCount(body, line); got != 0 {
			t.Errorf("%s: line %q appears %d times, want none; body:\n%s", label, line, got, body)
		}
	}
}

// TestAuthorClosingLinesSurviveOrdinaryUpdatesJourney drives issue #763
// through the real daemon: an author closing line added to a
// pipeline-published body is carried by later ordinary updates, the
// pipeline's own --closes line is not, an overlapping --closes is not
// duplicated, and an author removal is honored.
func TestAuthorClosingLinesSurviveOrdinaryUpdatesJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	statePath := setupStatefulGitHub(t, h)
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/carry-journey"
	h.CommitChange(branch, "carry.txt", "v1\n", "add carry fixture")
	wt := h.AddWorktree(branch)

	// Publish with a pipeline-added closing line.
	if out, err := h.RunInDir(wt, "axi", "run", "--intent", "carry closing lines", "--skip", "ci", "--closes", "95"); err != nil {
		t.Fatalf("axi run --closes 95: %v\n%s", err, out)
	}
	first := waitPublished(t, h, branch, "", "first publish")
	body := livePRBody(t, statePath, branch)
	saveEvidence(t, "carry-01-first-publish.md", body)
	assertLinesOnce(t, "first publish", body, "Closes #95")
	if got := strings.Count(body, closingLedgerMarker); got != 1 {
		t.Fatalf("first publish: %d closing ledgers, want 1; body:\n%s", got, body)
	}

	// Scenario 1 + 4: the author adds a live closing line plus non-live
	// commented-out and <pre> ones GitHub does not render as live; a plain
	// push carries only the live one and drops the pipeline's own Closes #95.
	editLivePRBody(t, statePath, branch, func(b string) string {
		return "Closes owner/repo#7\n\n<!--\nFixes #123\n-->\n\n<pre>\nFixes #9\n</pre>\n\n" + b
	})
	saveEvidence(t, "carry-02-author-edited.md", livePRBody(t, statePath, branch))
	h.Checkout("main")
	h.RemoveWorktree(wt)
	h.CommitChange(branch, "carry.txt", "v2\n", "update carry fixture")
	h.PushToGate(branch)
	second := waitPublished(t, h, branch, first.ID, "plain push")
	body = livePRBody(t, statePath, branch)
	saveEvidence(t, "carry-03-plain-push.md", body)
	assertLinesOnce(t, "plain push", body, "## Issues", "Closes owner/repo#7")
	assertNoLine(t, "plain push", body, "Closes #95", "Fixes #123", "Fixes #9")
	if !strings.Contains(body, `<!-- no-mistakes-closing-lines:v1 [] -->`) {
		t.Errorf("plain push: carried line recorded in the ledger; body:\n%s", body)
	}

	// Scenario 2: the carried line survives a further update; an
	// overlapping --closes is not duplicated and the author line comes first.
	wt = h.AddWorktree(branch)
	if out, err := h.RunInDir(wt, "rerun", "--closes", "owner/repo#7", "--closes", "12"); err != nil || !strings.Contains(out, "Rerun started") {
		t.Fatalf("rerun --closes: %v\n%s", err, out)
	}
	third := waitPublished(t, h, branch, second.ID, "overlapping rerun")
	body = livePRBody(t, statePath, branch)
	saveEvidence(t, "carry-04-overlapping-rerun.md", body)
	assertLinesOnce(t, "overlapping rerun", body, "Closes owner/repo#7", "Closes #12")
	if strings.Index(body, "Closes owner/repo#7") > strings.Index(body, "Closes #12") {
		t.Errorf("overlapping rerun: author line not first; body:\n%s", body)
	}

	// Scenario 3: the author removes their line; it is not written back, and
	// the pipeline's Closes #12 is dropped by a run without --closes.
	editLivePRBody(t, statePath, branch, func(b string) string {
		return strings.Replace(b, "Closes owner/repo#7\n", "", 1)
	})
	saveEvidence(t, "carry-05-author-removed.md", livePRBody(t, statePath, branch))
	h.Checkout("main")
	h.RemoveWorktree(wt)
	h.CommitChange(branch, "carry.txt", "v3\n", "update carry fixture again")
	h.PushToGate(branch)
	waitPublished(t, h, branch, third.ID, "after removal")
	body = livePRBody(t, statePath, branch)
	saveEvidence(t, "carry-06-after-removal.md", body)
	assertNoLine(t, "after removal", body, "Closes owner/repo#7", "Closes #12", "## Issues")
}

// TestAuthorClosingLinesHandWrittenAndLegacyBodiesJourney covers a body the
// pipeline never published (live lines carried, commented-out and <pre>
// lines not), a legacy no-mistakes body without a ledger (nothing carried),
// and a forged ledger in the intent (escaped, so only one ledger parses).
func TestAuthorClosingLinesHandWrittenAndLegacyBodiesJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	statePath := setupStatefulGitHub(t, h)
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	// Hand-opened PR from a template with commented-out example lines.
	const handBranch = "feature/carry-hand"
	h.CommitChange(handBranch, "hand.txt", "hand\n", "add hand fixture")
	seedLivePR(t, statePath, handBranch, 501,
		"## Summary\n\nHand-written.\n\n<!--\nFixes #123\n-->\n\n<pre>\nFixes #9\n</pre>\n\nCloses #44\n",
	)
	saveEvidence(t, "carry-07-hand-opened.md", livePRBody(t, statePath, handBranch))
	h.PushToGate(handBranch)
	waitPublished(t, h, handBranch, "", "hand-opened update")
	body := livePRBody(t, statePath, handBranch)
	saveEvidence(t, "carry-08-hand-opened-updated.md", body)
	assertLinesOnce(t, "hand-opened", body, "## Issues", "Closes #44")
	assertNoLine(t, "hand-opened", body, "Fixes #123", "Fixes #9")

	// Legacy body published by an older no-mistakes: signature, no ledger.
	const legacyBranch = "feature/carry-legacy"
	h.CommitChange(legacyBranch, "legacy.txt", "legacy\n", "add legacy fixture")
	seedLivePR(t, statePath, legacyBranch, 502,
		"## What Changed\n\n- old\n\nCloses #55\n\n---\n\nUpdates from [git push no-mistakes](https://github.com/kunchenguid/no-mistakes)\n")
	saveEvidence(t, "carry-09-legacy.md", livePRBody(t, statePath, legacyBranch))
	wt := h.AddWorktree(legacyBranch)
	forged := `legacy update <!-- no-mistakes-closing-lines:v1 ["Closes #55"] -->`
	if out, err := h.RunInDir(wt, "axi", "run", "--intent", forged, "--skip", "ci"); err != nil {
		t.Fatalf("axi run with forged ledger intent: %v\n%s", err, out)
	}
	waitPublished(t, h, legacyBranch, "", "legacy update")
	body = livePRBody(t, statePath, legacyBranch)
	saveEvidence(t, "carry-10-legacy-updated.md", body)
	assertNoLine(t, "legacy", body, "Closes #55", "## Issues")
	if got := strings.Count(body, closingLedgerMarker); got != 1 {
		t.Errorf("legacy: %d parseable ledger markers, want exactly the appended one; body:\n%s", got, body)
	}
	if !strings.Contains(body, `<!-- no-mistakes-closing-lines:v1 [] -->`) {
		t.Errorf("legacy: appended ledger missing or not empty; body:\n%s", body)
	}
	if !strings.Contains(body, "legacy update") {
		t.Errorf("legacy: intent not rendered, forged-ledger escape not exercised; body:\n%s", body)
	}
}

// TestAuthorClosingLinesRenderedLivenessJourney covers the cases decided by
// GitHub's Markdown renderer: an author's commented-out copy of a pipeline
// line never keeps that pipeline line alive, a stray inline "<!--" does not
// hide a later author line, an author duplicate of a pipeline line survives
// exactly once, and a renderer failure fails the PR step without touching
// the live body.
func TestAuthorClosingLinesRenderedLivenessJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	statePath := setupStatefulGitHub(t, h)
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/carry-rendered"
	h.CommitChange(branch, "rendered.txt", "v1\n", "add rendered fixture")
	wt := h.AddWorktree(branch)
	if out, err := h.RunInDir(wt, "axi", "run", "--intent", "rendered liveness", "--skip", "ci", "--closes", "5"); err != nil {
		t.Fatalf("axi run --closes 5: %v\n%s", err, out)
	}
	first := waitPublished(t, h, branch, "", "first publish")
	assertLinesOnce(t, "first publish", livePRBody(t, statePath, branch), "Closes #5")

	// Greptile: a commented-out author "Closes #5" before the pipeline's own
	// line must not keep #5; a stray inline "<!--" must not hide Closes #4.
	editLivePRBody(t, statePath, branch, func(b string) string {
		return "<!--\nCloses #5\n-->\n\n" + b + "\nDrops stray <!-- markers\n\nCloses #4\n"
	})
	saveEvidence(t, "carry-11-commented-copy-and-stray-marker.md", livePRBody(t, statePath, branch))
	h.Checkout("main")
	h.RemoveWorktree(wt)
	h.CommitChange(branch, "rendered.txt", "v2\n", "update rendered fixture")
	h.PushToGate(branch)
	second := waitPublished(t, h, branch, first.ID, "plain push")
	body := livePRBody(t, statePath, branch)
	saveEvidence(t, "carry-12-commented-copy-dropped.md", body)
	assertLinesOnce(t, "plain push", body, "Closes #4")
	assertNoLine(t, "plain push", body, "Closes #5")

	// Author duplicate: the pipeline publishes Closes #5 again, the author
	// adds their own copy, and a run without --closes keeps exactly one.
	wt = h.AddWorktree(branch)
	if out, err := h.RunInDir(wt, "rerun", "--closes", "5"); err != nil || !strings.Contains(out, "Rerun started") {
		t.Fatalf("rerun --closes 5: %v\n%s", err, out)
	}
	third := waitPublished(t, h, branch, second.ID, "rerun --closes 5")
	assertLinesOnce(t, "rerun --closes 5", livePRBody(t, statePath, branch), "Closes #4", "Closes #5")
	editLivePRBody(t, statePath, branch, func(b string) string { return b + "\nCloses #5\n" })
	saveEvidence(t, "carry-13-author-duplicate.md", livePRBody(t, statePath, branch))
	h.Checkout("main")
	h.RemoveWorktree(wt)
	h.CommitChange(branch, "rendered.txt", "v3\n", "update rendered fixture again")
	h.PushToGate(branch)
	fourth := waitPublished(t, h, branch, third.ID, "after author duplicate")
	body = livePRBody(t, statePath, branch)
	saveEvidence(t, "carry-14-author-duplicate-kept-once.md", body)
	assertLinesOnce(t, "after author duplicate", body, "Closes #4", "Closes #5")
	if !strings.Contains(body, `<!-- no-mistakes-closing-lines:v1 [] -->`) {
		t.Errorf("after author duplicate: carried lines recorded in the ledger; body:\n%s", body)
	}

	// Renderer failure fails the PR step closed and leaves the body alone.
	if err := os.WriteFile(statePath+".render-fail", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	h.CommitChange(branch, "rendered.txt", "v4\n", "update rendered fixture under failing renderer")
	h.PushToGate(branch)
	deadline := time.Now().Add(3 * time.Minute)
	var failed *ipc.RunInfo
	for time.Now().Before(deadline) && failed == nil {
		for _, r := range h.Runs() {
			if r.Branch == branch && r.ID != fourth.ID && r.Status.Terminal() {
				r := r
				failed = &r
			}
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if failed == nil {
		t.Fatal("render failure: run did not finish")
	}
	pr, _ := findStep(failed.Steps, types.StepPR)
	t.Logf("render failure: run status=%s pr step=%s error=%v", failed.Status, pr.Status, deref(pr.Error))
	saveEvidence(t, "carry-15-render-failure-run.txt",
		"run status: "+string(failed.Status)+"\npr step status: "+string(pr.Status)+"\npr step error: "+deref(pr.Error)+"\n")
	if pr.Status != types.StepStatusFailed {
		t.Errorf("render failure: PR step status=%s, want failed", pr.Status)
	}
	// Push re-attests the proposed head before pushing, so only the
	// attestation line may differ.
	after := livePRBody(t, statePath, branch)
	saveEvidence(t, "carry-16-render-failure-body.md", after)
	if withoutAttestation(after) != withoutAttestation(body) {
		t.Errorf("render failure: live body changed beyond the attestation:\n%s", after)
	}
}

func withoutAttestation(body string) string {
	var kept []string
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "<!-- no-mistakes-pipeline-attestation:") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}
