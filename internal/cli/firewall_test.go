package cli

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/firewall"
)

func TestFirewallGitHubCheck_PublicStdoutOmitsSnippets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("NM_HOME", home)
	diff := filepath.Join(t.TempDir(), "d.diff")
	content := "diff --git a/cfg.txt b/cfg.txt\n--- a/cfg.txt\n+++ b/cfg.txt\n@@ -0,0 +1 @@\n+bind 10.0.0.5\n"
	if err := os.WriteFile(diff, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(t.TempDir(), "private.json")
	root := newRootCmd()
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs([]string{
		"firewall", "github-check",
		"--diff", diff,
		"--repo", "carverauto/serviceradar",
		"--private-json", private,
	})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected non-zero exit for a violation")
	}
	text := out.String()
	if !strings.Contains(text, firewall.PublicPhrase) {
		t.Fatalf("stdout=%q", text)
	}
	if strings.Contains(text, "10.0.0.5") || strings.Contains(text, "cfg.txt") {
		t.Fatalf("github-check leaked match text: %q", text)
	}
	priv, err := os.ReadFile(private)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(priv), "10.0.0.5") {
		t.Fatalf("private json should keep LAN details: %s", priv)
	}
}

func TestFirewallGitHubCheck_CleanDiffPasses(t *testing.T) {
	home := t.TempDir()
	t.Setenv("NM_HOME", home)
	diff := filepath.Join(t.TempDir(), "d.diff")
	content := "diff --git a/docs.md b/docs.md\n--- a/docs.md\n+++ b/docs.md\n@@ -0,0 +1 @@\n+use 192.0.2.1\n"
	if err := os.WriteFile(diff, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"firewall", "github-check", "--diff", diff})
	if err := root.Execute(); err != nil {
		t.Fatalf("clean diff should pass: %v stdout=%q", err, out.String())
	}
	if !strings.Contains(out.String(), "publish-policy ok") {
		t.Fatalf("stdout=%q", out.String())
	}
}

// TestFirewallGitHubCheck_RefusedIngestIsAnErrorNotAViolation covers a large
// but clean pull request whose ingest the portal refuses: the check still
// fails closed, but with the exit code the action reports as `error`, so the
// author is not told their diff carries a Hard Rules value that it does not.
func TestFirewallGitHubCheck_RefusedIngestIsAnErrorNotAViolation(t *testing.T) {
	t.Setenv("NM_HOME", t.TempDir())
	portal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
	}))
	t.Cleanup(portal.Close)

	diff := filepath.Join(t.TempDir(), "d.diff")
	content := "diff --git a/docs.md b/docs.md\n--- a/docs.md\n+++ b/docs.md\n@@ -0,0 +1 @@\n+use 192.0.2.1\n"
	if err := os.WriteFile(diff, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"firewall", "github-check", "--diff", diff, "--portal-url", portal.URL})

	err := root.Execute()
	var exit *exitError
	if !errors.As(err, &exit) {
		t.Fatalf("want a non-zero exit, got %v stdout=%q", err, out.String())
	}
	if exit.code != firewall.ExitError {
		t.Fatalf("exit=%d, want %d (error, not a Hard Rules violation)", exit.code, firewall.ExitError)
	}
	if out.String() != firewall.PublicErrorText(portal.URL) {
		t.Fatalf("unexpected public error: %q", out.String())
	}
	if strings.Contains(out.String(), "192.0.2.1") {
		t.Fatalf("stdout leaked scanned content: %q", out.String())
	}
}

func TestAxiFirewallRespondDoesNotChangeConclusion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("NM_HOME", home)
	store, err := firewall.OpenStore(filepath.Join(home, "firewall.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	v, err := store.Insert(firewall.Input{
		Diff:    "diff --git a/cfg.txt b/cfg.txt\n--- a/cfg.txt\n+++ b/cfg.txt\n@@ -0,0 +1 @@\n+bind 10.0.0.5\n",
		Branch:  "fm/example",
		HeadSHA: "abc",
		Repo:    "carverauto/serviceradar",
	}, firewall.Scan(firewall.Input{Diff: "diff --git a/cfg.txt b/cfg.txt\n--- a/cfg.txt\n+++ b/cfg.txt\n@@ -0,0 +1 @@\n+bind 10.0.0.5\n"}), "http://portal.lan")
	store.Close()
	if err != nil {
		t.Fatal(err)
	}

	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"axi", "firewall", "respond", "--run", v.ID, "--action", "acknowledge"})
	if err := root.Execute(); err != nil {
		t.Fatalf("respond: %v stdout=%q", err, out.String())
	}
	if !strings.Contains(out.String(), "failure") {
		t.Fatalf("expected conclusion failure still present: %q", out.String())
	}
}

func TestFirewallGitHubCheck_MetadataFileReadFailures(t *testing.T) {
	for _, flag := range []string{"--title-file", "--body-file", "--commits-file"} {
		for _, kind := range []string{"missing", "directory", "empty"} {
			t.Run(flag+"/"+kind, func(t *testing.T) {
				t.Setenv("NM_HOME", t.TempDir())
				t.Setenv("NO_MISTAKES_FIREWALL_PORTAL_URL", "")
				path := filepath.Join(t.TempDir(), "metadata")
				if kind == "directory" {
					if err := os.Mkdir(path, 0o700); err != nil {
						t.Fatal(err)
					}
				} else if kind == "empty" {
					if err := os.WriteFile(path, nil, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				root := newRootCmd()
				var out, stderr bytes.Buffer
				root.SetOut(&out)
				root.SetErr(&stderr)
				root.SetIn(strings.NewReader("--- a/docs.md\n+++ b/docs.md\n@@ -0,0 +1 @@\n+use 192.0.2.1\n"))
				root.SetArgs([]string{"firewall", "github-check", flag, path})
				err := root.Execute()
				if kind == "empty" {
					if err != nil || out.String() != "publish-policy ok\n" {
						t.Fatalf("empty metadata: err=%v out=%q", err, out.String())
					}
				} else {
					var exit *exitError
					if !errors.As(err, &exit) || exit.code != firewall.ExitError {
						t.Fatalf("expected ExitError, got %v", err)
					}
					if out.String() != firewall.PublicErrorText("") || stderr.Len() != 0 {
						t.Fatalf("non-generic output: stdout=%q stderr=%q", out.String(), stderr.String())
					}
				}
			})
		}
	}
}
