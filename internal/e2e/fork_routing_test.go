//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestForkRouting(t *testing.T) {
	parentURL := "https://github.com/parent-owner/no-mistakes.git"
	forkURL := "https://github.com/fork-owner/no-mistakes.git"
	branch := "feature/fork-routing-e2e"
	artifactsDir := t.TempDir()
	ghLog := filepath.Join(artifactsDir, "gh-fork-routing.log")
	commandMarker := filepath.Join(artifactsDir, "trusted-command-source")
	t.Setenv("FAKEAGENT_GH_MODE", "fork-pr")
	t.Setenv("FAKEAGENT_GH_LOG", ghLog)
	t.Setenv("FAKEAGENT_GH_PARENT", "parent-owner/no-mistakes")

	allowRepoCommands := false
	h := NewHarness(t, SetupOpts{Agent: "claude", AllowRepoCommands: &allowRepoCommands})
	ctx := context.Background()

	writeConfig := func(source, base string) {
		t.Helper()
		body := fmt.Sprintf("allow_repo_commands: false\ncommands:\n  test: |\n    printf %s > %s\nbase_branch: %s\n", source, shellQuote(commandMarker), base)
		if err := os.WriteFile(filepath.Join(h.WorkDir, ".no-mistakes.yaml"), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s repo config: %v", source, err)
		}
	}
	writeConfig("main", "staging")
	if out, err := h.runGit(ctx, h.WorkDir, "add", ".no-mistakes.yaml"); err != nil {
		t.Fatalf("stage main config: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "commit", "-m", "declare staging pipeline base"); err != nil {
		t.Fatalf("commit main config: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "push", "origin", "main"); err != nil {
		t.Fatalf("push parent main config: %v\n%s", err, out)
	}

	forkDir := filepath.Join(filepath.Dir(h.UpstreamDir), "fork.git")
	if err := os.MkdirAll(forkDir, 0o755); err != nil {
		t.Fatalf("mkdir fork: %v", err)
	}
	if out, err := h.runGit(ctx, forkDir, "init", "--bare", "--initial-branch=main"); err != nil {
		t.Fatalf("init fork: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "push", forkDir, "main"); err != nil {
		t.Fatalf("seed fork main: %v\n%s", err, out)
	}

	configureGitURLRewrite(t, h, parentURL, h.UpstreamDir)
	configureGitURLRewrite(t, h, forkURL, forkDir)
	if out, err := h.runGit(ctx, h.WorkDir, "remote", "set-url", "origin", parentURL); err != nil {
		t.Fatalf("set parent origin: %v\n%s", err, out)
	}

	// The live provider default remains main. Seed a distinct trusted pipeline
	// base in the parent and create the feature from that base.
	if out, err := h.runGit(ctx, h.WorkDir, "checkout", "-b", "staging", "main"); err != nil {
		t.Fatalf("create staging: %v\n%s", err, out)
	}
	writeConfig("staging", "staging")
	if out, err := h.runGit(ctx, h.WorkDir, "add", ".no-mistakes.yaml"); err != nil {
		t.Fatalf("stage staging config: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "commit", "-m", "configure staging base"); err != nil {
		t.Fatalf("commit staging config: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "push", "origin", "staging"); err != nil {
		t.Fatalf("push parent staging: %v\n%s", err, out)
	}

	out, err := h.Run("init", "--fork-url", forkURL, "--base-branch", "staging")
	if err != nil {
		t.Fatalf("init with fork URL: %v\n%s", err, out)
	}
	if !strings.Contains(out, "main") || !strings.Contains(out, "staging (trusted config source)") {
		t.Fatalf("init output did not distinguish repository default and pipeline base:\n%s", out)
	}

	if out, err := h.runGit(ctx, h.WorkDir, "checkout", "-b", branch, "staging"); err != nil {
		t.Fatalf("create feature from staging: %v\n%s", err, out)
	}
	writeConfig("feature", "main")
	h.CommitChange(branch, "fork.txt", "fork route\n", "add fork route")
	h.PushToGate(branch)

	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
	}
	marker, err := os.ReadFile(commandMarker)
	if err != nil {
		t.Fatalf("read trusted command marker: %v", err)
	}
	if got := strings.TrimSpace(string(marker)); got != "staging" {
		t.Fatalf("test command came from %q, want frozen trusted staging config", got)
	}
	if run.PRURL == nil || !strings.HasPrefix(*run.PRURL, "https://github.com/parent-owner/no-mistakes/pull/") {
		ghData, _ := os.ReadFile(ghLog)
		daemonData, _ := os.ReadFile(filepath.Join(h.NMHome, "logs", "daemon.log"))
		var stepLogs strings.Builder
		_ = filepath.Walk(filepath.Join(h.NMHome, "logs", run.ID), func(path string, info os.FileInfo, walkErr error) error {
			if walkErr == nil && info != nil && !info.IsDir() {
				data, _ := os.ReadFile(path)
				fmt.Fprintf(&stepLogs, "\n%s:\n%s", filepath.Base(path), data)
			}
			return nil
		})
		t.Fatalf("PR URL = %v, want parent repository PR URL\ngh log:\n%s\ndaemon log:\n%s\nstep logs:%s", run.PRURL, ghData, daemonData, stepLogs.String())
	}

	forkSHA, err := h.runGit(ctx, forkDir, "rev-parse", "refs/heads/"+branch)
	if err != nil {
		t.Fatalf("fork branch missing: %v\n%s", err, forkSHA)
	}
	if got := string(bytes.TrimSpace(forkSHA)); got != run.HeadSHA {
		t.Fatalf("fork branch SHA = %s, want run head %s", got, run.HeadSHA)
	}
	if out, err := h.runGit(ctx, h.UpstreamDir, "rev-parse", "--verify", "refs/heads/"+branch); err == nil {
		t.Fatalf("parent unexpectedly received feature branch at %s", bytes.TrimSpace(out))
	}

	invocations := readGHStubInvocations(t, ghLog)
	var sawParentCreate bool
	for _, inv := range invocations {
		if len(inv.Args) >= 2 && inv.Args[0] == "pr" && inv.Args[1] == "list" && strings.Contains(inv.Head, ":") {
			t.Fatalf("gh pr list used unsupported owner-qualified head: %+v", inv)
		}
		if len(inv.Args) >= 2 && inv.Args[0] == "pr" && inv.Args[1] == "create" {
			if inv.Repo == "fork-owner/no-mistakes" {
				t.Fatalf("created silent self-PR against fork: %+v", inv)
			}
			if inv.Repo == "parent-owner/no-mistakes" && inv.Head == "fork-owner:"+branch && inv.Base == "staging" {
				sawParentCreate = true
			}
		}
	}
	if !sawParentCreate {
		t.Fatalf("did not see parent PR create with fork owner head in gh log: %+v", invocations)
	}
}

func configureGitURLRewrite(t *testing.T, h *Harness, rawURL, repoDir string) {
	t.Helper()
	rewrite := pathFileURL(t, repoDir)
	key := fmt.Sprintf("url.%s.insteadOf", rewrite)
	if out, err := h.runGit(context.Background(), h.WorkDir, "config", "--global", key, rawURL); err != nil {
		t.Fatalf("configure git URL rewrite %s to %s: %v\n%s", rawURL, rewrite, err, out)
	}
}

func pathFileURL(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("abs %s: %v", path, err)
	}
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}).String()
}

type ghStubInvocation struct {
	Args []string `json:"args"`
	Repo string   `json:"repo"`
	Head string   `json:"head"`
	Base string   `json:"base"`
}

func readGHStubInvocations(t *testing.T, path string) []ghStubInvocation {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read gh log: %v", err)
	}
	var invocations []ghStubInvocation
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var inv ghStubInvocation
		if err := json.Unmarshal(line, &inv); err != nil {
			t.Fatalf("parse gh log line: %v\n%s", err, line)
		}
		invocations = append(invocations, inv)
	}
	return invocations
}
