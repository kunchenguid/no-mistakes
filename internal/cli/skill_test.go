package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/skill"
)

func TestSkillInstallOutsideRepository(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("NM_HOME", state)
	t.Chdir(t.TempDir())

	for _, base := range skill.InstallBases {
		path := filepath.Join(home, base, skill.Name, "SKILL.md")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("stale skill"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		var out bytes.Buffer
		cmd := newRootCmd()
		cmd.SetOut(&out)
		cmd.SetArgs([]string{"skill", "install"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		for _, base := range skill.InstallBases {
			rel := filepath.Join(base, skill.Name, "SKILL.md")
			content, err := os.ReadFile(filepath.Join(home, rel))
			if err != nil {
				t.Fatal(err)
			}
			if string(content) != skill.Markdown() {
				t.Fatalf("skill at %s was not refreshed", rel)
			}
			if !strings.Contains(out.String(), rel) {
				t.Fatalf("output does not report %s: %s", rel, &out)
			}
		}
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("skill install touched application state: %v", err)
	}
}

func TestSkillInstallFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := os.WriteFile(filepath.Join(home, ".claude"), []byte("blocked"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd := newRootCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"skill", "install"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "install user skill") {
		t.Fatalf("expected installation error, got %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("failed installation reported success: %s", &out)
	}
}

func TestSkillInstallRejectsArguments(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cmd := newRootCmd()
	cmd.SetArgs([]string{"skill", "install", "unexpected"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected unexpected argument to be rejected")
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("invalid invocation wrote to the home directory")
	}
}
