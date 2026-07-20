//go:build e2e

package steps

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

// TestRunCoverageCheck_RealJSProject is the end-to-end coverage check for a real
// JavaScript project. It crosses a process/I/O boundary (c8 + node + npm are
// spawned as subprocesses, coverage-final.json is written and read from disk,
// and git diff drives the changed-file list), so it lives behind the `e2e`
// build tag and runs via `make e2e`, not `go test ./...`.
//
// Scenario mirrors the Go equivalent (TestRunCoverageCheck_RealGoModule): a
// package.json project whose main branch carries a covered src/lib.js plus its
// node:test, and a feature branch that adds an uncovered src/uncovered.js. The
// JS provider (c8 → Istanbul coverage-final.json → blocks) must flag the
// uncovered changed file via a namespaced finding and leave the covered one
// alone.
func TestRunCoverageCheck_RealJSProject(t *testing.T) {
	if _, err := exec.LookPath("npx"); err != nil {
		t.Skipf("npx not available: %v", err)
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skipf("node not available: %v", err)
	}

	dir, baseSHA, headSHA := setupJSProjectRepo(t)
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	// Pin the runner so the test does not depend on package.json script
	// detection or a global npm install; node --test is manifest-free.
	sctx.Env = append(sctx.Env, "NM_JS_TEST_RUNNER=node --test")

	got, tested, err := runCoverageCheck(sctx, baseSHA)
	if err != nil {
		t.Fatalf("runCoverageCheck: %v", err)
	}

	// The covered file (src/lib.js) must NOT be flagged; the uncovered
	// changed file (src/uncovered.js) must produce exactly one namespaced
	// JS finding.
	wantFile := "src/uncovered.js"
	var match *Finding
	for i := range got {
		if !strings.HasPrefix(got[i].ID, uncoveredChangedLinesIDPrefix+"js:") {
			continue
		}
		if got[i].File == wantFile {
			match = &got[i]
			break
		}
	}
	if match == nil {
		t.Fatalf("expected a JS coverage finding for %s, got %+v", wantFile, got)
	}
	if match.Severity != "warning" || match.Action != "ask-user" {
		t.Errorf("unexpected finding shape: %+v", match)
	}
	if !strings.Contains(match.Description, "changed line(s) have no test coverage") {
		t.Errorf("description should report uncovered changed lines, got %q", match.Description)
	}
	// Namespacing (#1 TUI/filter contract): the ID must read
	// `uncovered-changed-lines:js:<path>` so multi-language repos stay distinct.
	wantID := uncoveredChangedLinesIDPrefix + "js:" + wantFile
	if match.ID != wantID {
		t.Errorf("finding ID = %q, want namespaced %q", match.ID, wantID)
	}
	// The covered file must not also be flagged.
	for _, f := range got {
		if f.File == "src/lib.js" && strings.HasPrefix(f.ID, uncoveredChangedLinesIDPrefix) {
			t.Errorf("covered file src/lib.js should not be flagged, got %+v", f)
		}
	}
	// The tested log records the c8-wrapped runner so the user sees what ran.
	if !strings.Contains(tested, "c8") || !strings.Contains(tested, "node --test") {
		t.Errorf("tested log should mention c8 + node --test, got %q", tested)
	}
}

// setupJSProjectRepo builds a JS/TS git repo: a main branch with package.json,
// a covered src/lib.js, and a node:test exercising it; then a feature branch
// that adds an uncovered src/uncovered.js. Returns repo dir, base SHA, head SHA.
func setupJSProjectRepo(t *testing.T) (string, string, string) {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) string { return gitCmd(t, dir, args...) }
	writeRel := func(rel, content string) { mustWrite(t, filepath.Join(dir, rel), content) }

	run("init", "--initial-branch=main")
	writeRel("package.json", `{
  "name": "nm-cov-e2e",
  "version": "1.0.0",
  "type": "module",
  "scripts": {"test": "node --test"}
}
`)
	writeRel("src/lib.js", `export function add(a, b) {
  return a + b;
}
`)
	writeRel("test/lib.test.js", `import { test } from "node:test";
import { add } from "../src/lib.js";

test("add works", () => {
  if (add(1, 2) !== 3) throw new Error("nope");
});
`)
	run("add", "-A")
	run("commit", "-m", "base: covered lib")
	baseSHA := run("rev-parse", "HEAD")

	run("checkout", "-b", "feature")
	// Feature delta: a brand-new untested source file. All its lines are
	// added and none are exercised, so the changed-line check must fire
	// (subsumption of file-level coverage).
	writeRel("src/uncovered.js", `export function unused(x) {
  return x * 2;
}
`)
	run("add", "-A")
	run("commit", "-m", "feature: add uncovered file")
	headSHA := run("rev-parse", "HEAD")
	return dir, baseSHA, headSHA
}
