package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCommitRenderFixMessage_Default(t *testing.T) {
	t.Parallel()

	got, err := (Commit{}).RenderFixMessage(types.StepReview, "address review findings")
	if err != nil {
		t.Fatal(err)
	}
	want := "no-mistakes(review): address review findings"
	if got != want {
		t.Fatalf("RenderFixMessage() = %q, want %q", got, want)
	}
}

func TestCommitRenderFixMessage_CustomTemplate(t *testing.T) {
	t.Parallel()

	commit := Commit{FixMessage: "chore(no-mistakes-{{.Step}}): {{.Summary}}"}
	got, err := commit.RenderFixMessage(types.StepDocument, "update configuration docs")
	if err != nil {
		t.Fatal(err)
	}
	want := "chore(no-mistakes-document): update configuration docs"
	if got != want {
		t.Fatalf("RenderFixMessage() = %q, want %q", got, want)
	}
}

func TestLoadGlobal_CommitFixMessage(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	const source = "chore(no-mistakes-{{.Step}}): {{.Summary}}"
	data := []byte("commit:\n  fix_message: '" + source + "'\n")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Commit.FixMessage == nil || *cfg.Commit.FixMessage != source {
		t.Fatalf("commit.fix_message = %v, want %q", cfg.Commit.FixMessage, source)
	}
}

func TestLoadGlobal_RejectsInvalidCommitFixMessage(t *testing.T) {
	tests := map[string]string{
		"unknown variable":  "commit:\n  fix_message: '{{.Unknown}}'\n",
		"template function": "commit:\n  fix_message: '{{printf \"%s\" .Summary}}'\n",
		"conditional":       "commit:\n  fix_message: '{{if .Summary}}{{.Summary}}{{end}}'\n",
		"named template":    "commit:\n  fix_message: '{{define \"loop\"}}{{template \"loop\"}}{{end}}{{template \"loop\"}}'\n",
		"malformed syntax":  "commit:\n  fix_message: '{{'\n",
		"empty template":    "commit:\n  fix_message: ''\n",
		"multiline output":  "commit:\n  fix_message: |-\n    first line\n    second line\n",
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
				t.Fatal(err)
			}

			if _, err := LoadGlobal(path); err == nil {
				t.Fatal("LoadGlobal() accepted an invalid commit.fix_message")
			}
		})
	}
}

func TestLoadRepo_CommitFixMessage(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, ".no-mistakes.yaml")
	const source = "{{.Summary}}"
	data := []byte("commit:\n  fix_message: '" + source + "'\n")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Commit.FixMessage == nil || *cfg.Commit.FixMessage != source {
		t.Fatalf("commit.fix_message = %v, want %q", cfg.Commit.FixMessage, source)
	}
}

func TestLoadRepo_RejectsInvalidCommitFixMessage(t *testing.T) {
	t.Parallel()

	_, err := LoadRepoFromBytes([]byte("commit:\n  fix_message: '{{.Unknown}}'\n"))
	if err == nil {
		t.Fatal("LoadRepoFromBytes() accepted an invalid commit.fix_message")
	}
}

func TestMerge_CommitFixMessagePrecedence(t *testing.T) {
	t.Parallel()

	globalSource := "global: {{.Summary}}"
	repoSource := "repo({{.Step}}): {{.Summary}}"
	tests := []struct {
		name   string
		global CommitRaw
		repo   CommitRaw
		want   string
	}{
		{name: "default", want: DefaultFixMessageTemplate},
		{name: "global", global: CommitRaw{FixMessage: &globalSource}, want: globalSource},
		{name: "repo", global: CommitRaw{FixMessage: &globalSource}, repo: CommitRaw{FixMessage: &repoSource}, want: repoSource},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := Merge(&GlobalConfig{Commit: tt.global}, &RepoConfig{Commit: tt.repo})
			if cfg.Commit.FixMessage != tt.want {
				t.Fatalf("commit.fix_message = %q, want %q", cfg.Commit.FixMessage, tt.want)
			}
		})
	}
}
