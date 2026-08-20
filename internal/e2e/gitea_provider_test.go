//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestGiteaProviderJourney exercises the Gitea provider end to end through
// the real pipeline: self-hosted-host detection via tea's own config.yml
// (Gitea carries no distinguishing hostname substring the way gitlab.com or
// github.com do), buildHost wiring the gitea.Host, PR creation, and the CI
// step reading PR state back through the same host. It mirrors
// TestForkRouting's shape (a git URL rewrite standing in for the real
// provider, a stubbed CLI shadowing the real one), using the tea stub added
// to cmd/fakeagent alongside the existing gh stub.
func TestGiteaProviderJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})

	const (
		giteaHost  = "gitea.example.com"
		repoSlug   = "owner/repo"
		loginName  = "e2e"
		remoteURL  = "https://" + giteaHost + "/" + repoSlug + ".git"
		branchName = "feature/gitea-provider-e2e"
	)

	configureGitURLRewrite(t, h, remoteURL, h.UpstreamDir)
	if out, err := h.runGit(t.Context(), h.WorkDir, "remote", "set-url", "origin", remoteURL); err != nil {
		t.Fatalf("set gitea origin: %v\n%s", err, out)
	}

	// tea has no distinguishing hostname substring to detect by (unlike
	// gitlab.com/github.com); scm.DetectProvider falls back to consulting
	// tea's own config.yml for a login matching the remote's host. Isolate
	// XDG_CONFIG_HOME explicitly rather than relying on the harness's HOME
	// fallback, so an ambient XDG_CONFIG_HOME on the test host can never leak
	// into this assertion.
	xdgConfigHome := filepath.Join(h.HomeDir, ".config")
	t.Setenv("XDG_CONFIG_HOME", xdgConfigHome)
	teaConfigDir := filepath.Join(xdgConfigHome, "tea")
	if err := os.MkdirAll(teaConfigDir, 0o755); err != nil {
		t.Fatalf("mkdir tea config dir: %v", err)
	}
	teaConfig := "logins:\n" +
		"    - name: " + loginName + "\n" +
		"      url: https://" + giteaHost + "\n" +
		"      ssh_host: " + giteaHost + "\n" +
		"      user: e2e-tea-user\n" +
		"      token: xxx\n"
	if err := os.WriteFile(filepath.Join(teaConfigDir, "config.yml"), []byte(teaConfig), 0o644); err != nil {
		t.Fatalf("write tea config: %v", err)
	}

	teaLog := filepath.Join(filepath.Dir(h.AgentLog), "tea-provider.log")
	t.Setenv("FAKEAGENT_TEA_LOG", teaLog)
	t.Setenv("FAKEAGENT_TEA_HOST", giteaHost)

	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	h.CommitChange(branchName, "gitea.txt", "gitea provider e2e\n", "add gitea provider e2e file")
	h.PushToGate(branchName)

	run := h.WaitForRun(branchName, 90*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
	}
	if run.PRURL == nil || !strings.HasPrefix(*run.PRURL, "http://"+giteaHost+"/"+repoSlug+"/pulls/") {
		t.Fatalf("PR URL = %v, want a Gitea pull URL under %s/%s", run.PRURL, giteaHost, repoSlug)
	}

	invocations := readTeaStubInvocations(t, teaLog)
	if len(invocations) == 0 {
		t.Fatal("no tea invocations recorded; the pipeline never called the Gitea host")
	}

	var sawAuthCheck, sawCreate, sawWrongLogin bool
	for _, inv := range invocations {
		if len(inv.Args) >= 1 && inv.Args[0] == "api" && inv.Args[len(inv.Args)-1] == "/user" {
			sawAuthCheck = true
		}
		if len(inv.Args) >= 2 && inv.Args[0] == "pulls" && inv.Args[1] == "create" {
			sawCreate = true
			if inv.Repo != repoSlug {
				t.Fatalf("pulls create --repo = %q, want %q: %+v", inv.Repo, repoSlug, inv)
			}
			if inv.Head != branchName {
				t.Fatalf("pulls create --head = %q, want %q: %+v", inv.Head, branchName, inv)
			}
		}
		if inv.Login != "" && inv.Login != loginName {
			sawWrongLogin = true
			t.Errorf("tea invocation used login %q, want the configured %q: %+v", inv.Login, loginName, inv)
		}
	}
	if !sawAuthCheck {
		t.Fatalf("Host.Available never called `tea api --login <name> /user`: %+v", invocations)
	}
	if !sawCreate {
		t.Fatalf("PR step never called `tea pulls create`: %+v", invocations)
	}
	if sawWrongLogin {
		t.Fatalf("a tea invocation targeted the wrong login: %+v", invocations)
	}
}

type teaStubInvocation struct {
	Args  []string `json:"args"`
	Repo  string   `json:"repo"`
	Login string   `json:"login"`
	Head  string   `json:"head"`
	Base  string   `json:"base"`
}

func readTeaStubInvocations(t *testing.T, path string) []teaStubInvocation {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read tea log: %v", err)
	}
	var invocations []teaStubInvocation
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var inv teaStubInvocation
		if err := json.Unmarshal(line, &inv); err != nil {
			t.Fatalf("parse tea log line: %v\n%s", err, line)
		}
		invocations = append(invocations, inv)
	}
	return invocations
}
