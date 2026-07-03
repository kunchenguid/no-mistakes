package steps

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

// TestParseIstanbulBlocks_CoveredVsUncovered asserts the canonical
// coverage-final.json parse map: each statement becomes a coverBlock with its
// hit count, statements with count>0 stay covered, and zero-count statements
// are flagged by the downstream intersection. Path keys must be relativized to
// the worktree root (the #1 correctness invariant).
func TestParseIstanbulBlocks_CoveredVsUncovered(t *testing.T) {
	t.Parallel()
	workDir := "/repo"

	// One covered statement (line 1, count 1), one uncovered (line 2, count 0),
	// and a branch with one hit arm + one unhit arm on the same line.
	const raw = `{
  "/repo/src/lib.js": {
    "path": "/repo/src/lib.js",
    "statementMap": {
      "0": {"start": {"line": 1, "column": 0}, "end": {"line": 1, "column": 36}},
      "1": {"start": {"line": 2, "column": 0}, "end": {"line": 2, "column": 44}}
    },
    "s": {"0": 1, "1": 0},
    "branchMap": {
      "0": {"type": "branch", "line": 2, "locations": [
        {"start": {"line": 2, "column": 0}, "end": {"line": 2, "column": 22}},
        {"start": {"line": 2, "column": 25}, "end": {"line": 2, "column": 44}}
      ]}
    },
    "b": {"0": [1, 0]}
  },
  "/repo/src/util.ts": {
    "path": "/repo/src/util.ts",
    "statementMap": {
      "0": {"start": {"line": 5, "column": 0}, "end": {"line": 8, "column": 1}}
    },
    "s": {"0": 3}
  }
}`

	blocks := jsCoverageProvider{}.ParseBlocks(raw, workDir)

	// lib.js: 2 statement blocks + 2 branch-location blocks.
	lib := blocks["src/lib.js"]
	if len(lib) != 4 {
		t.Fatalf("src/lib.js expected 4 blocks (2 statements + 2 branch arms), got %d: %+v", len(lib), lib)
	}
	wantStmts := []coverBlock{{1, 1, 1}, {2, 2, 0}}
	for _, w := range wantStmts {
		if !containsCoverBlock(lib, w) {
			t.Errorf("src/lib.js missing statement block %+v; got %+v", w, lib)
		}
	}
	wantBranches := []coverBlock{{2, 2, 1}, {2, 2, 0}}
	for _, w := range wantBranches {
		if !containsCoverBlock(lib, w) {
			t.Errorf("src/lib.js missing branch block %+v; got %+v", w, lib)
		}
	}

	// util.ts: one covered multi-line statement.
	util := blocks["src/util.ts"]
	if len(util) != 1 || util[0] != (coverBlock{5, 8, 3}) {
		t.Errorf("src/util.ts expected [{5 8 3}], got %+v", util)
	}
}

// TestParseIstanbulBlocks_PathRelativization proves the #1 invariant: the parse
// keys must equal what `git diff --name-only` emits (repo-relative POSIX), so
// the coverable-changed-file and added-line maps line up byte-identically.
func TestParseIstanbulBlocks_PathRelativization(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	nested := filepath.Join(dir, "packages", "app", "src", "index.js")
	if err := os.MkdirAll(filepath.Dir(nested), 0o755); err != nil {
		t.Fatal(err)
	}

	// The coverage tool keys by absolute path; git diff emits the repo-relative
	// spelling "packages/app/src/index.js". toRepoRelPOSIX must bridge the two.
	raw := `{"` + jsonPath(nested) + `": {
		"statementMap": {"0": {"start": {"line": 1, "column": 0}, "end": {"line": 1, "column": 10}}},
		"s": {"0": 1}
	}}`

	blocks := jsCoverageProvider{}.ParseBlocks(raw, dir)
	want := "packages/app/src/index.js"
	if _, ok := blocks[want]; !ok {
		t.Errorf("expected key %q (repo-relative POSIX); got keys: %+v", want, mapKeys(blocks))
	}
}

// TestParseIstanbulBlocks_DropsOutOfWorkTree verifies that a coverage entry
// rooted outside the workDir (e.g. a hoisted dependency) does not leak into the
// repo-relative key space and confuse the diff intersection.
func TestParseIstanbulBlocks_DropsOutOfWorkTree(t *testing.T) {
	t.Parallel()
	// Use real temp dirs so the out-of-worktree path is genuinely absolute on
	// every OS — Windows filepath.IsAbs does not recognize Unix-style /paths.
	workDir := t.TempDir()
	outside := jsonPath(filepath.Join(t.TempDir(), "dep", "index.js"))
	raw := `{"` + outside + `": {
	    "statementMap": {"0": {"start": {"line": 1, "column": 0}, "end": {"line": 1, "column": 5}}},
	    "s": {"0": 1}
	  }
	}`
	blocks := jsCoverageProvider{}.ParseBlocks(raw, workDir)
	if len(blocks) != 0 {
		t.Errorf("out-of-worktree path should be dropped, got %v", blocks)
	}
}

// TestParseIstanbulBlocks_EmptyAndMalformed confirms graceful degradation on
// empty input (no blocks → brand-new untested files still fire via subsumption)
// and malformed JSON (no blocks, no panic).
func TestParseIstanbulBlocks_EmptyAndMalformed(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"", "   ", "{not json", "{}"} {
		got := (jsCoverageProvider{}).ParseBlocks(in, "/repo")
		if len(got) != 0 {
			t.Errorf("ParseBlocks(%q) = %+v, want empty map", in, got)
		}
	}
}

// TestJSCoverableChangedFiles exercises the filter: keeps JS/TS source across
// all supported extensions, drops test files by every test-file rule, applies
// ignore patterns, and drops deletions.
func TestJSCoverableChangedFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Materialize the coverable files so the "still exists" check passes.
	files := []string{
		"src/lib.js",
		"src/app.tsx",
		"src/utils.mjs",
		"src/server.cjs",
		"src/component.jsx",
		"src/types.ts",
		"src/__tests__/lib.test.js",
		"test/helpers.js",
		"tests/run.js",
		"src/lib.test.js",
		"src/foo.spec.ts",
		"src/nested/x.js",
	}
	for _, rel := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	changed := append([]string{}, files...)
	changed = append(changed,
		"src/deleted.js", // not on disk → skipped
		"README.md",      // non-JS → skipped
		"",               // blank → skipped
	)
	got := jsCoverageProvider{}.CoverableChangedFiles(changed, dir, nil)

	want := []string{
		"src/component.jsx",
		"src/lib.js",
		"src/nested/x.js",
		"src/server.cjs",
		"src/types.ts",
		"src/utils.mjs",
		"src/app.tsx",
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("coverable files mismatch:\n got: %v\nwant: %v", got, want)
	}

	// Ignore patterns remove matching files.
	got = jsCoverageProvider{}.CoverableChangedFiles(changed, dir, []string{"src/nested/**", "src/types.ts"})
	for _, p := range got {
		if p == "src/nested/x.js" || p == "src/types.ts" {
			t.Errorf("expected %s to be ignored, kept: %v", p, got)
		}
	}
}

// TestJSActive tests that the provider activates only when package.json is at
// the worktree root — the language gate the dispatcher relies on.
func TestJSActive(t *testing.T) {
	t.Parallel()
	withPkg := t.TempDir()
	mustWrite(t, filepath.Join(withPkg, "package.json"), `{"name":"x"}`)
	withoutPkg := t.TempDir()

	p := jsCoverageProvider{}
	if !p.Active(withPkg, nil) {
		t.Errorf("expected Active=true when package.json present")
	}
	if p.Active(withoutPkg, nil) {
		t.Errorf("expected Active=false when package.json absent")
	}
}

// TestJSPkgHasTestScript confirms package.json "scripts.test" detection, which
// drives the npm-test runner fallback.
func TestJSPkgHasTestScript(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"has test script", `{"scripts":{"test":"node --test"}}`, true},
		{"empty test script", `{"scripts":{"test":""}}`, false},
		{"missing scripts", `{"name":"x"}`, false},
		{"unreadable", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if tc.body != "" {
				mustWrite(t, filepath.Join(dir, "package.json"), tc.body)
			}
			if got := pkgHasTestScript(dir); got != tc.want {
				t.Errorf("pkgHasTestScript = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestJSResolveTestRunner locks down the priority order: env override wins,
// then the configured Commands.Test, then the package.json test script, then
// the node --test fallback.
func TestJSResolveTestRunner(t *testing.T) {
	t.Parallel()
	dirWithTest := t.TempDir()
	mustWrite(t, filepath.Join(dirWithTest, "package.json"), `{"scripts":{"test":"jest"}}`)
	dirWithoutTest := t.TempDir()
	mustWrite(t, filepath.Join(dirWithoutTest, "package.json"), `{"name":"x"}`)

	// Fallback: no test script, no commands.test, no env.
	sctx := newTestContext(t, &mockAgent{name: "x"}, dirWithoutTest, "b", "h", config.Commands{})
	if got := resolveJSTestRunner(sctx); got != "node --test" {
		t.Errorf("expected node --test fallback, got %q", got)
	}
	// package.json test script → npm test.
	sctx = newTestContext(t, &mockAgent{name: "x"}, dirWithTest, "b", "h", config.Commands{})
	if got := resolveJSTestRunner(sctx); got != "npm test" {
		t.Errorf("expected npm test from package.json, got %q", got)
	}
	// Configured Commands.Test beats package.json script.
	sctx = newTestContext(t, &mockAgent{name: "x"}, dirWithTest, "b", "h", config.Commands{Test: "vitest run"})
	if got := resolveJSTestRunner(sctx); got != "vitest run" {
		t.Errorf("expected configured test command to win, got %q", got)
	}
	// Env override wins over everything.
	sctx = newTestContext(t, &mockAgent{name: "x"}, dirWithTest, "b", "h", config.Commands{Test: "vitest run"})
	sctx.Env = []string{"NM_JS_TEST_RUNNER=bun test"}
	if got := resolveJSTestRunner(sctx); got != "bun test" {
		t.Errorf("expected NM_JS_TEST_RUNNER to override, got %q", got)
	}
}

// TestJSIsJSTestFile covers every branch of the JS test-file rule so the
// coverable filter doesn't accidentally hold test files accountable (or drop a
// real source file).
func TestJSIsJSTestFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path string
		want bool
	}{
		{"src/foo.test.js", true},
		{"src/foo.test.tsx", true},
		{"src/foo.spec.ts", true},
		{"src/foo.test.unit.js", true},
		{"src/foo.mjs", false},
		{"src/lib.js", false},
		{"src/app.tsx", false},
		{"__tests__/helper.js", true},
		{"src/__tests__/nested.js", true},
		{"test/run.js", true},
		{"tests/setup.js", true},
		{"src/test.js", false}, // bare test.js is not under a test/ dir
		{"vendor/tests-helper/index.js", false},
	}
	for _, tc := range tests {
		if got := isJSTestFile(tc.path); got != tc.want {
			t.Errorf("isJSTestFile(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func containsCoverBlock(blocks []coverBlock, want coverBlock) bool {
	for _, b := range blocks {
		if b == want {
			return true
		}
	}
	return false
}

func mapKeys(m map[string][]coverBlock) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
