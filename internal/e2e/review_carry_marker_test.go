//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A fix round's rereview that does not positively verify a selected finding
// keeps it, and the real gate output and PR body must say it was carried, from
// which round, and why; a finding the rereview restates stays unmarked.
func TestReviewCarriedFindingsAreMarkedOnTheGateAndPRBody(t *testing.T) {
	scenario := filepath.Join(t.TempDir(), "agent.yaml")
	body := `actions:
  - match: "Fix-round provenance:"
    structured:
      findings:
        - id: fresh-service
          severity: warning
          file: service.txt
          description: Service retry loop never backs off
          action: ask-user
        - id: restated-other
          severity: warning
          file: other.txt
          description: Other handler swallows errors
          action: ask-user
      reviewed_paths: [service.txt, other.txt]
      summary: "verification round"
      risk_level: low
      risk_rationale: "checked"
      risk_scope: source-or-external
  - match: "Investigate previous review findings"
    edits:
      - path: service.txt
        new: "corrected service\n"
    structured:
      findings: []
      summary: "fix applied"
  - match: "Review the code changes and return structured findings"
    structured:
      findings:
        - id: service-bug
          severity: warning
          file: service.txt
          description: Service leaks a file descriptor
          action: ask-user
        - id: cache-bug
          severity: warning
          file: cache.txt
          description: Cache entry never expires
          action: ask-user
        - id: other-bug
          severity: warning
          file: other.txt
          description: Other handler swallows errors
          action: ask-user
      summary: "three concerns"
      risk_level: medium
      risk_rationale: "concerns"
      risk_scope: source-or-external
  - match: "Draft a pull request title and summary for the full branch delta."
    structured:
      title: "fix: service"
      body: "## What Changed\n\n- Service fix.\n"
  - structured:
      findings: []
      summary: "clean"
      risk_level: low
      risk_rationale: "none"
      risk_scope: source-or-external
      tested: ["fixture"]
      testing_summary: "fixture"
      scenarios:
        - name: "fixture"
          result: pass
          live: true
          evidence: "fixture"
          reason: ""
      verdict: go
      artifacts: []
      title: "fix: service"
      body: "## What Changed\n\n- Service fix.\n"
`
	if err := os.WriteFile(scenario, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
	parentURL := "https://github.com/example/no-mistakes.git"
	forkURL := "https://github.com/example-fork/no-mistakes.git"
	forkDir := filepath.Join(filepath.Dir(h.UpstreamDir), "fork.git")
	if err := os.MkdirAll(forkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := h.runGit(t.Context(), forkDir, "init", "--bare", "--initial-branch=main"); err != nil {
		t.Fatalf("init fork: %v\n%s", err, out)
	}
	if out, err := h.runGit(t.Context(), h.WorkDir, "push", forkDir, "main"); err != nil {
		t.Fatalf("seed fork: %v\n%s", err, out)
	}
	configureGitURLRewrite(t, h, parentURL, h.UpstreamDir)
	configureGitURLRewrite(t, h, forkURL, forkDir)
	if out, err := h.runGit(t.Context(), h.WorkDir, "remote", "set-url", "origin", parentURL); err != nil {
		t.Fatalf("set origin: %v\n%s", err, out)
	}
	ghLog := filepath.Join(filepath.Dir(h.AgentLog), "gh-carry.log")
	t.Setenv("FAKEAGENT_GH_MODE", "fork-pr")
	t.Setenv("FAKEAGENT_GH_LOG", ghLog)
	t.Setenv("FAKEAGENT_GH_PARENT", "example/no-mistakes")
	if out, err := h.Run("init", "--fork-url", forkURL); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	branch := "feature/carry-marker"
	h.CommitChange(branch, "service.txt", "broken service\n", "service")
	h.CommitChange(branch, "cache.txt", "changed cache\n", "cache")
	h.CommitChange(branch, "notes.txt", "changed notes\n", "notes")
	h.CommitChange(branch, "other.txt", "changed other\n", "other")
	initial, err := h.Run("axi", "run", "--intent", "Fix the service")
	if err != nil || !strings.Contains(initial, "cache-bug") {
		t.Fatalf("initial review gate: %v\n%s", err, initial)
	}
	if strings.Contains(initial, "carried") {
		t.Fatalf("round 1 gate marked a finding it reported itself as carried:\n%s", initial)
	}
	t.Logf("ROUND 1 GATE:\n%s", initial)

	response, err := h.Run("axi", "respond", "--action", "fix", "--findings", "service-bug,cache-bug,other-bug",
		"--add-finding", `{"severity":"warning","file":"notes.txt","description":"Notes typo the operator spotted"}`)
	if err != nil {
		t.Fatalf("select fix: %v\n%s", err, response)
	}
	t.Logf("ROUND 2 GATE (respond output):\n%s", response)
	waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusFixReview, 90*time.Second)
	gate, err := h.Run("axi", "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, gate)
	}
	t.Logf("ROUND 2 GATE (axi status):\n%s", gate)

	rowFor := func(out, needle string) string {
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, needle) {
				return line
			}
		}
		t.Fatalf("no row containing %q in:\n%s", needle, out)
		return ""
	}
	for needle, want := range map[string]string{
		"Cache entry never expires":      "carried from round 1, not cleared: file not in reviewed_paths",
		"Service leaks a file descriptor": "carried from round 1, not cleared: new finding in the same file",
		"Notes typo the operator spotted": "added by the operator at round 1, not cleared: file not in reviewed_paths",
	} {
		if row := rowFor(gate, needle); !strings.Contains(row, want) {
			t.Errorf("gate row for %q lacks %q:\n%s", needle, want, row)
		}
	}
	for _, needle := range []string{"Other handler swallows errors", "Service retry loop never backs off"} {
		if row := rowFor(gate, needle); strings.Contains(row, "carried from") || strings.Contains(row, "added by the operator") {
			t.Errorf("a finding this round reported is marked carried: %s", row)
		}
	}

	if out, err := h.Run("axi", "respond", "--action", "approve"); err != nil {
		t.Fatalf("approve: %v\n%s", err, out)
	}
	run := h.WaitForRun(branch, 120*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, error=%v", run.Status, run.Error)
	}
	store, err := db.OpenReadOnly(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	steps, err := store.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	for _, st := range steps {
		if st.StepName != types.StepReview {
			continue
		}
		rounds, err := store.GetRoundsByStep(st.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rounds {
			if r.Round == 2 && r.FindingsJSON != nil {
				stored = *r.FindingsJSON
			}
		}
	}
	t.Logf("STORED ROUND 2 findings_json:\n%s", stored)
	parsed, err := types.ParseFindingsJSON(stored)
	if err != nil {
		t.Fatalf("parse stored round 2: %v\n%s", err, stored)
	}
	wantStored := map[string]string{
		"cache-bug":     "file not in reviewed_paths",
		"service-bug":   "new finding in the same file",
		"user-1":        "file not in reviewed_paths",
		"other-bug":     "",
		"fresh-service": "",
	}
	for _, f := range parsed.Items {
		want, ok := wantStored[f.ID]
		if !ok {
			continue
		}
		delete(wantStored, f.ID)
		wantRound := 0
		if want != "" {
			wantRound = 1
		}
		if f.CarriedReason != want || f.CarriedFromRound != wantRound {
			t.Errorf("stored %s carried=(%d,%q), want (%d,%q)", f.ID, f.CarriedFromRound, f.CarriedReason, wantRound, want)
		}
	}
	if len(wantStored) != 0 {
		t.Errorf("stored round 2 is missing %v", wantStored)
	}
	prBody := createdPRBody(t, readGHStubInvocations(t, ghLog))
	t.Logf("PR BODY:\n%s", prBody)
	for _, want := range []string{
		"(3 carried from an earlier round)",
		"Cache entry never expires _(carried from round 1, not cleared: file not in reviewed_paths)_",
		"Service leaks a file descriptor _(carried from round 1, not cleared: new finding in the same file)_",
		"Notes typo the operator spotted _(added by the operator at round 1, not cleared: file not in reviewed_paths)_",
	} {
		if !strings.Contains(prBody, want) {
			t.Errorf("PR body lacks %q", want)
		}
	}
	for _, line := range strings.Split(prBody, "\n") {
		if (strings.Contains(line, "Other handler swallows errors") || strings.Contains(line, "Service retry loop")) && strings.Contains(line, "carried") {
			t.Errorf("PR body marks a reported finding as carried: %s", line)
		}
	}
}
