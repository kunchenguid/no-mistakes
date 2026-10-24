//go:build e2e

package e2e

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// archiveSummaryMarker is a sentence repeated in the Test step's recorded
// testing summary until the generated report alone exceeds the GitHub PR
// body budget. Multi-byte runes keep the byte-vs-rune accounting honest.
const archiveSummaryMarker = "Recorded live evidence for the archive journey: Zażółć gęślą jaźń 😀."

// oversizedEvidenceScenario makes the Test step record a report far larger
// than a PR body may carry: a long testing summary and a long scenario list.
func oversizedEvidenceScenario(t *testing.T) string {
	t.Helper()
	scenarios := make([]map[string]any, 0, 200)
	for i := 0; i < 200; i++ {
		scenarios = append(scenarios, map[string]any{
			"name":     fmt.Sprintf("fakeagent: archive journey scenario %03d drives the product end to end", i),
			"result":   "pass",
			"live":     true,
			"evidence": fmt.Sprintf("fakeagent: transcript for scenario %03d", i),
			"reason":   "",
		})
	}
	testOutput := map[string]any{
		"findings":        []any{},
		"summary":         "all tests passed",
		"tested":          []string{"fakeagent: simulated test run"},
		"testing_summary": strings.Repeat(archiveSummaryMarker+" ", 1500),
		"scenarios":       scenarios,
		"verdict":         "go",
		"artifacts":       []any{},
	}
	fallback := map[string]any{
		"findings":        []any{},
		"summary":         "no issues found",
		"risk_level":      "low",
		"risk_rationale":  "no risks detected in the diff",
		"risk_scope":      "source-or-external",
		"tested":          []string{"fakeagent: simulated test run"},
		"testing_summary": "simulated tests passed",
		"scenarios":       scenarios[:1],
		"verdict":         "go",
		"artifacts":       []any{},
		"title":           "feat: fakeagent change",
		"body":            "## Overview\n\nfakeagent canned PR body",
	}
	scenario := map[string]any{"actions": []any{
		map[string]any{"match": "You are validating a code change by driving the product itself.", "text": "recorded a large report", "structured": testOutput},
		map[string]any{"text": "no issues found", "structured": fallback},
	}}
	data, err := json.Marshal(scenario) // JSON is valid YAML.
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "oversized-evidence.yaml")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func saveArchiveEvidence(t *testing.T, name, content string) {
	t.Helper()
	dir := os.Getenv("NM_ARCHIVE_EVIDENCE_DIR")
	if dir == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Logf("write evidence %s: %v", name, err)
	}
}

func setupArchiveJourney(t *testing.T, storeInRepo bool) (*Harness, string) {
	t.Helper()
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: oversizedEvidenceScenario(t)})
	statePath := setupStatefulGitHub(t, h)
	h.CommitChange("main", ".github/pr_template.md", "## Overview\n\nDescribe the change.\n", "add PR template")
	pushMainRepoConfig(t, h, fmt.Sprintf("no_ci: true\npr:\n  template: .github/pr_template.md\ntest:\n  evidence:\n    store_in_repo: %t\n    attach_media: false\n", storeInRepo))
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	return h, statePath
}

var archiveLinkPattern = regexp.MustCompile(`\[Complete generated report\]\(https://github\.com/example/closes/blob/([0-9a-f]{40,64})/([^ )]+)\) \(snapshot for candidate ` + "`([0-9a-f]+)`" + `\)`)

// verifyArchive resolves the commit-pinned link against the upstream the
// pipeline really pushed to and checks the digest and candidate binding.
func verifyArchive(t *testing.T, h *Harness, body, head string) (commitPath string, data []byte) {
	t.Helper()
	m := archiveLinkPattern.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("live PR body has no commit-pinned archive link:\n%s", body)
	}
	if m[3] != head {
		t.Fatalf("archive names candidate %s, want run head %s", m[3], head)
	}
	out, err := h.runGit(t.Context(), h.UpstreamDir, "show", m[1]+":"+m[2])
	if err != nil {
		t.Fatalf("archive link does not resolve in upstream: %v\n%s", err, out)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(out))
	if !strings.Contains(body, "SHA-256 of the exact archived file: `"+digest+"`") {
		t.Fatalf("live digest does not match archived bytes (%s)", digest)
	}
	if !strings.HasSuffix(m[2], "pr-report-"+digest+".md") {
		t.Fatalf("archive file %s is not named by its digest %s", m[2], digest)
	}
	if n := strings.Count(string(out), archiveSummaryMarker); n != 1500 {
		t.Fatalf("archive carries %d of 1500 recorded summary sentences", n)
	}
	if !strings.Contains(string(out), "archive journey scenario") {
		t.Fatal("archive lost the recorded scenario list")
	}
	// The live body carries the archived report's attestation verbatim.
	marker := regexp.MustCompile(`<!-- no-mistakes-pipeline-attestation:v1 [^\n]*-->`)
	if live, archived := marker.FindString(body), marker.FindString(string(out)); live == "" || live != archived {
		t.Fatalf("live attestation differs from the archived report's:\nlive:     %s\narchived: %s", live, archived)
	}
	return m[1] + ":" + m[2], out
}

func TestPRReportArchiveJourney(t *testing.T) {
	h, statePath := setupArchiveJourney(t, true)
	const branch = "feature/archive-journey"
	h.CommitChange(branch, "a.txt", "a\n", "add archive fixture")
	wt := h.AddWorktree(branch)
	if out, err := h.RunInDir(wt, "axi", "run", "--intent", "archive oversized evidence", "--skip", "ci", "--closes", "95"); err != nil {
		t.Fatalf("axi run: %v\n%s", err, out)
	}
	first := waitPublished(t, h, branch, "", "archive create")
	head := h.WorktreeRefSHA("refs/heads/" + branch)
	body := livePRBody(t, statePath, branch)
	saveArchiveEvidence(t, "01-created-pr-body.md", body)
	if len(body) > 65536 {
		t.Fatalf("live PR body is %d bytes", len(body))
	}
	for _, want := range []string{"## Overview", "<!-- no-mistakes-pipeline-attestation:", "\"head_sha\":\"" + head + "\""} {
		if !strings.Contains(body, want) {
			t.Fatalf("live body lacks %q:\n%s", want, body)
		}
	}
	assertLinesOnce(t, "archive create", body, "Closes #95")
	object, data := verifyArchive(t, h, body, head)
	saveArchiveEvidence(t, "02-archived-report.md", string(data))
	tree, _ := h.runGit(t.Context(), h.UpstreamDir, "ls-tree", "-r", "--name-only", "no-mistakes/evidence")
	mainLog, _ := h.runGit(t.Context(), h.UpstreamDir, "log", "--oneline", "-3", "main")
	saveArchiveEvidence(t, "03-upstream-state.txt", fmt.Sprintf("run %s head %s\nlink object %s (%d bytes, sha256 %x)\n\nno-mistakes/evidence tree:\n%s\nmain (unchanged code branch):\n%s", first.ID, head, object, len(data), sha256.Sum256(data), tree, mainLog))

	// An author edits the live PR, then a rerun updates it: the author edit
	// survives and the earlier commit-pinned archive keeps its exact bytes.
	state := readStatefulGH(t, statePath)
	state.PRs[branch].Body = strings.Replace(state.PRs[branch].Body, "## Overview", "## Overview\n\nAuthor edit: Zażółć 😀 keep me.", 1)
	writeStatefulGH(t, statePath, state)
	if out, err := h.RunInDir(wt, "rerun"); err != nil || !strings.Contains(out, "Rerun started") {
		t.Fatalf("rerun: %v\n%s", err, out)
	}
	waitPublished(t, h, branch, first.ID, "archive update")
	updated := livePRBody(t, statePath, branch)
	saveArchiveEvidence(t, "04-updated-pr-body.md", updated)
	if !strings.Contains(updated, "Author edit: Zażółć 😀 keep me.") {
		t.Fatalf("author edit lost on update:\n%s", updated)
	}
	newObject, _ := verifyArchive(t, h, updated, head)
	old, err := h.runGit(t.Context(), h.UpstreamDir, "show", object)
	if err != nil || string(old) != string(data) {
		t.Fatalf("earlier archive link changed or vanished: %v", err)
	}
	saveArchiveEvidence(t, "05-immutable-links.txt", fmt.Sprintf("first link object:  %s (still %d identical bytes)\nupdate link object: %s\n", object, len(old), newObject))
}

func archivePRStepFailure(t *testing.T, h *Harness, branch string) string {
	t.Helper()
	run := h.WaitForRun(branch, 3*time.Minute)
	pr, _ := findStep(run.Steps, types.StepPR)
	if pr.Status == types.StepStatusCompleted || run.Status == types.RunCompleted {
		t.Fatalf("oversized report published: run=%s pr=%s", run.Status, pr.Status)
	}
	return fmt.Sprintf("run status: %s\npr step status: %s\npr step error: %s\n", run.Status, pr.Status, deref(pr.Error))
}

func TestPRReportArchiveRefusedWithoutOptIn(t *testing.T) {
	h, statePath := setupArchiveJourney(t, false)
	const branch = "feature/archive-no-opt-in"
	h.CommitChange(branch, "a.txt", "a\n", "add archive fixture")
	wt := h.AddWorktree(branch)
	h.RunInDir(wt, "axi", "run", "--intent", "archive without opt-in", "--skip", "ci", "--closes", "95")
	summary := archivePRStepFailure(t, h, branch)
	if _, err := h.runGit(t.Context(), h.UpstreamDir, "rev-parse", "--verify", "refs/heads/no-mistakes/evidence"); err == nil {
		t.Fatal("evidence branch published without the opt-in")
	}
	state, _ := os.ReadFile(statePath)
	saveArchiveEvidence(t, "06-no-opt-in.txt", summary+"\nfake forge state:\n"+string(state))
	if strings.Contains(string(state), "Complete generated report") {
		t.Fatal("PR carries an archive link without the opt-in")
	}
	if !strings.Contains(summary, "exceeds provider budget") {
		t.Fatalf("refusal does not name the budget:\n%s", summary)
	}
}

func TestPRReportArchivePushRefusedFailsClosed(t *testing.T) {
	h, statePath := setupArchiveJourney(t, true)
	hook := "#!/bin/sh\nwhile read old new ref; do\n  case \"$ref\" in refs/heads/no-mistakes/evidence) echo 'evidence push refused' >&2; exit 1;; esac\ndone\nexit 0\n"
	if err := os.WriteFile(filepath.Join(h.UpstreamDir, "hooks", "pre-receive"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	const branch = "feature/archive-refused"
	h.CommitChange(branch, "a.txt", "a\n", "add archive fixture")
	wt := h.AddWorktree(branch)
	h.RunInDir(wt, "axi", "run", "--intent", "archive refused", "--skip", "ci", "--closes", "95")
	summary := archivePRStepFailure(t, h, branch)
	state, _ := os.ReadFile(statePath)
	saveArchiveEvidence(t, "07-push-refused.txt", summary+"\nfake forge state:\n"+string(state))
	if strings.Contains(string(state), "Complete generated report") {
		t.Fatal("PR carries a dead archive link after a refused push")
	}
	if !strings.Contains(summary, "archive complete PR report") {
		t.Fatalf("failure does not name the archive push:\n%s", summary)
	}
}

func TestPRReportArchiveRefusedOnUnsupportedHost(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: oversizedEvidenceScenario(t)})
	const (
		giteaHost = "gitea.example.com"
		remoteURL = "https://" + giteaHost + "/owner/repo.git"
		branch    = "feature/archive-gitea"
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
	h.CommitChange("main", ".github/pr_template.md", "## Overview\n\nDescribe the change.\n", "add PR template")
	pushMainRepoConfig(t, h, "no_ci: true\npr:\n  template: .github/pr_template.md\ntest:\n  evidence:\n    store_in_repo: true\n    attach_media: false\n")
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	h.CommitChange(branch, "a.txt", "a\n", "add archive fixture")
	wt := h.AddWorktree(branch)
	h.RunInDir(wt, "axi", "run", "--intent", "archive on gitea", "--skip", "ci")
	summary := archivePRStepFailure(t, h, branch)
	teaCalls, _ := os.ReadFile(teaLog)
	saveArchiveEvidence(t, "08-unsupported-host.txt", summary+"\ntea invocations:\n"+string(teaCalls))
	if _, err := h.runGit(t.Context(), h.UpstreamDir, "rev-parse", "--verify", "refs/heads/no-mistakes/evidence"); err == nil {
		t.Fatal("unsupported host published an archive")
	}
	if !strings.Contains(summary, "lossless report archiving requires GitHub evidence links") {
		t.Fatalf("refusal does not name the unsupported host:\n%s", summary)
	}
}
