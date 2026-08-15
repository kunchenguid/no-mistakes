package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// docsContentRoot is the authored-page tree; every slug in the sidebar resolves
// under it.
var docsContentRoot = filepath.Join("docs", "src", "content", "docs")

var sidebarSlugPattern = regexp.MustCompile(`slug:\s*"([^"]+)"`)

// registeredSidebarSlugs reads the sidebar registration out of
// docs/astro.config.mjs. Starlight accepts several entry forms; this reads only
// the explicit double-quoted `slug:` form the config uses today, so it also
// asserts that no other form has appeared. Without that assertion a page
// registered by an `autogenerate` group or a `link:` entry would silently read
// as unregistered, and the reachability check below would fail with a
// misleading "no sidebar entry" message for a page that is in fact reachable.
func registeredSidebarSlugs(t *testing.T) []string {
	t.Helper()

	config, err := os.ReadFile(filepath.Join("docs", "astro.config.mjs"))
	if err != nil {
		t.Fatalf("read astro config: %v", err)
	}
	for _, form := range []string{"autogenerate", "link:"} {
		if strings.Contains(string(config), form) {
			t.Fatalf("docs/astro.config.mjs uses the %q sidebar form, which these tests do not parse; teach registeredSidebarSlugs to read it before using it, or the reachability check will report registered pages as unregistered", form)
		}
	}

	var slugs []string
	for _, match := range sidebarSlugPattern.FindAllStringSubmatch(string(config), -1) {
		slugs = append(slugs, match[1])
	}
	return slugs
}

// A docs page that is not registered in the Starlight sidebar still builds and
// still answers its URL, so nothing fails - it is simply unreachable by
// navigation, and only someone who already knows the slug can find it. This
// test is the reachability check: every authored page under
// docs/src/content/docs must have a sidebar entry in docs/astro.config.mjs.
// The splash homepage (index.mdx) is deliberately exempt; it is the site root,
// reached through the logo and the hero, never through a sidebar item.
func TestDocsSidebarRegistersEveryAuthoredPage(t *testing.T) {
	registered := map[string]bool{}
	for _, slug := range registeredSidebarSlugs(t) {
		registered[slug] = true
	}

	err := filepath.Walk(docsContentRoot, func(path string, info os.FileInfo, err error) error {
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
		slug, err := filepath.Rel(docsContentRoot, path)
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
	for _, slug := range registeredSidebarSlugs(t) {
		base := filepath.Join(docsContentRoot, filepath.FromSlash(slug))
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
