package agent

import (
	"reflect"
	"testing"
)

func TestClassifyToolCommand_UnwrapsShellAndClassifiesVerbs(t *testing.T) {
	cases := []struct {
		name    string
		command string
		want    []ToolCategory
	}{
		{
			name:    "read through zsh wrapper",
			command: "/run/current-system/sw/bin/zsh -lc 'cat file.txt'",
			want:    []ToolCategory{ToolRead},
		},
		{
			name:    "grep is read",
			command: "bash -lc 'grep -rn foo internal/'",
			want:    []ToolCategory{ToolRead},
		},
		{
			name:    "go test is test_lint",
			command: "bash -lc 'go test ./...'",
			want:    []ToolCategory{ToolTestLint},
		},
		{
			name:    "go build is other",
			command: "bash -lc 'go build ./cmd/x'",
			want:    []ToolCategory{ToolOther},
		},
		{
			name:    "git is git",
			command: "zsh -lc 'git commit -m wip'",
			want:    []ToolCategory{ToolGit},
		},
		{
			name:    "apply_patch is edit",
			command: "bash -lc 'apply_patch <<EOF'",
			want:    []ToolCategory{ToolEdit},
		},
		{
			name:    "sed in place is edit",
			command: "bash -lc 'sed -i.bak s/a/b/ f'",
			want:    []ToolCategory{ToolEdit},
		},
		{
			name:    "sed non-inplace is read",
			command: "bash -lc 'sed -n 1,5p f'",
			want:    []ToolCategory{ToolRead},
		},
		{
			name:    "sleep is wait",
			command: "bash -lc 'sleep 5'",
			want:    []ToolCategory{ToolWait},
		},
		{
			name:    "codex write_stdin poll is wait",
			command: "write_stdin",
			want:    []ToolCategory{ToolWait},
		},
		{
			name:    "shellcheck is test_lint",
			command: "bash -lc 'shellcheck bin/*.sh'",
			want:    []ToolCategory{ToolTestLint},
		},
		{
			name:    "make lint is test_lint, make build is other",
			command: "bash -lc 'make lint'",
			want:    []ToolCategory{ToolTestLint},
		},
		{
			name:    "npm run lint is test_lint",
			command: "bash -lc 'npm run lint'",
			want:    []ToolCategory{ToolTestLint},
		},
		{
			name:    "env-var prefix is skipped",
			command: "bash -lc 'FOO=bar CGO_ENABLED=0 go vet ./...'",
			want:    []ToolCategory{ToolTestLint},
		},
		{
			name:    "sudo wrapper is skipped",
			command: "bash -lc 'sudo rm -rf build'",
			want:    []ToolCategory{ToolEdit},
		},
		{
			name:    "compound command classifies each sub-command",
			command: "bash -lc 'go test ./... && git commit -am wip && cat out.txt'",
			want:    []ToolCategory{ToolTestLint, ToolGit, ToolRead},
		},
		{
			name:    "pipe splits sub-commands",
			command: "bash -lc 'cat log | grep err'",
			want:    []ToolCategory{ToolRead, ToolRead},
		},
		{
			name:    "quoted operators stay within one sub-command",
			command: `rg 'foo|bar;baz'`,
			want:    []ToolCategory{ToolRead},
		},
		{
			name:    "escaped operators stay within one sub-command",
			command: `printf a\|b\;c && git status`,
			want:    []ToolCategory{ToolOther, ToolGit},
		},
		{
			name:    "logical or splits sub-commands",
			command: `rg foo || printf missing`,
			want:    []ToolCategory{ToolRead, ToolOther},
		},
		{
			name:    "no shell wrapper classifies directly",
			command: "git status",
			want:    []ToolCategory{ToolGit},
		},
		{
			name:    "unknown verb is other",
			command: "bash -lc 'curl https://example.com'",
			want:    []ToolCategory{ToolOther},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyToolCommand(tc.command)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ClassifyToolCommand(%q) = %v, want %v", tc.command, got, tc.want)
			}
		})
	}
}

func TestToolCategoryCounts_AddAndTotal(t *testing.T) {
	var c ToolCategoryCounts
	for _, cat := range []ToolCategory{ToolWait, ToolTestLint, ToolTestLint, ToolEdit, ToolRead, ToolGit, ToolOther, "unrecognized"} {
		c.Add(cat)
	}
	want := ToolCategoryCounts{Wait: 1, TestLint: 2, Edit: 1, Read: 1, Git: 1, Other: 2}
	if c != want {
		t.Fatalf("counts = %+v, want %+v", c, want)
	}
	if c.Total() != 8 {
		t.Fatalf("Total() = %d, want 8", c.Total())
	}
}

func TestModelTimeMS(t *testing.T) {
	cases := []struct{ duration, wait, want int64 }{
		{1000, 0, 1000},
		{1000, 300, 700},
		{1000, 1000, 0},
		{1000, 1500, 0},
		{500, -5, 500},
	}
	for _, tc := range cases {
		if got := ModelTimeMS(tc.duration, tc.wait); got != tc.want {
			t.Fatalf("ModelTimeMS(%d,%d) = %d, want %d", tc.duration, tc.wait, got, tc.want)
		}
	}
}

func TestFreshInputTokens(t *testing.T) {
	cases := []struct{ input, cache, want int }{
		{1000, 0, 1000},
		{1000, 800, 200},
		{1000, 1000, 0},
		{1000, 1200, 0},
	}
	for _, tc := range cases {
		if got := FreshInputTokens(tc.input, tc.cache); got != tc.want {
			t.Fatalf("FreshInputTokens(%d,%d) = %d, want %d", tc.input, tc.cache, got, tc.want)
		}
	}
}

// TestPerRoundTokens covers the cumulative-vs-per-invocation delta rule that
// keeps a resumed session's cumulative counter from being mistaken for
// per-round usage.
func TestPerRoundTokens(t *testing.T) {
	cases := []struct {
		name       string
		current    int
		prior      int
		cumulative bool
		want       int
	}{
		{"cumulative resumed round subtracts prior", 1_280_000, 578_000, true, 702_000},
		{"cumulative first round has no prior", 578_000, 0, true, 578_000},
		{"non-cumulative reports current", 702_000, 578_000, false, 702_000},
		{"cumulative shrink treated as per-round", 400_000, 578_000, true, 400_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PerRoundTokens(tc.current, tc.prior, tc.cumulative); got != tc.want {
				t.Fatalf("PerRoundTokens(%d,%d,%v) = %d, want %d", tc.current, tc.prior, tc.cumulative, got, tc.want)
			}
		})
	}
}
