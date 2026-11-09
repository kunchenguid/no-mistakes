package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

type appendixReadOnlyForge struct {
	body  string
	reads int
}

func (f *appendixReadOnlyForge) GetPRContent(_ context.Context, pr string) (scm.PRContent, error) {
	f.reads++
	if pr != "7" {
		return scm.PRContent{}, fmt.Errorf("read wrong PR: %s", pr)
	}
	return scm.PRContent{Body: f.body}, nil
}

func TestPRAppendixVerifyCommandIsReadOnly(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	home := filepath.Join(root, "nm")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NM_HOME", home)
	dbPath := filepath.Join(home, "state.sqlite")
	if err := os.WriteFile(dbPath, []byte("existing database must never be opened"), 0600); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return string(out)
	}
	git("init")
	refs := git("for-each-ref")
	appendix := "evidence\n<!-- no-mistakes-pipeline-attestation:v1 {\"head_sha\":\"abc\"} -->"
	wrap := func(s string) string {
		return fmt.Sprintf("<!-- no-mistakes-pr-appendix:v1 sha256=%x -->\n%s\n<!-- /no-mistakes-pr-appendix:v1 -->", sha256.Sum256([]byte(s)), s)
	}
	valid := wrap(appendix)
	for _, tc := range []struct{ body, verdict string }{
		{valid, "match"},
		{strings.Replace(valid, "evidence", "Evidence", 1), "mismatch"},
		{"author", "absent"},
		{appendix, "absent"},
		{"<!-- no-mistakes-pr-appendix", "malformed"},
		{valid + valid, "malformed"},
		{strings.ReplaceAll(valid, "<!-- /no-mistakes-pr-appendix:v1 -->", ""), "malformed"},
		{strings.ReplaceAll(valid, "<!-- no-mistakes-pr-appendix:v1 sha256=", ""), "malformed"},
		{strings.ReplaceAll(wrap(appendix+"\x1b ^ERROR"), "\x1b", "^["), "match"},
		{strings.ReplaceAll(wrap(appendix+strings.Repeat("\x1b", 13)), "\x1b", "^["), "match"},
	} {
		body := tc.body
		before, err := os.ReadFile(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		forge := &appendixReadOnlyForge{body: body}
		cmd := newAxiPRVerifyCmdWithReader(func(context.Context, string) (steps.PRAppendixReader, error) {
			return forge.GetPRContent, nil
		})
		cmd.SetArgs([]string{"--repo", "https://github.com/example/repo.git", "--pr", "7"})
		var out bytes.Buffer
		cmd.SetOut(&out)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("command: %v: %s", err, &out)
		}
		if !strings.Contains(out.String(), "verdict: "+tc.verdict) || !strings.Contains(out.String(), "reason:") || forge.reads != 1 {
			t.Fatalf("output %s, reads %d", &out, forge.reads)
		}
		after, err := os.ReadFile(dbPath)
		if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
			t.Fatal("changed local database")
		}
		if forge.body != body {
			t.Fatal("changed input body")
		}
		if got := git("for-each-ref"); got != refs {
			t.Fatal("changed local refs")
		}
		entries, err := os.ReadDir(home)
		if err != nil || len(entries) != 1 {
			t.Fatalf("opened daemon or app resources: %v, %v", entries, err)
		}
	}
}

func TestPRAppendixVerifyRegistered(t *testing.T) {
	cmd, _, err := newAxiCmd().Find([]string{"pr-verify"})
	if err != nil || cmd.Name() != "pr-verify" {
		t.Fatalf("command not registered: %v", err)
	}
}

func TestPRAppendixVerifyRefusesIncompleteRepository(t *testing.T) {
	for _, repo := range []string{
		"gitlab.com/team/target",
		"https://github.com/owner",
		"",
		"owner/repo",
		"https://github.com",
		"https:///owner/repo",
		"https://:443/owner/repo",
		"https://github.com/owner/.git",
		"https://github.com/owner/..",
		"https://github.com/owner/repo?other",
		"https://github.com/owner/repo#other",
		"ssh://git@/owner/repo",
		"ssh://git@github.com/owner",
		"git@github.com:owner",
		"git@:owner/repo",
		":owner/repo",
		"C:/owner/repo",
		`C:\owner\repo`,
		"file:///owner/repo",
		"ftp://github.com/owner/repo",
	} {
		t.Run(repo, func(t *testing.T) {
			resolved, reads := 0, 0
			cmd := newAxiPRVerifyCmdWithReader(func(context.Context, string) (steps.PRAppendixReader, error) {
				resolved++
				return func(context.Context, string) (scm.PRContent, error) {
					reads++
					return scm.PRContent{Body: "author"}, nil
				}, nil
			})
			cmd.SetArgs([]string{"--repo", repo, "--pr", "7"})
			var out bytes.Buffer
			cmd.SetOut(&out)
			err := cmd.Execute()
			var exit *exitError
			if !errors.As(err, &exit) || exit.code != 2 {
				t.Fatalf("error = %v, want exit 2; output = %s", err, &out)
			}
			if resolved != 0 || reads != 0 || strings.Contains(out.String(), "verdict:") {
				t.Fatalf("resolved = %d, reads = %d, output = %s", resolved, reads, &out)
			}
		})
	}
}

func TestPRAppendixVerifyAcceptsFullRepositoryRemotes(t *testing.T) {
	for _, repo := range []string{
		"https://github.com/owner/repo.git",
		"http://gitlab.example/team/sub/repo.git",
		"ssh://git@github.com:22/owner/repo.git",
		"git://github.com/owner/repo.git",
		"git@github.com:owner/repo.git",
		"gitlab.example:team/sub/repo.git",
		"git@work-alias:owner/repo.git",
		"https://codeberg.org/owner/repo.git",
		"https://forgejo.example:3443/git/owner/repo.git",
		"https://dev.azure.com/org/project/_git/repo",
		"git@ssh.dev.azure.com:v3/org/project/repo",
		"https://org.visualstudio.com/project/_git/repo",
		"https://origin.cursor.com/owner/repo.git",
	} {
		t.Run(repo, func(t *testing.T) {
			resolved := 0
			forge := &appendixReadOnlyForge{body: "author"}
			cmd := newAxiPRVerifyCmdWithReader(func(_ context.Context, gotRepo string) (steps.PRAppendixReader, error) {
				resolved++
				if gotRepo != repo {
					t.Fatalf("resolved %q, want %q", gotRepo, repo)
				}
				return forge.GetPRContent, nil
			})
			cmd.SetArgs([]string{"--repo", repo, "--pr", "7"})
			var out bytes.Buffer
			cmd.SetOut(&out)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("command: %v: %s", err, &out)
			}
			if resolved != 1 || forge.reads != 1 || !strings.Contains(out.String(), "verdict: absent") {
				t.Fatalf("resolved = %d, reads = %d, output = %s", resolved, forge.reads, &out)
			}
		})
	}
}
