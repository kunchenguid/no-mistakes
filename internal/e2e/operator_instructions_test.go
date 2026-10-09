//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	globalOperatorRule     = "Every exported Go identifier in this tree needs a doc comment."
	repositoryOperatorRule = "Sync wording is always sync from upstream, never merge."
	operatorDocumentPolicy = "Configuration keys are owned by docs/reference/config.md."
	documentStepMarker     = "Analyze what the change made stale"
)

// TestOperatorInstructionsJourney drives the real daemon with review and
// documentation guidance that lives only in the operator's global config: a
// global rule for every repository and a repository_overrides entry for this
// one. Both reach their gate agents in labeled sections ahead of the
// repository's own trusted rule, which still applies.
func TestOperatorInstructionsJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	pushMainRepoConfig(t, h, trustedRepoConfigWithPathInstructions)

	globalConfig := filepath.Join(h.NMHome, "config.yaml")
	data, err := os.ReadFile(globalConfig)
	if err != nil {
		t.Fatalf("read global config: %v", err)
	}
	review := "review:\n  path_instructions:\n    - path: 'internal/**'\n      instructions: " + globalOperatorRule + "\n"
	if err := os.WriteFile(globalConfig, append(data, []byte(review)...), 0o644); err != nil {
		t.Fatalf("write global config: %v", err)
	}
	setupMachineLocalCommands(t, h, `    review:
      path_instructions:
        - path: 'internal/scm/**'
          instructions: `+repositoryOperatorRule+`
    document:
      instructions: `+operatorDocumentPolicy+`
`)

	branch := "operator-instructions"
	h.CommitChange(branch, "internal/scm/github/github.go", "package github\n\n// changed\n", "touch scm")
	h.PushToGate(branch)

	run := h.WaitForRun(branch, 120*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
	}

	prompt := reviewPrompt(t, h)
	global := strings.Index(prompt, config.ReviewGlobalPathInstructionsHeading+"\n"+config.ReviewPathInstructionsPathLabel+"internal/**\n")
	repository := strings.Index(prompt, config.ReviewRepositoryPathInstructionsHeading+"\n"+config.ReviewPathInstructionsPathLabel+"internal/scm/**\n")
	trusted := strings.Index(prompt, config.ReviewPathInstructionsHeading+"\n"+config.ReviewPathInstructionsPathLabel+"internal/scm/**\n")
	if global < 0 || repository < 0 || trusted < 0 || !(global < repository && repository < trusted) {
		t.Fatalf("want global, per-repository, then trusted sections (indexes %d, %d, %d) in:\n%s", global, repository, trusted, prompt)
	}
	for _, rule := range []string{globalOperatorRule, repositoryOperatorRule, scmPathRule} {
		if !strings.Contains(prompt, rule) {
			t.Errorf("review prompt is missing rule %q", rule)
		}
	}

	var documentPrompt string
	for _, inv := range h.AgentInvocations() {
		if strings.Contains(inv.Prompt, documentStepMarker) {
			documentPrompt = inv.Prompt
			break
		}
	}
	if !strings.Contains(documentPrompt, "Machine-local documentation ownership policy for this repository") ||
		!strings.Contains(documentPrompt, operatorDocumentPolicy) {
		t.Fatalf("document prompt is missing the machine-local policy:\n%s", documentPrompt)
	}

	logs, err := h.Run("axi", "logs", "--run", run.ID, "--step", "review", "--full")
	if err != nil {
		t.Fatalf("axi logs: %v\n%s", err, logs)
	}
	for _, want := range []string{
		"applied 1 machine-local global review instruction block(s) for changed paths: internal/** (1 file(s))",
		"applied 1 machine-local per-repository review instruction block(s) for changed paths: internal/scm/** (1 file(s))",
		"applied 1 trusted review instruction block(s) for changed paths: internal/scm/** (1 file(s))",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("review step log is missing %q\n%s", want, logs)
		}
	}
}
