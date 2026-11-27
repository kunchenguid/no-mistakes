package testgit

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// pathGitLookup is the call this guard forbids in test code.
const pathGitLookup = `exec.LookPath("git")`

// guardFile is this file, which names the forbidden call and is skipped.
const guardFile = "internal/testgit/lookpath_guard_test.go"

// TestNoPATHGitLookupInTests keeps every test resolving real git through
// RealGit. exec.LookPath("git") returns the PATH winner, which can be a git
// wrapper; once a test prepends a shim or the fake CLI ahead of it, the two
// forward into each other without bound
// (github.com/ironerumi/no-mistakes#5). Files are read as plain source, so
// tests behind build tags (internal/e2e) are covered too.
func TestNoPATHGitLookupInTests(t *testing.T) {
	root := moduleRoot(t)
	var hits []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == guardFile {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for line := 1; sc.Scan(); line++ {
			if strings.Contains(sc.Text(), pathGitLookup) {
				hits = append(hits, rel+":"+strconv.Itoa(line))
			}
		}
		return sc.Err()
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	for _, hit := range hits {
		t.Errorf("%s: %s resolves git through PATH; use testgit.RealGit()", hit, pathGitLookup)
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}
