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

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
