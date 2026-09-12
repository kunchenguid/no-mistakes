//go:build e2e

package e2e

import (
	"path/filepath"
	"testing"
)

const dlock31CIFixAction = `  - match: "The following CI checks have failed"
    edits:
      - path: repair.txt
        new: "repaired\n"
    structured:
      summary: "repair the disposable fixture"
      code_change_needed: true
`

// These inner agent responses drive the fixture; the tests assert the actual
// executable, IPC, SQLite, and Git behavior around those fixture responses.
const dlock31CleanAction = `  - structured:
      findings: []
      summary: "fixture clean response"
      risk_level: low
      risk_rationale: "fixture"
      risk_scope: source-or-external
      tested: ["simulated"]
      testing_summary: "simulated"
      artifacts: []
      scenarios:
        - name: "simulated fixture"
          result: pass
          live: true
          evidence: "simulated agent response, not live product proof"
          reason: ""
      verdict: go
      title: "test: fixture"
      body: "disposable fixture"
`

func newDLOCK31Harness(t *testing.T, actions string) (*Harness, string) {
	t.Helper()
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	provider := t.TempDir()
	t.Setenv("FAKEAGENT_GH_MODE", "dlock31")
	t.Setenv("DLOCK31_PROVIDER", provider)
	t.Setenv("DLOCK31_UPSTREAM", h.UpstreamDir)
	t.Setenv("FAKEAGENT_SCENARIO", filepath.Join(provider, "scenario.yaml"))
	setDLOCK31Actions(t, provider, actions)
	const remote = "https://github.com/dlock31/fixture.git"
	configureGitURLRewrite(t, h, remote, h.UpstreamDir)
	if out, err := h.runGit(t.Context(), h.WorkDir, "remote", "set-url", "origin", remote); err != nil {
		t.Fatalf("remote: %v %s", err, out)
	}
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v %s", err, out)
	}
	return h, provider
}

func setDLOCK31Actions(t *testing.T, provider, actions string) {
	t.Helper()
	writeDLOCK31(t, filepath.Join(provider, "scenario.yaml"), "actions:\n"+actions+dlock31CleanAction)
}
