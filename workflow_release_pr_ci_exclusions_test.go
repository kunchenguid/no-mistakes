package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestPullRequestWorkflowsExcludeReleasePleaseOutputs is the drift check for
// the release-please zero-run path filter. release-please opens its PR with
// GITHUB_TOKEN, which creates pull_request runs that sit in action_required
// forever; the only native way to create zero runs is to exclude every path
// release-please writes from each pull_request trigger. This test derives the
// expected output set from release-please-config.json so adding an extra-files
// entry (or changing release-type) without updating the workflows fails CI.
func TestPullRequestWorkflowsExcludeReleasePleaseOutputs(t *testing.T) {
	expected := expectedReleasePleaseOutputs(t)
	if len(expected) == 0 {
		t.Fatal("expected release-please output set is empty")
	}

	entries, err := os.ReadDir(".github/workflows")
	if err != nil {
		t.Fatalf("read workflows: %v", err)
	}

	checked := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml") {
			continue
		}
		path := filepath.Join(".github/workflows", name)
		pr, ok := loadWorkflowPullRequest(t, path)
		if !ok {
			continue
		}
		checked++
		for _, output := range expected {
			if !pullRequestExcludesPath(pr, output) {
				t.Errorf("%s pull_request must exclude release-please output %q (via paths-ignore, a trailing negated paths entry, or by omitting it from a paths allow-list)", path, output)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no pull_request workflows found to check")
	}
}

// expectedReleasePleaseOutputs derives the file set release-please writes for
// this repository from release-please-config.json. Always includes the
// manifest; release-type selects the strategy outputs; every configured
// extra-files path is included.
func expectedReleasePleaseOutputs(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile("release-please-config.json")
	if err != nil {
		t.Fatalf("read release-please-config.json: %v", err)
	}
	var cfg struct {
		Packages map[string]struct {
			ReleaseType string   `json:"release-type"`
			ExtraFiles  []string `json:"extra-files"`
			Component   string   `json:"component"`
			PackageName string   `json:"package-name"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse release-please-config.json: %v", err)
	}
	if len(cfg.Packages) == 0 {
		t.Fatal("release-please-config.json has no packages")
	}

	seen := map[string]struct{}{}
	var out []string
	add := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" {
			return
		}
		if _, ok := seen[path]; ok {
			return
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	add(".release-please-manifest.json")

	for pkgPath, pkg := range cfg.Packages {
		prefix := ""
		if pkgPath != "." {
			prefix = strings.TrimSuffix(pkgPath, "/") + "/"
		}
		switch pkg.ReleaseType {
		case "go", "simple", "rust", "python", "elixir", "terraform-module":
			add(prefix + "CHANGELOG.md")
		case "node", "nodejs":
			add(prefix + "CHANGELOG.md")
			add(prefix + "package.json")
			// npm lockfile only when the package still carries one; pnpm
			// workspaces keep a root pnpm-lock.yaml that release-please does
			// not write, so it must not enter the ignore set.
			lock := prefix + "package-lock.json"
			if _, err := os.Stat(lock); err == nil {
				add(lock)
			}
		case "":
			t.Fatalf("package %q missing release-type", pkgPath)
		default:
			// Unknown strategy: still require CHANGELOG.md, which every
			// built-in strategy writes, and rely on extra-files for the rest.
			add(prefix + "CHANGELOG.md")
		}
		if pkg.ReleaseType == "simple" {
			add(prefix + "version.txt")
		}
		for _, extra := range pkg.ExtraFiles {
			// extra-files entries may be objects in richer configs; this repo
			// uses plain path strings. Non-strings are ignored by the typed
			// decode above and must be represented as strings in config.
			if prefix != "" && !strings.Contains(extra, "/") && !strings.HasPrefix(extra, "!") {
				add(prefix + extra)
				continue
			}
			add(extra)
		}
	}
	return out
}

func loadWorkflowPullRequest(t *testing.T, path string) (map[string]any, bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	// Decode via yaml.Node so a bare `on:` key (YAML 1.1 bool true) and a
	// plain string "on" key are both visible; map[string]any alone can drop
	// the boolean-key form depending on decoder settings.
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	onNode := workflowOnNode(&root)
	if onNode == nil {
		return nil, false
	}
	var on map[string]any
	if err := onNode.Decode(&on); err != nil {
		t.Fatalf("decode on: in %s: %v", path, err)
	}
	pr, ok := on["pull_request"]
	if !ok {
		return nil, false
	}
	switch v := pr.(type) {
	case nil:
		// bare `pull_request:` with no filters
		return map[string]any{}, true
	case map[string]any:
		return v, true
	default:
		t.Fatalf("%s: pull_request trigger has unexpected type %T", path, pr)
		return nil, false
	}
}

// workflowOnNode finds the workflow's on: mapping node under a document root.
func workflowOnNode(root *yaml.Node) *yaml.Node {
	if root == nil {
		return nil
	}
	doc := root
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		doc = root.Content[0]
	}
	if doc.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(doc.Content); i += 2 {
		k := doc.Content[i]
		v := doc.Content[i+1]
		if k.Value == "on" || (k.Tag == "!!bool" && k.Value == "true") {
			if v.Kind == yaml.MappingNode {
				return v
			}
			return nil
		}
	}
	return nil
}

// pullRequestExcludesPath reports whether a pull_request trigger will create
// no run when the PR changes only paths that should be ignored for
// release-please, specifically whether `path` cannot alone cause a run.
func pullRequestExcludesPath(pr map[string]any, path string) bool {
	if ignores, ok := stringSlice(pr["paths-ignore"]); ok {
		for _, pattern := range ignores {
			if pathMatch(pattern, path) {
				return true
			}
		}
		// paths-ignore present but path not listed: a PR that also changes
		// only other ignored paths still runs if this path is present. Not excluded.
		return false
	}
	if paths, ok := stringSlice(pr["paths"]); ok {
		// Allow-list: excluded if no positive pattern matches, or a positive
		// match is later cancelled by a negation. GitHub requires negations to
		// follow the positive patterns they narrow.
		matched := false
		for _, pattern := range paths {
			if strings.HasPrefix(pattern, "!") {
				if matched && pathMatch(strings.TrimPrefix(pattern, "!"), path) {
					return true
				}
				continue
			}
			if pathMatch(pattern, path) {
				matched = true
			}
		}
		return !matched
	}
	// Bare pull_request with neither filter runs on every path.
	return false
}

func stringSlice(v any) ([]string, bool) {
	switch t := v.(type) {
	case nil:
		return nil, false
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			s, ok := item.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	case []string:
		return t, true
	default:
		return nil, false
	}
}

// pathMatch implements the subset of GitHub paths filter semantics needed for
// exact release-output paths: exact match, '**' / '*' globs, and leading '/'.
func pathMatch(pattern, path string) bool {
	pattern = strings.TrimPrefix(pattern, "/")
	path = strings.TrimPrefix(path, "/")
	if pattern == path {
		return true
	}
	// filepath.Match does not treat ** specially; handle common forms used in
	// this fleet before falling back to path.Match for single-segment globs.
	if strings.Contains(pattern, "**") {
		// prefix/**
		if strings.HasSuffix(pattern, "/**") {
			prefix := strings.TrimSuffix(pattern, "/**")
			return path == prefix || strings.HasPrefix(path, prefix+"/")
		}
		// **/*.ext
		if strings.HasPrefix(pattern, "**/") {
			rest := strings.TrimPrefix(pattern, "**/")
			if !strings.Contains(rest, "*") {
				return path == rest || strings.HasSuffix(path, "/"+rest)
			}
			// **/*.md
			if strings.HasPrefix(rest, "*.") {
				ext := strings.TrimPrefix(rest, "*")
				return strings.HasSuffix(path, ext)
			}
		}
	}
	ok, err := filepath.Match(pattern, path)
	return err == nil && ok
}
