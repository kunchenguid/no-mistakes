//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type statefulGHPR struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Base   string `json:"base"`
	Head   string `json:"head"`
	State  string `json:"state"`
}

type statefulGHState struct {
	PRs map[string]*statefulGHPR `json:"prs"`
}

// setupStatefulGitHub points origin at a github.com URL (rewritten to the
// local upstream) and shadows gh with the fakeagent stub that remembers PR
// bodies, so the pipeline's create/update/verify round trip reads back exactly
// what it last published.
func setupStatefulGitHub(t *testing.T, h *Harness) string {
	t.Helper()
	originURL := "https://github.com/example/closes.git"
	configureGitURLRewrite(t, h, originURL, h.UpstreamDir)
	if out, err := h.runGit(t.Context(), h.WorkDir, "remote", "set-url", "origin", originURL); err != nil {
		t.Fatalf("set GitHub origin: %v\n%s", err, out)
	}
	statePath := filepath.Join(filepath.Dir(h.AgentLog), "gh-state.json")
	t.Setenv("FAKEAGENT_GH_MODE", "stateful-pr")
	t.Setenv("FAKEAGENT_GH_STATE", statePath)
	t.Setenv("FAKEAGENT_GH_LOG", filepath.Join(filepath.Dir(h.AgentLog), "gh-stateful.log"))
	t.Setenv("FAKEAGENT_GH_PARENT", "example/closes")
	return statePath
}

func readStatefulGH(t *testing.T, path string) statefulGHState {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read gh state: %v", err)
	}
	var state statefulGHState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("parse gh state: %v", err)
	}
	return state
}

func writeStatefulGH(t *testing.T, path string, state statefulGHState) {
	t.Helper()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func livePRBody(t *testing.T, statePath, branch string) string {
	t.Helper()
	pr := readStatefulGH(t, statePath).PRs[branch]
	if pr == nil {
		t.Fatalf("no PR recorded for %s", branch)
	}
	return pr.Body
}

func readRunClosingRefs(t *testing.T, nmHome, runID string) ([]string, *int64) {
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
	return run.ClosingIssueRefs, run.ClosingIssueRefsLockedAt
}

// exactLineCount counts lines equal to want after trimming whitespace.
func exactLineCount(body, want string) int {
	n := 0
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) == want {
			n++
		}
	}
	return n
}

func assertLinesOnce(t *testing.T, label, body string, lines ...string) {
	t.Helper()
	for _, want := range lines {
		if got := exactLineCount(body, want); got != 1 {
			t.Errorf("%s: line %q appears %d times, want exactly once; body:\n%s", label, want, got, body)
		}
	}
}

func saveEvidence(t *testing.T, name, content string) {
	t.Helper()
	dir := os.Getenv("NM_CLOSES_EVIDENCE_DIR")
	if dir == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Logf("write evidence %s: %v", name, err)
	}
}

// waitPublished waits for the newest run on branch other than prevID to
// complete its PR step. A run that does not skip CI keeps monitoring the open
// PR until it merges, so once the body is published it is cancelled: the
// next scenario needs the branch idle, not a merged PR.
// A CI-skipped run must finish naturally; PR completion can precede its
// terminal status, and cancelling that snapshot races the finished run.
func waitPublished(t *testing.T, h *Harness, branch, prevID, label string) *ipc.RunInfo {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	var run *ipc.RunInfo
	for time.Now().Before(deadline) {
		run = nil
		for _, r := range h.Runs() {
			if r.Branch == branch && r.ID != prevID {
				r := r
				run = &r
				break
			}
		}
		if run != nil {
			pr, _ := findStep(run.Steps, types.StepPR)
			ci, _ := findStep(run.Steps, types.StepCI)
			if run.Status.Terminal() || pr.Status == types.StepStatusCompleted && ci.Status == types.StepStatusRunning {
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if run == nil {
		t.Fatalf("%s: no new run for %s", label, branch)
	}
	if pr, _ := findStep(run.Steps, types.StepPR); pr.Status != types.StepStatusCompleted {
		for _, s := range run.Steps {
			t.Logf("%s: step %s status=%s error=%v", label, s.StepName, s.Status, deref(s.Error))
		}
		t.Fatalf("%s: run %s status=%s error=%v: PR step did not complete", label, run.ID, run.Status, deref(run.Error))
	}
	if !run.Status.Terminal() {
		if ci, _ := findStep(run.Steps, types.StepCI); ci.Status != types.StepStatusRunning {
			t.Fatalf("%s: run %s status=%s: CI did not start monitoring or the run did not finish", label, run.ID, run.Status)
		}
		h.CancelRun(run.ID)
		h.WaitForRun(branch, time.Minute)
	} else if run.Status != types.RunCompleted {
		t.Fatalf("%s: run %s status=%s error=%v", label, run.ID, run.Status, deref(run.Error))
	}
	return run
}

func TestClosingIssueRefsGitHubJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	statePath := setupStatefulGitHub(t, h)
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	const branch = "feature/closes-journey"
	h.CommitChange(branch, "closes.txt", "closes\n", "add closes fixture")
	wt := h.AddWorktree(branch)

	// Scenario: invalid references are refused before any run starts.
	for _, bad := range []string{"#95", "abc", "0", "owner#9", "owner/repo#"} {
		out, err := h.RunInDir(wt, "axi", "run", "--intent", "close issues", "--skip", "ci", "--closes", bad)
		if err == nil || !strings.Contains(out, "invalid --closes") {
			t.Fatalf("--closes %q should be rejected, err=%v out:\n%s", bad, err, out)
		}
		saveEvidence(t, "01-invalid-closes.txt", "$ no-mistakes axi run --closes "+bad+"\n"+out)
	}
	if runs := h.Runs(); len(runs) != 0 {
		t.Fatalf("invalid --closes started %d run(s)", len(runs))
	}

	// Scenario: axi run --closes dedupes, orders, persists, and renders once.
	out, err := h.RunInDir(wt, "axi", "run", "--intent", "close the issues", "--skip", "ci",
		"--closes", "95", "--closes", "Owner/Repo#7", "--closes", "95", "--closes", "owner/repo#7", "--closes", "12")
	if err != nil {
		t.Fatalf("axi run --closes: %v\n%s", err, out)
	}
	first := waitPublished(t, h, branch, "", "first run")
	refs, locked := readRunClosingRefs(t, h.NMHome, first.ID)
	if want := []string{"12", "95", "owner/repo#7"}; !reflect.DeepEqual(refs, want) {
		t.Fatalf("persisted refs = %v, want %v", refs, want)
	}
	if locked == nil {
		t.Errorf("closing refs were never claimed by the PR step")
	}
	body := livePRBody(t, statePath, branch)
	saveEvidence(t, "02-first-run-pr-body.md", body)
	assertLinesOnce(t, "first run", body, "## Issues", "Closes #12", "Closes #95", "Closes owner/repo#7")
	if i12, i95, i7 := strings.Index(body, "Closes #12"), strings.Index(body, "Closes #95"), strings.Index(body, "Closes owner/repo#7"); !(i12 < i95 && i95 < i7) {
		t.Errorf("closing lines not in deterministic order; body:\n%s", body)
	}

	// Scenario: a rerun (after a daemon restart) inherits refs and adds one.
	if out, err := h.Run("daemon", "restart"); err != nil {
		t.Fatalf("daemon restart: %v\n%s", err, out)
	}
	if out, err := h.RunInDir(wt, "rerun", "--closes", "200"); err != nil || !strings.Contains(out, "Rerun started") {
		t.Fatalf("rerun --closes: %v\n%s", err, out)
	}
	second := waitPublished(t, h, branch, first.ID, "rerun")
	if second.ID == first.ID {
		t.Fatal("rerun did not start a new run")
	}
	refs, _ = readRunClosingRefs(t, h.NMHome, second.ID)
	if want := []string{"12", "95", "200", "owner/repo#7"}; !reflect.DeepEqual(refs, want) {
		t.Fatalf("rerun refs = %v, want inherited+added %v", refs, want)
	}
	body = livePRBody(t, statePath, branch)
	saveEvidence(t, "03-rerun-pr-body.md", body)
	assertLinesOnce(t, "rerun", body, "Closes #12", "Closes #95", "Closes #200", "Closes owner/repo#7")

	// Scenario: a plain gate push (a new run with no --closes) regenerates the
	// body without closing references.
	h.Checkout("main")
	h.RemoveWorktree(wt)
	h.CommitChange(branch, "closes.txt", "closes v2\n", "update closes fixture")
	h.PushToGate(branch)
	third := waitPublished(t, h, branch, second.ID, "plain push")
	if third.ID == second.ID {
		t.Fatal("plain push did not start a new run")
	}
	if refs, _ := readRunClosingRefs(t, h.NMHome, third.ID); len(refs) != 0 {
		t.Errorf("plain push run unexpectedly carries requested refs %v", refs)
	}
	body = livePRBody(t, statePath, branch)
	saveEvidence(t, "04-plain-push-pr-body.md", body)
	for _, keyword := range []string{"## Issues", "Closes #"} {
		if strings.Contains(body, keyword) {
			t.Errorf("plain push body contains %q; body:\n%s", keyword, body)
		}
	}

	// Scenario: a gate push option carries a reference on the non-AXI path.
	const optBranch = "feature/closes-push-option"
	h.CommitChange(optBranch, "opt.txt", "opt\n", "add push option fixture")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := h.runGit(ctx, h.WorkDir, "push", "-o", "no-mistakes.closes=41", "-o", "no-mistakes.closes=acme/widgets#5", "-o", "no-mistakes.skip=ci", "no-mistakes", optBranch); err != nil {
		t.Fatalf("push with closes option: %v\n%s", err, out)
	}
	optRun := waitPublished(t, h, optBranch, "", "push option")
	refs, _ = readRunClosingRefs(t, h.NMHome, optRun.ID)
	if want := []string{"41", "acme/widgets#5"}; !reflect.DeepEqual(refs, want) {
		t.Fatalf("push option refs = %v, want %v", refs, want)
	}
	body = livePRBody(t, statePath, optBranch)
	saveEvidence(t, "05-push-option-pr-body.md", body)
	assertLinesOnce(t, "push option", body, "## Issues", "Closes #41", "Closes acme/widgets#5")

	// Scenario: an invalid push option is refused at the gate.
	const badOptBranch = "feature/closes-bad-option"
	h.CommitChange(badOptBranch, "bad.txt", "bad\n", "add bad option fixture")
	out2, err := h.runGit(ctx, h.WorkDir, "push", "-o", "no-mistakes.closes=#9", "no-mistakes", badOptBranch)
	saveEvidence(t, "06-invalid-push-option.txt", string(out2))
	if run := h.ActiveRun(badOptBranch); run != nil {
		t.Errorf("invalid closes push option started run %s (push err=%v)\n%s", run.ID, err, out2)
	}
	for _, r := range h.Runs() {
		if r.Branch == badOptBranch {
			t.Errorf("invalid closes push option created run %s (status %s); push output:\n%s", r.ID, r.Status, out2)
		}
	}

	// Scenario: without --closes no closing keyword is added or inferred,
	// even when the branch name and commit message mention an issue.
	const plainBranch = "fix/issue-321"
	h.CommitChange(plainBranch, "plain.txt", "plain\n", "Fix #321: plain change")
	pwt := h.AddWorktree(plainBranch)
	if out, err := h.RunInDir(pwt, "axi", "run", "--intent", "fixes issue #321", "--skip", "ci"); err != nil {
		t.Fatalf("axi run without closes: %v\n%s", err, out)
	}
	waitPublished(t, h, plainBranch, "", "no closes")
	body = livePRBody(t, statePath, plainBranch)
	saveEvidence(t, "07-no-closes-pr-body.md", body)
	for _, keyword := range []string{"## Issues", "Closes #", "Fixes #", "Resolves #"} {
		if strings.Contains(body, keyword) {
			t.Errorf("PR without --closes contains %q; body:\n%s", keyword, body)
		}
	}
}

// TestClosingIssueRefsReattachAfterComposeIsRefused leaves CI running (no
// checks yet) after the PR body is composed, then reattaches with --closes.
func TestClosingIssueRefsReattachAfterComposeIsRefused(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	statePath := setupStatefulGitHub(t, h)
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/closes-reattach"
	h.CommitChange(branch, "r.txt", "r\n", "add reattach fixture")
	wt := h.AddWorktree(branch)
	out, err := h.RunInDir(wt, "axi", "run", "--intent", "reattach", "--closes", "95", "--wait", "20s")
	t.Logf("first axi run (err=%v):\n%s", err, out)

	deadline := time.Now().Add(2 * time.Minute)
	var run *ipc.RunInfo
	for time.Now().Before(deadline) {
		run = h.ActiveRun(branch)
		if run != nil {
			if pr, ok := findStep(run.Steps, types.StepPR); ok && pr.Status == types.StepStatusCompleted {
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if run == nil {
		t.Fatalf("no active run after PR step; runs=%+v", h.Runs())
	}
	if pr, _ := findStep(run.Steps, types.StepPR); pr.Status != types.StepStatusCompleted {
		t.Fatalf("PR step did not complete while the run stayed active: %+v", run.Steps)
	}
	assertLinesOnce(t, "reattach base", livePRBody(t, statePath, branch), "Closes #95")

	same, sameErr := h.RunInDir(wt, "axi", "run", "--intent", "reattach", "--closes", "95", "--wait", "5s")
	saveEvidence(t, "08-reattach-same-ref.txt", "$ no-mistakes axi run --closes 95  # reattach, already recorded\n"+same)
	if strings.Contains(same, "already composed") {
		t.Errorf("re-sending an already-recorded ref was refused: %v\n%s", sameErr, same)
	}

	added, addErr := h.RunInDir(wt, "axi", "run", "--intent", "reattach", "--closes", "77", "--wait", "5s")
	saveEvidence(t, "09-reattach-new-ref.txt", "$ no-mistakes axi run --closes 77  # reattach after PR body composed\n"+added)
	if addErr == nil || !strings.Contains(added, "already composed") || !strings.Contains(added, "NOT added") {
		t.Errorf("new ref after compose should be refused explicitly, err=%v\n%s", addErr, added)
	}
	refs, _ := readRunClosingRefs(t, h.NMHome, run.ID)
	if !reflect.DeepEqual(refs, []string{"95"}) {
		t.Errorf("refused reattach changed persisted refs: %v", refs)
	}
	if strings.Contains(livePRBody(t, statePath, branch), "#77") {
		t.Errorf("refused ref reached the PR body")
	}
	h.CancelRun(run.ID)
}

// TestClosingIssueRefsNonGitHubFailsAtPR runs --closes against a Gitea origin.
func TestClosingIssueRefsNonGitHubFailsAtPR(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	const (
		giteaHost = "gitea.example.com"
		remoteURL = "https://" + giteaHost + "/owner/repo.git"
		branch    = "feature/closes-gitea"
	)
	configureGitURLRewrite(t, h, remoteURL, h.UpstreamDir)
	if out, err := h.runGit(t.Context(), h.WorkDir, "remote", "set-url", "origin", remoteURL); err != nil {
		t.Fatalf("set origin: %v\n%s", err, out)
	}
	xdg := filepath.Join(h.HomeDir, ".config")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if err := os.MkdirAll(filepath.Join(xdg, "tea"), 0o755); err != nil {
		t.Fatal(err)
	}
	teaConfig := "logins:\n    - name: e2e\n      url: https://" + giteaHost + "\n      ssh_host: " + giteaHost + "\n      user: e2e-tea-user\n      token: xxx\n"
	if err := os.WriteFile(filepath.Join(xdg, "tea", "config.yml"), []byte(teaConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	teaLog := filepath.Join(filepath.Dir(h.AgentLog), "tea.log")
	t.Setenv("FAKEAGENT_TEA_LOG", teaLog)
	t.Setenv("FAKEAGENT_TEA_HOST", giteaHost)
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	h.CommitChange(branch, "g.txt", "g\n", "add gitea fixture")
	wt := h.AddWorktree(branch)
	out, _ := h.RunInDir(wt, "axi", "run", "--intent", "gitea closes", "--skip", "ci", "--closes", "5")
	run := h.WaitForRun(branch, 3*time.Minute)
	pr, _ := findStep(run.Steps, types.StepPR)
	evidence := "$ no-mistakes axi run --closes 5   # origin is Gitea\n" + out + "\nrun status: " + string(run.Status) + "\npr step status: " + string(pr.Status) + "\npr step error: " + deref(pr.Error) + "\nrun error: " + deref(run.Error) + "\n"
	saveEvidence(t, "10-gitea-closes.txt", evidence)
	if run.Status == types.RunCompleted || pr.Status == types.StepStatusCompleted {
		t.Fatalf("--closes on Gitea should fail the PR step; %s", evidence)
	}
	if data, err := os.ReadFile(teaLog); err == nil && strings.Contains(string(data), "\"create\"") {
		t.Errorf("a Gitea PR was created despite --closes; tea log:\n%s", data)
	}
	t.Log(evidence)
}

// TestClosingIssueRefsOwnedTemplateJourney drives the author-preserving
// pr.template path: the appendix renders the requested reference, and author
// text that already closes the issue is not duplicated by the appendix.
func TestClosingIssueRefsOwnedTemplateJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	statePath := setupStatefulGitHub(t, h)
	h.CommitChange("main", ".github/pr_template.md", "## Overview\n\nDescribe the change.\n", "add PR template")
	h.CommitChange("main", ".no-mistakes.yaml", "no_ci: true\npr:\n  template: .github/pr_template.md\n", "configure PR template")
	if out, err := h.runGit(t.Context(), h.WorkDir, "push", "origin", "main"); err != nil {
		t.Fatalf("push main: %v\n%s", err, out)
	}
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/closes-template"
	h.CommitChange(branch, "t.txt", "t\n", "add template fixture")
	wt := h.AddWorktree(branch)
	if out, err := h.RunInDir(wt, "axi", "run", "--intent", "template closes", "--skip", "ci", "--closes", "95"); err != nil {
		t.Fatalf("axi run: %v\n%s", err, out)
	}
	created := waitPublished(t, h, branch, "", "template create")
	body := livePRBody(t, statePath, branch)
	saveEvidence(t, "11-template-create-pr-body.md", body)
	if !strings.Contains(body, "no-mistakes") || !strings.Contains(body, "Describe") && !strings.Contains(body, "fakeagent") {
		t.Logf("template body:\n%s", body)
	}
	assertLinesOnce(t, "template create", body, "Closes #95")

	// Author text that already closes the issue: a rerun (which inherits
	// --closes 95) must not add a second closing reference for it in the
	// appendix, and the author's line stays verbatim.
	state := readStatefulGH(t, statePath)
	state.PRs[branch].Body = "Fixes #95\n\n" + state.PRs[branch].Body
	writeStatefulGH(t, statePath, state)
	if out, err := h.RunInDir(wt, "rerun"); err != nil || !strings.Contains(out, "Rerun started") {
		t.Fatalf("rerun: %v\n%s", err, out)
	}
	waitPublished(t, h, branch, created.ID, "template author closes")
	body = livePRBody(t, statePath, branch)
	saveEvidence(t, "13-template-author-fixes-pr-body.md", body)
	if got := exactLineCount(body, "Fixes #95"); got != 1 {
		t.Errorf("author line Fixes #95 appears %d times; body:\n%s", got, body)
	}
	if got := exactLineCount(body, "Closes #95"); got != 0 {
		t.Errorf("appendix repeats a reference the author text already closes (%d Closes #95 lines); body:\n%s", got, body)
	}
}
