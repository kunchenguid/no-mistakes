//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAxiTargetBranchJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t)})
	ctx := context.Background()
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := h.runGit(ctx, dir, args...)
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	git(h.WorkDir, "branch", "-m", "master")
	git(h.WorkDir, "push", "origin", "master")
	git(h.UpstreamDir, "symbolic-ref", "HEAD", "refs/heads/master")
	git(h.WorkDir, "push", "origin", "--delete", "main")
	masterSHA := git(h.WorkDir, "rev-parse", "master")
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init with master default: %v\n%s", err, out)
	}

	git(h.WorkDir, "checkout", "-b", "test", "master")
	for i := 0; i < 4; i++ {
		path := filepath.Join(h.WorkDir, fmt.Sprintf("target-%d.txt", i))
		if err := os.WriteFile(path, []byte("target-only history\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(h.WorkDir, "add", filepath.Base(path))
		git(h.WorkDir, "commit", "-m", fmt.Sprintf("target history %d", i))
	}
	testSHA := git(h.WorkDir, "rev-parse", "HEAD")
	git(h.WorkDir, "push", "-u", "origin", "test")

	git(h.WorkDir, "checkout", "-b", "feature/invalid-target")
	if err := os.WriteFile(filepath.Join(h.WorkDir, "invalid.txt"), []byte("invalid target attempt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(h.WorkDir, "add", "invalid.txt")
	git(h.WorkDir, "commit", "-m", "invalid target attempt")
	beforeInvalidAgents := len(h.AgentInvocations())
	invalidOut, invalidErr := h.RunInDir(h.WorkDir, "axi", "run", "--yes", "--intent", "prove invalid target refusal", "--target", "missing")
	if invalidErr == nil || !strings.Contains(invalidOut, "no run started") {
		t.Fatalf("invalid target did not fail before execution: %v\n%s", invalidErr, invalidOut)
	}
	if len(h.AgentInvocations()) != beforeInvalidAgents {
		t.Fatal("invalid target invoked a pipeline agent")
	}
	if _, err := h.runGit(ctx, h.UpstreamDir, "rev-parse", "--verify", "refs/heads/feature/invalid-target"); err == nil {
		t.Fatal("invalid target reached the upstream push phase")
	}
	for _, run := range h.Runs() {
		if run.Branch == "feature/invalid-target" {
			t.Fatalf("invalid target created durable run %s", run.ID)
		}
	}

	git(h.WorkDir, "checkout", "test")
	git(h.WorkDir, "checkout", "-b", "feature/target-contract")
	if err := os.WriteFile(filepath.Join(h.WorkDir, "feature.txt"), []byte("one intended commit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(h.WorkDir, "add", "feature.txt")
	git(h.WorkDir, "commit", "-m", "feature target contract")
	featureHead := git(h.WorkDir, "rev-parse", "HEAD")
	out, err := h.RunInDir(h.WorkDir, "axi", "run", "--yes", "--intent", "validate one commit against test", "--target", "test")
	if err != nil {
		t.Fatalf("axi run --target test: %v\n%s", err, out)
	}
	for _, want := range []string{"outcome: passed", "target_branch: test", "target_sha: " + testSHA} {
		if !strings.Contains(out, want) {
			t.Fatalf("explicit-target output missing %q:\n%s", want, out)
		}
	}
	run := h.WaitForRun("feature/target-contract", 60*time.Second)
	if run.TargetBranch != "test" || run.TargetSHA != testSHA || run.HeadSHA != featureHead {
		t.Fatalf("explicit run identity = head %s target %s@%s, want head %s target test@%s", run.HeadSHA, run.TargetBranch, run.TargetSHA, featureHead, testSHA)
	}
	rebaseLog := readStepLog(t, h, run.ID, "rebase")
	if !strings.Contains(rebaseLog, "target branch test pinned at "+testSHA) || strings.Contains(rebaseLog, "origin/master") {
		t.Fatalf("rebase did not preserve explicit target:\n%s", rebaseLog)
	}
	for _, invocation := range h.AgentInvocations()[beforeInvalidAgents:] {
		if strings.Contains(invocation.Prompt, "Context:\n- branch: feature/target-contract") {
			if !strings.Contains(invocation.Prompt, "base commit: "+testSHA) || !strings.Contains(invocation.Prompt, "target branch: test") {
				t.Fatalf("target-sensitive prompt omitted test identity:\n%s", invocation.Prompt)
			}
			if strings.Contains(invocation.Prompt, "base commit: "+masterSHA) || strings.Contains(invocation.Prompt, "target branch: master") {
				t.Fatalf("target-sensitive prompt fell back to master:\n%s", invocation.Prompt)
			}
		}
	}

	git(h.WorkDir, "checkout", "master")
	git(h.WorkDir, "checkout", "-b", "feature/default-contract")
	if err := os.WriteFile(filepath.Join(h.WorkDir, "default.txt"), []byte("default behavior\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(h.WorkDir, "add", "default.txt")
	git(h.WorkDir, "commit", "-m", "default target behavior")
	defaultWorktree := h.WorkDir
	defaultOut, err := h.RunInDir(defaultWorktree, "axi", "run", "--yes", "--intent", "preserve omitted target behavior")
	if err != nil {
		t.Fatalf("axi run with omitted target: %v\n%s", err, defaultOut)
	}
	defaultRun := h.WaitForRun("feature/default-contract", 60*time.Second)
	if defaultRun.TargetBranch != "master" || defaultRun.TargetSHA != masterSHA {
		t.Fatalf("omitted target = %s@%s, want master@%s", defaultRun.TargetBranch, defaultRun.TargetSHA, masterSHA)
	}
}
