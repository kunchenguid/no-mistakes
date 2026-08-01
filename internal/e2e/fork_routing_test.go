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
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	ctx := context.Background()

	parentURL := "https://github.com/parent-owner/no-mistakes.git"
	forkURL := "https://github.com/fork-owner/no-mistakes.git"
	branch := "feature/fork-routing-e2e"

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

	ghLog := filepath.Join(filepath.Dir(h.AgentLog), "gh-fork-routing.log")
	t.Setenv("FAKEAGENT_GH_MODE", "fork-pr")
	t.Setenv("FAKEAGENT_GH_LOG", ghLog)
	t.Setenv("FAKEAGENT_GH_PARENT", "parent-owner/no-mistakes")

	out, err := h.Run("init", "--fork-url", forkURL)
	if err != nil {
		t.Fatalf("init with fork URL: %v\n%s", err, out)
	}

	h.CommitChange(branch, "fork.txt", "fork route\n", "add fork route")
	h.PushToGate(branch)

	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
	}
	if run.PRURL == nil || !strings.HasPrefix(*run.PRURL, "https://github.com/parent-owner/no-mistakes/pull/") {
		t.Fatalf("PR URL = %v, want parent repository PR URL", run.PRURL)
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
			if inv.Repo == "parent-owner/no-mistakes" && inv.Head == "fork-owner:"+branch && inv.Base == "main" {
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
	Body string   `json:"body"`
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
