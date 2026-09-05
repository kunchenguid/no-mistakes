package cli

import (
	"bytes"
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
