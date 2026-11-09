package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
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
