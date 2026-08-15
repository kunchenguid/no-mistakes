package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A docs page that is not registered in the Starlight sidebar still builds and
// still answers its URL, so nothing fails - it is simply unreachable by
// navigation, and only someone who already knows the slug can find it. This
// test is the reachability check: every authored page under
// docs/src/content/docs must have a sidebar entry in docs/astro.config.mjs.
// The splash homepage (index.mdx) is deliberately exempt; it is the site root,
// reached through the logo and the hero, never through a sidebar item.
func TestDocsSidebarRegistersEveryAuthoredPage(t *testing.T) {
	config, err := os.ReadFile(filepath.Join("docs", "astro.config.mjs"))
	if err != nil {
		t.Fatalf("read astro config: %v", err)
	}

	registered := map[string]bool{}
	for _, match := range regexp.MustCompile(`slug:\s*"([^"]+)"`).FindAllStringSubmatch(string(config), -1) {
		registered[match[1]] = true
	}

	root := filepath.Join("docs", "src", "content", "docs")
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".md" && ext != ".mdx" {
			return nil
		}
		slug, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		slug = filepath.ToSlash(strings.TrimSuffix(slug, ext))
		if slug == "index" {
			return nil
		}
		if !registered[slug] {
			t.Errorf("docs page %q has no sidebar entry in docs/astro.config.mjs, so readers can only reach it by typing its URL", slug)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk docs content: %v", err)
	}
}

// The mirror direction: a sidebar entry pointing at a slug with no page behind
// it renders a broken navigation link.
func TestDocsSidebarHasNoEntryWithoutAPage(t *testing.T) {
	config, err := os.ReadFile(filepath.Join("docs", "astro.config.mjs"))
	if err != nil {
		t.Fatalf("read astro config: %v", err)
	}

	root := filepath.Join("docs", "src", "content", "docs")
	for _, match := range regexp.MustCompile(`slug:\s*"([^"]+)"`).FindAllStringSubmatch(string(config), -1) {
		slug := match[1]
		base := filepath.Join(root, filepath.FromSlash(slug))
		if fileExists(base+".md") || fileExists(base+".mdx") {
			continue
		}
		t.Errorf("sidebar entry %q has no page under docs/src/content/docs", slug)
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
