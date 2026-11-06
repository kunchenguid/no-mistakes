//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCommitIdentitySubmissionJourney(t *testing.T) {
	for _, agentCommits := range []bool{false, true} {
		name := "pipeline correction"
		if agentCommits {
			name = "agent correction with intentional author"
		}
		t.Run(name, func(t *testing.T) {
			scenario := configurableFixCommitScenario(t, "guard unsafe value")
			data, err := os.ReadFile(scenario)
			if err != nil {
				t.Fatal(err)
			}
			data = []byte(strings.Replace(string(data), "          action: auto-fix\n", "          action: auto-fix\n          review_scope: source\n", 1))
			if agentCommits {
				data = []byte(strings.Replace(string(data), "    structured:\n      summary:", "    git:\n      - [add, feature.txt]\n      - [commit, '--author=Contributor <contributor@example.test>', -m, 'agent correction']\n    structured:\n      summary:", 1))
			}
			if err := os.WriteFile(scenario, data, 0o600); err != nil {
				t.Fatal(err)
			}
			h := NewHarness(t, SetupOpts{Agent: "codex", Scenario: scenario})
			configPath := filepath.Join(h.NMHome, "config.yaml")
			data, err = os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(configPath, []byte(strings.Replace(string(data), "  review: 0\n", "  review: 1\n", 1)), 0o600); err != nil {
				t.Fatal(err)
			}
			const branch = "identity-correction"
			h.CommitChange(branch, "feature.txt", "unsafe\n", "author's change")
			for _, role := range []string{"AUTHOR", "COMMITTER"} {
				t.Setenv("GIT_"+role+"_NAME", "Daemon")
				t.Setenv("GIT_"+role+"_EMAIL", "daemon@example.test")
			}
			if out, err := h.Run("init"); err != nil {
				t.Fatalf("init private daemon: %v\n%s", err, out)
			}
			// Change the caller's environment after the private daemon starts.
			// Unlike Harness.runGit, this push must retain those explicit values.
			t.Setenv("GIT_AUTHOR_NAME", "Submitting Author")
			t.Setenv("GIT_AUTHOR_EMAIL", "author@example.test")
			t.Setenv("GIT_COMMITTER_NAME", "Submitting Committer")
			t.Setenv("GIT_COMMITTER_EMAIL", "committer@example.test")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "git", "push", "no-mistakes", branch)
			cmd.Dir, cmd.Env = h.WorkDir, os.Environ()
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("push with submitter identity: %v\n%s", err, out)
			}
			gated := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusFixReview, 60*time.Second)
			h.Respond(gated.ID, types.StepReview, types.ActionApprove)
			run := h.WaitForRun(branch, 60*time.Second)
			if run.Status != types.RunCompleted {
				t.Fatalf("identity journey failed: %s, %v", run.Status, run.Error)
			}
			out, err := h.runGit(context.Background(), h.UpstreamDir, "show", "-s", "--format=%an <%ae> | %cn <%ce>", "refs/heads/"+branch)
			if err != nil {
				t.Fatal(err)
			}
			author := "Submitting Author <author@example.test>"
			if agentCommits {
				author = "Contributor <contributor@example.test>"
			}
			want := author + " | Submitting Committer <committer@example.test>"
			if got := strings.TrimSpace(string(out)); got != want {
				t.Fatalf("published correction identity = %q, want %q", got, want)
			}
			t.Logf("published correction identity: %s", want)
		})
	}
}
