//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func readRunPRFreshness(t *testing.T, nmHome, runID string) (titleFresh, bodyFresh bool) {
	t.Helper()
	database, err := db.Open(paths.WithRoot(nmHome).DB())
	if err != nil {
		t.Fatalf("open e2e db: %v", err)
	}
	defer database.Close()
	run, err := database.GetRun(runID)
	if err != nil || run == nil {
		t.Fatalf("get run %s: %v", runID, err)
	}
	return run.PRTitleFresh, run.PRBodyFresh
}

// editLivePR simulates a maintainer editing the PR on the forge between runs.
func editLivePR(t *testing.T, statePath, branch string, edit func(*statefulGHPR)) {
	t.Helper()
	state := readStatefulGH(t, statePath)
	pr := state.PRs[branch]
	if pr == nil {
		t.Fatalf("no PR recorded for %s", branch)
	}
	edit(pr)
	writeStatefulGH(t, statePath, state)
}

// assertPipelineOwnedTail checks the final body still carries the pipeline's
// appendix and attestation exactly once after a supplied-text update.
func assertPipelineOwnedTail(t *testing.T, label, body string) {
	t.Helper()
	for _, marker := range []string{"<!-- no-mistakes-pr-appendix:v1", "no-mistakes-pipeline-attestation"} {
		if n := strings.Count(body, marker); n < 1 {
			t.Errorf("%s: body missing %q; body:\n%s", label, marker, body)
		}
	}
	if n := strings.Count(body, "<!-- no-mistakes-pr-appendix:v1"); n > 1 {
		t.Errorf("%s: appendix marker repeated %d times; body:\n%s", label, n, body)
	}
}

// TestSuppliedPRTextExistingPRJourney drives issue #1762 through the real
// binary, daemon, and stateful fake gh: on an already open PR, a freshly
// passed --pr-title and/or --pr-body-file updates exactly the named field and
// keeps the pipeline-owned appendix and attestation, while a plain rerun or an
// axi retry that only inherits stored text never rewrites maintainer edits.
func TestSuppliedPRTextExistingPRJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	statePath := setupStatefulGitHub(t, h)
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	scratch := t.TempDir()
	const branch = "feature/existing-pr-text"
	h.CommitChange(branch, "existing.txt", "existing\n", "add existing fixture")
	wt := h.AddWorktree(branch)

	// 0. A PR already exists with drafted text (no supplied flags).
	if out, err := h.RunInDir(wt, "axi", "run", "--intent", "existing pr text", "--skip", "ci"); err != nil {
		t.Fatalf("axi run (create): %v\n%s", err, out)
	}
	prev := waitPublished(t, h, branch, "", "create drafted")
	pr := livePR(t, statePath, branch)
	saveSuppliedEvidence(t, "e0-created-drafted.md", "TITLE: "+pr.Title+"\n\n"+pr.Body)
	draftedTitle := pr.Title

	// 1. No fresh flags: a plain rerun keeps the configured update_text
	//    behavior and stores no supplied text.
	if out, err := h.RunInDir(wt, "rerun"); err != nil {
		t.Fatalf("plain rerun: %v\n%s", err, out)
	}
	prev = waitPublished(t, h, branch, prev.ID, "no flags")
	if gt, gb := readRunPRText(t, h.NMHome, prev.ID); gt != nil || gb != nil {
		t.Errorf("no-flag rerun stored PR text %v/%v", gt, gb)
	}
	pr = livePR(t, statePath, branch)
	saveSuppliedEvidence(t, "e1-no-flags.md", "TITLE: "+pr.Title+"\n\n"+pr.Body)
	if pr.Title != draftedTitle {
		t.Errorf("no-flag rerun title = %q, want drafted %q", pr.Title, draftedTitle)
	}

	// 2. Fresh title only: the existing PR's title changes; body stays drafted.
	const titleA = "feat(existing): Fresh title only"
	if out, err := h.RunInDir(wt, "rerun", "--pr-title", titleA); err != nil {
		t.Fatalf("rerun --pr-title: %v\n%s", err, out)
	}
	prev = waitPublished(t, h, branch, prev.ID, "title only")
	if tf, bf := readRunPRFreshness(t, h.NMHome, prev.ID); !tf || bf {
		t.Errorf("title-only freshness = %v/%v, want true/false", tf, bf)
	}
	pr = livePR(t, statePath, branch)
	saveSuppliedEvidence(t, "e2-title-only.md", "TITLE: "+pr.Title+"\n\n"+pr.Body)
	if pr.Title != titleA {
		t.Errorf("title-only: PR title = %q, want %q", pr.Title, titleA)
	}
	if !strings.Contains(pr.Body, "no-mistakes-pipeline-attestation") {
		t.Errorf("title-only: attestation missing; body:\n%s", pr.Body)
	}

	// 3. Fresh body only, after a maintainer retitled the PR: the narrative
	//    is replaced, the appendix/attestation stay valid, and the inherited
	//    title does not overwrite the maintainer's title.
	const maintainerTitle = "Maintainer retitle"
	editLivePR(t, statePath, branch, func(p *statefulGHPR) { p.Title = maintainerTitle })
	bodyB := "## Summary\n\nFresh body only narrative.\n"
	bodyBPath := filepath.Join(scratch, "b.md")
	if err := os.WriteFile(bodyBPath, []byte(bodyB), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := h.RunInDir(wt, "rerun", "--pr-body-file", bodyBPath); err != nil {
		t.Fatalf("rerun --pr-body-file: %v\n%s", err, out)
	}
	prev = waitPublished(t, h, branch, prev.ID, "body only")
	if tf, bf := readRunPRFreshness(t, h.NMHome, prev.ID); tf || !bf {
		t.Errorf("body-only freshness = %v/%v, want false/true (title inherited)", tf, bf)
	}
	pr = livePR(t, statePath, branch)
	saveSuppliedEvidence(t, "e3-body-only.md", "TITLE: "+pr.Title+"\n\n"+pr.Body)
	if pr.Title != maintainerTitle {
		t.Errorf("body-only: inherited title rewrote PR title to %q, want maintainer's %q", pr.Title, maintainerTitle)
	}
	if !strings.HasPrefix(pr.Body, strings.TrimRight(bodyB, "\n")) {
		t.Errorf("body-only: body does not lead with supplied narrative; body:\n%s", pr.Body)
	}
	assertPipelineOwnedTail(t, "body-only", pr.Body)

	// 4. Both fresh: both fields replaced; previous narrative gone.
	const titleC = "feat(existing): Both fields fresh"
	bodyC := "## Summary\n\nBoth fields fresh narrative.\n"
	bodyCPath := filepath.Join(scratch, "c.md")
	if err := os.WriteFile(bodyCPath, []byte(bodyC), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := h.RunInDir(wt, "rerun", "--pr-title", titleC, "--pr-body-file", bodyCPath); err != nil {
		t.Fatalf("rerun both: %v\n%s", err, out)
	}
	prev = waitPublished(t, h, branch, prev.ID, "both")
	pr = livePR(t, statePath, branch)
	saveSuppliedEvidence(t, "e4-both.md", "TITLE: "+pr.Title+"\n\n"+pr.Body)
	if pr.Title != titleC {
		t.Errorf("both: PR title = %q, want %q", pr.Title, titleC)
	}
	if !strings.HasPrefix(pr.Body, strings.TrimRight(bodyC, "\n")) || strings.Contains(pr.Body, "Fresh body only narrative.") {
		t.Errorf("both: body not replaced by supplied narrative; body:\n%s", pr.Body)
	}
	assertPipelineOwnedTail(t, "both", pr.Body)

	// 5. Plain rerun inheriting stored text after maintainer edits: the
	//    inherited title/description are stored but must not rewrite the PR.
	const maintainerTitle2 = "Maintainer second retitle"
	editLivePR(t, statePath, branch, func(p *statefulGHPR) {
		p.Title = maintainerTitle2
		p.Body = strings.Replace(p.Body, "Both fields fresh narrative.", "Maintainer edited narrative.", 1)
	})
	if out, err := h.RunInDir(wt, "rerun"); err != nil {
		t.Fatalf("inheriting rerun: %v\n%s", err, out)
	}
	prev = waitPublished(t, h, branch, prev.ID, "inherit rerun")
	gt, gb := readRunPRText(t, h.NMHome, prev.ID)
	if gt == nil || *gt != titleC || gb == nil || *gb != bodyC {
		t.Errorf("inheriting rerun stored %v/%v, want inherited %q/%q", gt, gb, titleC, bodyC)
	}
	if tf, bf := readRunPRFreshness(t, h.NMHome, prev.ID); tf || bf {
		t.Errorf("inheriting rerun marked text fresh: %v/%v", tf, bf)
	}
	pr = livePR(t, statePath, branch)
	saveSuppliedEvidence(t, "e5-inherit-rerun.md", "TITLE: "+pr.Title+"\n\n"+pr.Body)
	if pr.Title != maintainerTitle2 {
		t.Errorf("inherit rerun: title = %q, want maintainer's %q", pr.Title, maintainerTitle2)
	}
	if !strings.Contains(pr.Body, "Maintainer edited narrative.") || strings.Contains(pr.Body, "Both fields fresh narrative.") {
		t.Errorf("inherit rerun: maintainer narrative not kept; body:\n%s", pr.Body)
	}
	assertPipelineOwnedTail(t, "inherit rerun", pr.Body)

	// 6. Retry inheritance: a fresh-title run fails on an agent invocation
	//    error (provider outage) before the PR step, then axi retry continues
	//    it with inherited (non-fresh) text, which must not rewrite the
	//    existing PR's title. fakeagent dispatches on argv[0], so the shim
	//    execs a claude-named link.
	agentLink := filepath.Join(h.BinDir, executableName("claude"))
	fakeBin, err := os.Readlink(agentLink)
	if err != nil {
		t.Fatalf("readlink claude: %v", err)
	}
	healthyDir := filepath.Join(h.BinDir, "healthy")
	if err := os.MkdirAll(healthyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	healthy := filepath.Join(healthyDir, executableName("claude"))
	if err := os.Symlink(fakeBin, healthy); err != nil {
		t.Fatal(err)
	}
	failFlag := filepath.Join(h.BinDir, "provider-down")
	script := "#!/bin/sh\nif [ -f " + shellQuote(failFlag) + " ]; then echo 'provider processing error' >&2; exit 1; fi\nexec " + shellQuote(healthy) + " \"$@\"\n"
	if err := os.Remove(agentLink); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agentLink, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(failFlag, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	const titleD = "feat(existing): Fresh title before outage"
	if out, err := h.RunInDir(wt, "rerun", "--pr-title", titleD); err != nil {
		t.Fatalf("rerun --pr-title (outage): %v\n%s", err, out)
	}
	var failed *ipc.RunInfo
	for deadline := time.Now().Add(3 * time.Minute); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		for _, r := range h.Runs() {
			if r.Branch == branch && r.ID != prev.ID {
				r := r
				failed = &r
				break
			}
		}
		if failed == nil {
			continue
		}
		if st, _ := findStep(failed.Steps, types.StepPR); failed.Status.Terminal() || (st.Status != types.StepStatusPending && st.Status != types.StepStatusRunning) {
			break
		}
	}
	if failed == nil {
		t.Fatal("outage run never appeared")
	}
	t.Logf("outage run %s status=%s", failed.ID, failed.Status)
	if failed.Status != types.RunFailed {
		for _, st := range failed.Steps {
			t.Logf("step %s status=%s err=%v", st.StepName, st.Status, deref(st.Error))
		}
		t.Fatalf("outage run status = %s, want failed (error=%v)", failed.Status, deref(failed.Error))
	}
	if err := os.Remove(failFlag); err != nil {
		t.Fatal(err)
	}
	if pr = livePR(t, statePath, branch); pr.Title != maintainerTitle2 {
		t.Fatalf("outage run changed title to %q", pr.Title)
	}
	if out, err := h.RunInDir(wt, "axi", "sync", "--recover"); err != nil {
		t.Logf("axi sync --recover: %v\n%s", err, out)
	}
	// axi retry follows the run (into CI monitoring), so it runs alongside
	// waitPublished, which cancels the run once its PR step completes.
	type cmdResult struct {
		out string
		err error
	}
	retryDone := make(chan cmdResult, 1)
	go func() {
		out, err := h.RunInDir(wt, "axi", "retry", "--run", failed.ID)
		retryDone <- cmdResult{out, err}
	}()
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(300 * time.Millisecond) {
		if runs := h.Runs(); len(runs) > 0 && runs[0].Branch == branch && runs[0].RetryOf != nil && runs[0].RetryOf.RunID == failed.ID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("retry run never appeared")
		}
	}
	retried := waitPublished(t, h, branch, failed.ID, "retry")
	select {
	case res := <-retryDone:
		saveSuppliedEvidence(t, "e6-axi-retry-output.txt", res.out)
		if strings.Contains(res.out, "refusing retry") {
			t.Fatalf("axi retry refused: %v\n%s", res.err, res.out)
		}
	case <-time.After(90 * time.Second):
		t.Errorf("axi retry did not return after its run was settled")
	}
	if retried.RetryOf == nil || retried.RetryOf.RunID != failed.ID {
		t.Errorf("retry did not continue failed run: %+v", retried.RetryOf)
	}
	gt, _ = readRunPRText(t, h.NMHome, retried.ID)
	if gt == nil || *gt != titleD {
		t.Errorf("retry stored title %v, want inherited %q", gt, titleD)
	}
	if tf, bf := readRunPRFreshness(t, h.NMHome, retried.ID); tf || bf {
		t.Errorf("retry marked inherited text fresh: %v/%v", tf, bf)
	}
	pr = livePR(t, statePath, branch)
	saveSuppliedEvidence(t, "e6-retry.md", "TITLE: "+pr.Title+"\n\n"+pr.Body)
	if pr.Title != maintainerTitle2 {
		t.Errorf("retry: inherited title rewrote PR title to %q, want %q", pr.Title, maintainerTitle2)
	}
	assertPipelineOwnedTail(t, "retry", pr.Body)
}
