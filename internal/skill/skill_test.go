package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMarkdownFrontmatter(t *testing.T) {
	md := Markdown()
	if !strings.HasPrefix(md, "---\n") {
		t.Fatalf("SKILL.md must start with YAML frontmatter, got:\n%s", md[:min(40, len(md))])
	}
	for _, want := range []string{
		"name: " + Name + "\n",
		"description: " + Description + "\n",
		"user-invocable: true\n",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("frontmatter missing %q", want)
		}
	}
	// Frontmatter block must be closed before the body.
	if strings.Count(md, "---\n") < 2 {
		t.Errorf("frontmatter not closed with a second --- delimiter")
	}
	if !strings.Contains(md, "no-mistakes axi run") {
		t.Errorf("body should document the axi run command")
	}
}

func TestInstallWritesBothPaths(t *testing.T) {
	root := t.TempDir()
	written, err := Install(root)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	wantRel := []string{
		filepath.Join(".claude", "skills", Name, "SKILL.md"),
		filepath.Join(".agents", "skills", Name, "SKILL.md"),
	}
	if len(written) != len(wantRel) {
		t.Fatalf("written = %v, want %v", written, wantRel)
	}
	for i, rel := range wantRel {
		if written[i] != rel {
			t.Errorf("written[%d] = %q, want %q", i, written[i], rel)
		}
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if string(data) != Markdown() {
			t.Errorf("%s content does not match Markdown()", rel)
		}
	}
}

func TestInstallIsIdempotent(t *testing.T) {
	root := t.TempDir()
	if _, err := Install(root); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if _, err := Install(root); err != nil {
		t.Fatalf("second install: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".claude", "skills", Name, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != Markdown() {
		t.Errorf("content drifted after re-install")
	}
}

// TestInstallSymlinkLayouts covers repos that consolidate the two skill bases
// with a symlink. `.claude/skills` may link to `.agents/skills`, the whole
// `.claude` dir may link to `.agents`, or the link may point the other way. In
// every case Install must succeed and the skill must be reachable via both
// logical bases - including when the symlink target dir does not exist yet.
func TestInstallSymlinkLayouts(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, root string)
	}{
		{
			name: "claude_skills_link_target_exists",
			setup: func(t *testing.T, root string) {
				mkdirAll(t, filepath.Join(root, ".agents", "skills"))
				mkdirAll(t, filepath.Join(root, ".claude"))
				symlink(t, filepath.Join("..", ".agents", "skills"), filepath.Join(root, ".claude", "skills"))
			},
		},
		{
			name: "claude_skills_link_target_missing",
			setup: func(t *testing.T, root string) {
				mkdirAll(t, filepath.Join(root, ".claude"))
				symlink(t, filepath.Join("..", ".agents", "skills"), filepath.Join(root, ".claude", "skills"))
			},
		},
		{
			name: "claude_dir_link",
			setup: func(t *testing.T, root string) {
				mkdirAll(t, filepath.Join(root, ".agents"))
				symlink(t, ".agents", filepath.Join(root, ".claude"))
			},
		},
		{
			name: "agents_skills_link_reverse",
			setup: func(t *testing.T, root string) {
				mkdirAll(t, filepath.Join(root, ".claude", "skills"))
				mkdirAll(t, filepath.Join(root, ".agents"))
				symlink(t, filepath.Join("..", ".claude", "skills"), filepath.Join(root, ".agents", "skills"))
			},
		},
		{
			name: "agents_dir_link_reverse",
			setup: func(t *testing.T, root string) {
				mkdirAll(t, filepath.Join(root, ".claude"))
				symlink(t, ".claude", filepath.Join(root, ".agents"))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			tt.setup(t, root)

			written, err := Install(root)
			if err != nil {
				t.Fatalf("Install: %v", err)
			}

			// Every reported path must be readable with current content.
			for _, rel := range written {
				data, err := os.ReadFile(filepath.Join(root, rel))
				if err != nil {
					t.Fatalf("read reported %s: %v", rel, err)
				}
				if string(data) != Markdown() {
					t.Errorf("%s content does not match Markdown()", rel)
				}
			}

			// The skill must be discoverable via both logical bases no matter
			// which side carries the symlink.
			for _, base := range InstallBases {
				p := filepath.Join(root, base, Name, "SKILL.md")
				data, err := os.ReadFile(p)
				if err != nil {
					t.Fatalf("skill not reachable via %s: %v", base, err)
				}
				if string(data) != Markdown() {
					t.Errorf("%s content does not match Markdown()", base)
				}
			}
		})
	}
}

// TestInstallOverwritesStaleContent guards the upgrade path: an older SKILL.md
// left by a previous binary version must be refreshed to current content when
// Install runs again.
func TestInstallOverwritesStaleContent(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, ".claude", "skills", Name, "SKILL.md")
	mkdirAll(t, filepath.Dir(stale))
	if err := os.WriteFile(stale, []byte("---\nname: "+Name+"\n---\nstale body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(root); err != nil {
		t.Fatalf("Install: %v", err)
	}
	data, err := os.ReadFile(stale)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != Markdown() {
		t.Errorf("stale SKILL.md was not refreshed to current content")
	}
}

func TestInstallRejectsSymlinkCycle(t *testing.T) {
	root := t.TempDir()
	symlink(t, ".agents", filepath.Join(root, ".claude"))
	symlink(t, ".claude", filepath.Join(root, ".agents"))

	if _, err := Install(root); err == nil {
		t.Fatalf("Install succeeded with cyclic skill directory symlinks")
	}
}

func mkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
