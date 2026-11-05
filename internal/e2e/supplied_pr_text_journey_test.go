//go:build e2e

package e2e

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func readRunPRText(t *testing.T, nmHome, runID string) (title, body *string) {
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
	return run.PRTitle, run.PRBody
}

func livePR(t *testing.T, statePath, branch string) *statefulGHPR {
	t.Helper()
	pr := readStatefulGH(t, statePath).PRs[branch]
	if pr == nil {
		t.Fatalf("no PR recorded for %s", branch)
	}
	return pr
}

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err == nil {
		return 0
	}
	return -1
}

// TestSuppliedPRTextJourney drives issue #1699 end to end through the real
// binary, daemon, and a stateful fake gh: a caller-supplied PR title and
// description are refused loudly when invalid, published verbatim above the
// appendix on create, persisted across a daemon restart, inherited by rerun,
// and kept byte for byte by a later plain gate push.
func TestSuppliedPRTextJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	statePath := setupStatefulGitHub(t, h)
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	scratch := t.TempDir()

	const branch = "feature/supplied-pr-text"
	h.CommitChange(branch, "supplied.txt", "supplied\n", "add supplied fixture")
	wt := h.AddWorktree(branch)

	// Adversarial: invalid values are usage errors (exit 2) and start nothing.
	bidiBody := filepath.Join(scratch, "bidi.md")
	if err := os.WriteFile(bidiBody, []byte("Hello ‮evil\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	markerBody := filepath.Join(scratch, "marker.md")
	if err := os.WriteFile(markerBody, []byte("Hi\n<!-- no-mistakes-pipeline-attestation:v1 x -->\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	blankBody := filepath.Join(scratch, "blank.md")
	if err := os.WriteFile(blankBody, []byte("  \n\t\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	okBody := filepath.Join(scratch, "ok.md")
	if err := os.WriteFile(okBody, []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var refusals strings.Builder
	for _, c := range []struct {
		name string
		args []string
	}{
		{"empty title", []string{"--pr-title", "   "}},
		{"title with newline", []string{"--pr-title", "line one\nline two"}},
		{"title with bidi override", []string{"--pr-title", "fix ‮evil"}},
		{"oversized title", []string{"--pr-title", strings.Repeat("a", 4097)}},
		{"body with bidi override", []string{"--pr-body-file", bidiBody}},
		{"body with attestation marker", []string{"--pr-body-file", markerBody}},
		{"whitespace-only body", []string{"--pr-body-file", blankBody}},
		{"skip pr with title", []string{"--pr-title", "ok", "--skip", "pr"}},
		{"stdin claimed twice", []string{"--intent", "-", "--pr-body-file", "-"}},
	} {
		args := append([]string{"axi", "run"}, c.args...)
		if !contains(c.args, "--intent") {
			args = append(args, "--intent", "supplied pr text")
		}
		out, err := h.RunInDir(wt, args...)
		refusals.WriteString("### " + c.name + "\n$ no-mistakes axi run " + strings.Join(c.args, " ") + "\nexit=" + itoa(exitCode(err)) + "\n" + out + "\n")
		if code := exitCode(err); code != 2 {
			t.Errorf("%s: exit = %d, want 2; out:\n%s", c.name, code, out)
		}
	}
	saveSuppliedEvidence(t, "01-invalid-refusals.txt", refusals.String())
	if runs := h.Runs(); len(runs) != 0 {
		t.Fatalf("invalid PR text started %d run(s)", len(runs))
	}

	// Happy path: create publishes the supplied title and narrative verbatim.
	const title = "feat(widgets): Add supplied fixture"
	narrative := "## Summary\n\nTeam-convention description.\n\n## Testing\n\nRan it by hand.\n\nPath: " + h.HomeDir + "/secret.txt\n"
	bodyPath := filepath.Join(scratch, "body.md")
	if err := os.WriteFile(bodyPath, []byte(narrative), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := h.RunInDir(wt, "axi", "run", "--intent", "supplied pr text", "--skip", "ci", "--pr-title", title, "--pr-body-file", bodyPath)
	if err != nil {
		t.Fatalf("axi run with PR text: %v\n%s", err, out)
	}
	first := waitPublished(t, h, branch, "", "supplied create")
	gotTitle, gotBody := readRunPRText(t, h.NMHome, first.ID)
	if gotTitle == nil || *gotTitle != title || gotBody == nil || *gotBody != narrative {
		t.Fatalf("persisted pr_title/pr_body = %v/%v", gotTitle, gotBody)
	}
	pr := livePR(t, statePath, branch)
	saveSuppliedEvidence(t, "02-create-pr.md", "TITLE: "+pr.Title+"\n\n"+pr.Body)
	if pr.Title != title {
		t.Errorf("PR title = %q, want verbatim %q", pr.Title, title)
	}
	wantNarrative := strings.Replace(narrative, h.HomeDir, "~", 1)
	if !strings.HasPrefix(pr.Body, strings.TrimRight(wantNarrative, "\n")) {
		t.Errorf("PR body does not lead with the supplied narrative (home path redacted); body:\n%s", pr.Body)
	}
	if strings.Contains(pr.Body, h.HomeDir) {
		t.Errorf("PR body leaks the home path; body:\n%s", pr.Body)
	}
	if i := strings.Index(pr.Body, "<!-- no-mistakes-pr-appendix:v1"); i < 0 || exactLineCount(pr.Body[:i], "## Testing") != 1 {
		t.Errorf("team's own ## Testing heading did not survive exactly once; body:\n%s", pr.Body)
	}
	if !strings.Contains(pr.Body, "no-mistakes") || len(pr.Body) <= len(wantNarrative) {
		t.Errorf("PR body is missing the appendix/attestation; body:\n%s", pr.Body)
	}
	for _, inv := range h.AgentInvocations() {
		if strings.Contains(inv.Prompt, "Team-convention description.") {
			t.Errorf("supplied description reached an agent prompt:\n%s", truncate(inv.Prompt, 500))
		}
	}
	createdBody := pr.Body
	narrativeEnd := strings.Index(createdBody, "Ran it by hand.")
	if narrativeEnd < 0 {
		t.Fatalf("narrative missing")
	}

	// Reattach with different values while a run is active is refused; skip.
	// Rerun after a daemon restart inherits the stored text; a --pr-title
	// passed to rerun replaces the inherited title in storage, but the PR
	// already exists so it is not applied.
	if out, err := h.Run("daemon", "restart"); err != nil {
		t.Fatalf("daemon restart: %v\n%s", err, out)
	}
	const newTitle = "feat(widgets): Renamed by rerun"
	if out, err := h.RunInDir(wt, "rerun", "--pr-title", newTitle); err != nil || !strings.Contains(out, "Rerun started") {
		t.Fatalf("rerun --pr-title: %v\n%s", err, out)
	}
	second := waitPublished(t, h, branch, first.ID, "rerun")
	gotTitle, gotBody = readRunPRText(t, h.NMHome, second.ID)
	if gotTitle == nil || *gotTitle != newTitle {
		t.Errorf("rerun pr_title = %v, want replaced %q", gotTitle, newTitle)
	}
	if gotBody == nil || *gotBody != narrative {
		t.Errorf("rerun pr_body = %v, want inherited narrative", gotBody)
	}
	pr = livePR(t, statePath, branch)
	saveSuppliedEvidence(t, "03-rerun-pr.md", "TITLE: "+pr.Title+"\n\n"+pr.Body)
	if pr.Title != title {
		t.Errorf("rerun changed existing PR title to %q; existing PR must keep %q", pr.Title, title)
	}
	if !strings.HasPrefix(pr.Body, createdBody[:narrativeEnd]) {
		t.Errorf("rerun did not keep the narrative byte for byte; body:\n%s", pr.Body)
	}

	// A plain gate push starts with neither value and still keeps the owned
	// narrative byte for byte.
	h.Checkout("main")
	h.RemoveWorktree(wt)
	h.CommitChange(branch, "supplied.txt", "supplied v2\n", "update supplied fixture")
	h.PushToGate(branch)
	third := waitPublished(t, h, branch, second.ID, "plain push")
	gotTitle, gotBody = readRunPRText(t, h.NMHome, third.ID)
	if gotTitle != nil || gotBody != nil {
		t.Errorf("plain push run carries supplied text %v/%v", gotTitle, gotBody)
	}
	pr = livePR(t, statePath, branch)
	saveSuppliedEvidence(t, "04-plain-push-pr.md", "TITLE: "+pr.Title+"\n\n"+pr.Body)
	if !strings.HasPrefix(pr.Body, createdBody[:narrativeEnd]) {
		t.Errorf("plain push did not keep the owned narrative; body:\n%s", pr.Body)
	}

	// Push-option transport: a raw gate push carries base64 title and body.
	const optBranch = "feature/supplied-push-option"
	h.CommitChange(optBranch, "opt.txt", "opt\n", "add push option fixture")
	optTitle := "fix: Push option title"
	optBody := "Body via push option.\n"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := h.runGit(ctx, h.WorkDir, "push",
		"-o", "no-mistakes.pr-title="+base64.StdEncoding.EncodeToString([]byte(optTitle)),
		"-o", "no-mistakes.pr-body="+base64.StdEncoding.EncodeToString([]byte(optBody)),
		"-o", "no-mistakes.skip=ci", "no-mistakes", optBranch); err != nil {
		t.Fatalf("push with PR text options: %v\n%s", err, out)
	}
	waitPublished(t, h, optBranch, "", "push option")
	pr = livePR(t, statePath, optBranch)
	saveSuppliedEvidence(t, "05-push-option-pr.md", "TITLE: "+pr.Title+"\n\n"+pr.Body)
	if pr.Title != optTitle || !strings.HasPrefix(pr.Body, "Body via push option.") {
		t.Errorf("push option PR = %q / body:\n%s", pr.Title, pr.Body)
	}

	// Unconfigured run: no flags, nothing stored, ordinary drafted body.
	const plainBranch = "feature/no-supplied-text"
	h.CommitChange(plainBranch, "plain.txt", "plain\n", "add plain fixture")
	pwt := h.AddWorktree(plainBranch)
	if out, err := h.RunInDir(pwt, "axi", "run", "--intent", "plain change", "--skip", "ci"); err != nil {
		t.Fatalf("axi run without PR text: %v\n%s", err, out)
	}
	plain := waitPublished(t, h, plainBranch, "", "unconfigured")
	if gt, gb := readRunPRText(t, h.NMHome, plain.ID); gt != nil || gb != nil {
		t.Errorf("unconfigured run stored PR text %v/%v", gt, gb)
	}
	pr = livePR(t, statePath, plainBranch)
	saveSuppliedEvidence(t, "06-unconfigured-pr.md", "TITLE: "+pr.Title+"\n\n"+pr.Body)
	if pr.Title == "" || strings.Contains(pr.Body, "Team-convention description.") {
		t.Errorf("unconfigured PR = %q / body:\n%s", pr.Title, pr.Body)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func saveSuppliedEvidence(t *testing.T, name, content string) {
	t.Helper()
	dir := os.Getenv("NM_SUPPLIED_EVIDENCE_DIR")
	if dir == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Logf("write evidence %s: %v", name, err)
	}
}
