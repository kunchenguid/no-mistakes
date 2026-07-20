package steps

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

func TestParseGoCoverProfile(t *testing.T) {
	t.Parallel()
	const module = "example.com/test"
	const profile = `mode: set
example.com/test/pkg/covered.go:10.24,12.2 1 1
example.com/test/pkg/covered.go:15.24,17.2 1 0
example.com/test/pkg/uncovered.go:5.24,7.2 1 0
example.com/test/pkg/uncovered.go:9.24,11.2 1 0
example.com/other/pkg/x.go:1.1,2.2 1 1
`
	covered := parseGoCoverProfile(profile, module)
	if !covered["pkg/covered.go"] {
		t.Errorf("expected pkg/covered.go to be covered (at least one hit block), got %v", covered)
	}
	if covered["pkg/uncovered.go"] {
		t.Errorf("expected pkg/uncovered.go to be uncovered (all blocks zero), got %v", covered)
	}
	if _, present := covered["example.com/other/pkg/x.go"]; present {
		t.Errorf("expected out-of-module paths to be ignored, got %v", covered)
	}
	if len(covered) != 1 {
		t.Errorf("expected exactly one covered repo-relative file, got %v", covered)
	}

	// count/atomic modes use arbitrary integer counts; >0 still counts as covered.
	const countMode = `mode: count
example.com/test/a.go:1.1,2.2 1 5
example.com/test/b.go:1.1,2.2 1 0
`
	got := parseGoCoverProfile(countMode, module)
	if !got["a.go"] || got["b.go"] {
		t.Errorf("count-mode coverage mismatch, got %v", got)
	}

	// Empty input is safe.
	if g := parseGoCoverProfile("", module); len(g) != 0 {
		t.Errorf("expected empty map for empty profile, got %v", g)
	}
	// Unknown module path: entries are kept as-is (no prefix stripping).
	noMod := parseGoCoverProfile("foo/bar.go:1.1,2.2 1 1\n", "")
	if !noMod["foo/bar.go"] {
		t.Errorf("expected raw path when module path unknown, got %v", noMod)
	}
}

func TestCoverableChangedGoFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Create files on disk so the "still exists" check passes.
	mustWrite(t, filepath.Join(dir, "src.go"), "package x\n")
	mustWrite(t, filepath.Join(dir, "src_test.go"), "package x\n")
	mustWrite(t, filepath.Join(dir, "gen.go"), "package x\n")
	mustWrite(t, filepath.Join(dir, "readme.md"), "readme\n")

	changed := []string{
		"src.go",        // coverable
		"src_test.go",   // test file: skipped
		"gen.go",        // coverable but will be ignored below
		"readme.md",     // non-Go: skipped
		"deleted.go",    // not on disk: skipped
		"pkg/nested.go", // coverable (exists below)
		"",              // blank: skipped
	}
	mustWrite(t, filepath.Join(dir, "pkg", "nested.go"), "package pkg\n")

	got := coverableChangedGoFiles(changed, dir, nil)
	if len(got) != 3 || !contains(got, "src.go") || !contains(got, "gen.go") || !contains(got, "pkg/nested.go") {
		t.Errorf("expected src.go, gen.go, pkg/nested.go, got %v", got)
	}

	// Ignore patterns remove matching files.
	got = coverableChangedGoFiles(changed, dir, []string{"gen.go", "pkg/**"})
	if len(got) != 1 || got[0] != "src.go" {
		t.Errorf("expected only src.go after ignores, got %v", got)
	}
}

func TestUncoveredFileFindings(t *testing.T) {
	t.Parallel()
	coverable := []string{"a/covered.go", "a/bare.go", "c/zero.go"}
	covered := map[string]bool{"a/covered.go": true, "a/bare.go": false}

	findings := uncoveredFileFindings(coverable, covered)
	if len(findings) != 2 {
		t.Fatalf("expected 2 uncovered findings, got %d: %+v", len(findings), findings)
	}

	seen := make(map[string]bool, len(findings))
	for _, f := range findings {
		seen[f.File] = true
		if f.Severity != "warning" {
			t.Errorf("expected warning severity for %s, got %s", f.File, f.Severity)
		}
		if f.Action != "ask-user" {
			t.Errorf("expected ask-user action for %s, got %s", f.File, f.Action)
		}
		if !strings.HasPrefix(f.ID, uncoveredChangedFileIDPrefix) {
			t.Errorf("expected ID prefix %q for %s, got %s", uncoveredChangedFileIDPrefix, f.File, f.ID)
		}
		if !strings.Contains(f.Description, "0% test coverage") {
			t.Errorf("expected description to mention 0%% coverage for %s, got %s", f.File, f.Description)
		}
	}
	if !seen["a/bare.go"] || !seen["c/zero.go"] {
		t.Errorf("expected bare.go (false) and zero.go (absent), got %v", seen)
	}
	if seen["a/covered.go"] {
		t.Errorf("did not expect covered file to be flagged, got %v", seen)
	}
}

func TestGoModulePath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module github.com/foo/bar\n\ngo 1.25.0\n")
	if got := goModulePath(dir); got != "github.com/foo/bar" {
		t.Errorf("goModulePath = %q, want github.com/foo/bar", got)
	}
	// Missing go.mod.
	empty := t.TempDir()
	if got := goModulePath(empty); got != "" {
		t.Errorf("goModulePath with no go.mod = %q, want empty", got)
	}
}

func TestCoverageCheckEnabled(t *testing.T) {
	// The "default" cases pass a nil env slice and rely on coverageCheckEnabled
	// falling back to the go.mod presence check. That fallback is only reached
	// when NO_MISTAKES_COVERAGE_CHECK is unset in the OS environment, so
	// neutralize the ambient value here — otherwise running the suite with the
	// variable exported (e.g. the coverage-aware dogfood run) flips the defaults
	// and makes the test non-deterministic. Not parallel because we touch os env.
	prev, hadPrev := os.LookupEnv("NO_MISTAKES_COVERAGE_CHECK")
	os.Unsetenv("NO_MISTAKES_COVERAGE_CHECK")
	t.Cleanup(func() {
		if hadPrev {
			os.Setenv("NO_MISTAKES_COVERAGE_CHECK", prev)
		} else {
			os.Unsetenv("NO_MISTAKES_COVERAGE_CHECK")
		}
	})

	goDir := t.TempDir()
	mustWrite(t, filepath.Join(goDir, "go.mod"), "module x\n\ngo 1.21\n")
	nonGoDir := t.TempDir()

	tests := []struct {
		name string
		dir  string
		env  []string
		want bool
	}{
		{"default go project on", goDir, nil, true},
		{"default non-go project off", nonGoDir, nil, false},
		{"explicit enable on non-go", nonGoDir, []string{"NO_MISTAKES_COVERAGE_CHECK=1"}, true},
		{"explicit disable on go", goDir, []string{"NO_MISTAKES_COVERAGE_CHECK=0"}, false},
		{"explicit false", goDir, []string{"NO_MISTAKES_COVERAGE_CHECK=false"}, false},
		{"explicit true uppercase", nonGoDir, []string{"NO_MISTAKES_COVERAGE_CHECK=TRUE"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := coverageCheckEnabled(tc.dir, tc.env); got != tc.want {
				t.Errorf("coverageCheckEnabled(%q, %v) = %v, want %v", tc.dir, tc.env, got, tc.want)
			}
		})
	}
}

// TestRunCoverageCheck_RealGoModule verifies the orchestrator end-to-end on a
// real Go module: a covered source file (+test) committed on main, then a
// feature commit that adds an uncovered source file. The check must flag the
// uncovered changed file and leave the covered one alone.
func TestRunCoverageCheck_RealGoModule(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go toolchain not available: %v", err)
	}

	dir, baseSHA, headSHA := setupGoModuleRepo(t, "pkg")
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "go test ./..."})

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}

	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatal(err)
	}
	t.Logf("coverage findings JSON: %s", outcome.Findings)

	var uncovered []Finding
	for _, f := range findings.Items {
		if strings.HasPrefix(f.ID, uncoveredChangedLinesIDPrefix) {
			uncovered = append(uncovered, f)
		}
	}
	if len(uncovered) != 1 {
		t.Fatalf("expected exactly one coverage finding, got %d: %+v", len(uncovered), findings.Items)
	}
	want := "pkg/uncovered.go"
	if uncovered[0].File != want {
		t.Errorf("expected finding for %s, got %s", want, uncovered[0].File)
	}
	if uncovered[0].Action != "ask-user" || uncovered[0].Severity != "warning" {
		t.Errorf("unexpected finding shape: %+v", uncovered[0])
	}
	if !strings.Contains(uncovered[0].Description, "changed line(s) have no test coverage") {
		t.Errorf("expected changed-line description, got %s", uncovered[0].Description)
	}
	if !outcome.NeedsApproval {
		t.Error("expected NeedsApproval when an uncovered changed file is flagged")
	}
	recordedCoverage := false
	for _, entry := range findings.Tested {
		if strings.Contains(entry, "go test -cover") {
			recordedCoverage = true
			break
		}
	}
	if !recordedCoverage {
		t.Errorf("expected coverage run recorded in tested, got %v", findings.Tested)
	}
}

// TestRunCoverageCheck_DisabledByEnv confirms the check is a no-op when the
// gate flag is turned off, even on a Go project with an uncovered changed file.
func TestRunCoverageCheck_DisabledByEnv(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go toolchain not available: %v", err)
	}
	dir, baseSHA, headSHA := setupGoModuleRepo(t, "pkg")
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "go test ./..."})
	sctx.Env = []string{"NO_MISTAKES_COVERAGE_CHECK=0"}

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("did not expect approval when coverage check disabled")
	}
	var findings Findings
	json.Unmarshal([]byte(outcome.Findings), &findings)
	for _, f := range findings.Items {
		if strings.HasPrefix(f.ID, uncoveredChangedLinesIDPrefix) {
			t.Errorf("did not expect coverage finding when disabled, got %+v", f)
		}
	}
}

// TestRunCoverageCheck_NoCoverableChanges skips the expensive cover run when
// the diff only touches non-Go or test files.
func TestRunCoverageCheck_NoCoverableChanges(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go toolchain not available: %v", err)
	}
	dir := t.TempDir()
	run := func(args ...string) string { return gitCmd(t, dir, args...) }
	writeRel := func(rel, content string) { mustWrite(t, filepath.Join(dir, rel), content) }

	run("init", "--initial-branch=main")
	writeRel("go.mod", "module example.com/test\n\ngo 1.21\n")
	writeRel("pkg/source.go", "package pkg\n\nfunc Source() int { return 1 }\n")
	writeRel("pkg/source_test.go", "package pkg\n\nimport \"testing\"\n\nfunc TestSource(t *testing.T) { _ = Source() }\n")
	run("add", "-A")
	run("commit", "-m", "base")
	baseSHA := run("rev-parse", "HEAD")

	run("checkout", "-b", "feature")
	// Feature delta is test-only + a markdown doc: no coverable source files.
	writeRel("pkg/extra_test.go", "package pkg\n\nimport \"testing\"\n\nfunc TestExtra(t *testing.T) { _ = 1 }\n")
	writeRel("notes.md", "# notes\n")
	run("add", "-A")
	run("commit", "-m", "feature: add test + docs")
	headSHA := run("rev-parse", "HEAD")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "go test ./..."})

	got, tested, err := runCoverageCheck(sctx, baseSHA)
	if err != nil {
		t.Fatalf("runCoverageCheck: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no findings when only test/non-Go files changed, got %v", got)
	}
	if tested != "" {
		t.Errorf("expected no coverage run when no coverable files changed, got tested=%q", tested)
	}
}

func TestRunCoverageCheck_NoGoModule_SkipsGracefully(t *testing.T) {
	t.Parallel()
	// A plain git repo with no go.mod: the default gate keeps the check off.
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "true"})
	got, _, err := runCoverageCheck(sctx, baseSHA)
	if err != nil {
		t.Fatalf("expected graceful skip on non-Go repo, got err=%v", err)
	}
	if got != nil {
		t.Errorf("expected no findings on non-Go repo, got %v", got)
	}
}

func TestParseCoverLoc(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in         string
		start, end int
		ok         bool
	}{
		{"10.24,12.2", 10, 12, true},
		{"3.16,3.28", 3, 3, true},
		{"1.1,1.1", 1, 1, true},
		{"nocomma", 0, 0, false},
		{"", 0, 0, false},
		{"x.y,z.w", 0, 0, false},
	}
	for _, tc := range tests {
		s, e, ok := parseCoverLoc(tc.in)
		if ok != tc.ok || s != tc.start || e != tc.end {
			t.Errorf("parseCoverLoc(%q) = (%d,%d,%v), want (%d,%d,%v)", tc.in, s, e, ok, tc.start, tc.end, tc.ok)
		}
	}
}

func TestParseHunkAddedRange(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want addedLineRange
		ok   bool
	}{
		{"@@ -10,5 +15,8 @@ ctx", addedLineRange{15, 22}, true},
		{"@@ -10 +15 @@", addedLineRange{15, 15}, true},
		{"@@ -0,0 +1,3 @@", addedLineRange{1, 3}, true}, // brand-new file
		{"@@ -1,3 +0,0 @@\n", addedLineRange{}, false},  // pure deletion
		{"@@@ -1,2 +1,2 @@@@", addedLineRange{}, false}, // not a @@ hunk start
		{"no hunk here", addedLineRange{}, false},
	}
	for _, tc := range tests {
		got, ok := parseHunkAddedRange(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseHunkAddedRange(%q) = (%+v,%v), want (%+v,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestParseAddedLineRanges(t *testing.T) {
	t.Parallel()
	const diff = `diff --git a/pkg/a.go b/pkg/a.go
index 1..2 100644
--- a/pkg/a.go
+++ b/pkg/a.go
@@ -3,0 +4,2 @@ func One() int { return 1 }
+
+func Two() int { return 2 }
diff --git a/pkg/b.go b/pkg/b.go
--- a/pkg/b.go
+++ b/pkg/b.go
@@ -5,2 +6,0 @@ ctx
-old
-old2
diff --git a/pkg/new.go b/pkg/new.go
--- a/pkg/new.go
+++ b/pkg/new.go
@@ -0,0 +1,4 @@
+package pkg
+
+func New() int { return 9 }
+}
`
	got := parseAddedLineRanges(diff)
	if r := got["pkg/a.go"]; len(r) != 1 || r[0] != (addedLineRange{4, 5}) {
		t.Errorf("pkg/a.go added ranges = %+v, want [{4 5}]", r)
	}
	if r, ok := got["pkg/b.go"]; ok && len(r) > 0 {
		t.Errorf("pkg/b.go pure-deletion should add no range, got %+v", r)
	}
	if r := got["pkg/new.go"]; len(r) != 1 || r[0] != (addedLineRange{1, 4}) {
		t.Errorf("pkg/new.go added ranges = %+v, want [{1 4}]", r)
	}
	if g := parseAddedLineRanges(""); len(g) != 0 {
		t.Errorf("empty diff should yield empty map, got %v", g)
	}
}

func TestParseGoCoverProfileBlocks(t *testing.T) {
	t.Parallel()
	const module = "example.com/test"
	const profile = `mode: set
example.com/test/pkg/math.go:3.16,3.28 1 1
example.com/test/pkg/math.go:5.16,5.28 1 0
example.com/test/pkg/other.go:2.1,4.2 2 0
example.com/other/x.go:1.1,2.2 1 1
`
	blocks := parseGoCoverProfileBlocks(profile, module)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 in-module files, got %v", blocks)
	}
	if b := blocks["pkg/math.go"]; len(b) != 2 || b[0] != (coverBlock{3, 3, 1}) || b[1] != (coverBlock{5, 5, 0}) {
		t.Errorf("pkg/math.go blocks = %+v, want [{3 3 1} {5 5 0}]", b)
	}
	if b := blocks["pkg/other.go"]; len(b) != 1 || b[0] != (coverBlock{2, 4, 0}) {
		t.Errorf("pkg/other.go blocks = %+v, want [{2 4 0}]", b)
	}
	if _, present := blocks["example.com/other/x.go"]; present {
		t.Errorf("out-of-module paths should be skipped, got %v", blocks)
	}
}

func TestUncoveredChangedLineFindings(t *testing.T) {
	t.Parallel()
	coverable := []string{"pkg/covered.go", "pkg/partial.go", "pkg/uncovered.go", "pkg/noadded.go"}

	added := map[string][]addedLineRange{
		"pkg/covered.go":   {{1, 5}},  // all within count>0 block
		"pkg/partial.go":   {{1, 10}}, // spans a count==0 block region
		"pkg/uncovered.go": {{1, 4}},  // file has no blocks → subsumption
		// pkg/noadded.go absent → skip
	}
	blocks := map[string][]coverBlock{
		"pkg/covered.go": {{1, 5, 1}},
		"pkg/partial.go": {{1, 3, 1}, {4, 6, 0}}, // lines 4-6 executable & uncovered
		// pkg/uncovered.go has no blocks
	}
	got := uncoveredChangedLineFindings(coverable, added, blocks)

	seen := map[string]Finding{}
	for _, f := range got {
		seen[f.File] = f
		if !strings.HasPrefix(f.ID, uncoveredChangedLinesIDPrefix) {
			t.Errorf("ID prefix wrong for %s: %s", f.File, f.ID)
		}
		if f.Severity != "warning" || f.Action != "ask-user" {
			t.Errorf("shape wrong for %s: %+v", f.File, f)
		}
	}
	if _, ok := seen["pkg/covered.go"]; ok {
		t.Errorf("fully-covered file should not be flagged, got %+v", got)
	}
	if _, ok := seen["pkg/noadded.go"]; ok {
		t.Errorf("file with no added lines should not be flagged, got %+v", got)
	}
	pf, ok := seen["pkg/partial.go"]
	if !ok {
		t.Fatalf("expected partial.go finding, got %+v", got)
	}
	if !strings.Contains(pf.Description, "3 changed line(s)") {
		t.Errorf("partial.go description should report 3 uncovered lines, got %q", pf.Description)
	}
	uf, ok := seen["pkg/uncovered.go"]
	if !ok {
		t.Fatalf("expected uncovered.go (no blocks) finding for subsumption, got %+v", got)
	}
	if !strings.Contains(uf.Description, "4 changed line(s)") {
		t.Errorf("uncovered.go description should report 4 uncovered lines, got %q", uf.Description)
	}
}

// TestRunCoverageCheck_DiffLevel covers the four core diff-intersection
// scenarios through the real orchestrator on a synthetic Go module:
// fully-covered additions, fully-uncovered additions, partially-covered
// additions, and a brand-new untested file (proving subsumption of file-level).
func TestRunCoverageCheck_DiffLevel(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go toolchain not available: %v", err)
	}

	mathBase := "package pkg\n\nfunc One() int { return 1 }\n"
	mathTestBase := "package pkg\n\nimport \"testing\"\n\nfunc TestOne(t *testing.T) { _ = One() }\n"

	type scenario struct {
		name        string
		feature     map[string]string
		wantFinding string // empty string means expect no finding
		wantCount   string // substring expected in description when wantFinding set
	}
	cases := []scenario{
		{
			name: "fully_covered_no_finding",
			feature: map[string]string{
				"pkg/math.go":      mathBase + "\nfunc Two() int { return 2 }\n",
				"pkg/math_test.go": mathTestBase + "\nfunc TestTwo(t *testing.T) { _ = Two() }\n",
			},
			wantFinding: "",
		},
		{
			name: "fully_uncovered_finding",
			feature: map[string]string{
				"pkg/math.go": mathBase + "\nfunc Two() int { return 2 }\n",
				// math_test.go intentionally unchanged: Two has no test.
			},
			wantFinding: "pkg/math.go",
			wantCount:   "changed line(s)",
		},
		{
			name: "partially_covered_finding",
			feature: map[string]string{
				"pkg/math.go":      mathBase + "\nfunc Two() int { return 2 }\n\nfunc Three() int { return 3 }\n",
				"pkg/math_test.go": mathTestBase + "\nfunc TestTwo(t *testing.T) { _ = Two() }\n",
				// Three is added but untested.
			},
			wantFinding: "pkg/math.go",
			wantCount:   "changed line(s)",
		},
		{
			name: "brand_new_untested_file_finding",
			feature: map[string]string{
				"pkg/extra.go": "package pkg\n\nfunc Extra() int { return 99 }\n",
			},
			wantFinding: "pkg/extra.go",
			wantCount:   "changed line(s)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			main := map[string]string{
				"pkg/math.go":      mathBase,
				"pkg/math_test.go": mathTestBase,
			}
			dir, baseSHA, headSHA := setupDiffCoverageRepo(t, main, tc.feature)
			ag := &mockAgent{name: "test"}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "go test ./..."})

			got, _, err := runCoverageCheck(sctx, baseSHA)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantFinding == "" {
				for _, f := range got {
					if strings.HasPrefix(f.ID, uncoveredChangedLinesIDPrefix) {
						t.Errorf("expected no coverage finding, got %+v", f)
					}
				}
				return
			}
			var match *Finding
			for i := range got {
				if got[i].File == tc.wantFinding && strings.HasPrefix(got[i].ID, uncoveredChangedLinesIDPrefix) {
					match = &got[i]
					break
				}
			}
			if match == nil {
				t.Fatalf("expected a %s finding for %s, got %+v", uncoveredChangedLinesIDPrefix, tc.wantFinding, got)
			}
			if match.Severity != "warning" || match.Action != "ask-user" {
				t.Errorf("unexpected finding shape: %+v", match)
			}
			if !strings.Contains(match.Description, tc.wantCount) {
				t.Errorf("description %q should contain %q", match.Description, tc.wantCount)
			}
		})
	}
}

// setupDiffCoverageRepo builds a Go module git repo: a main branch seeded with
// mainFiles, then a feature branch whose tree is extended/overwritten by
// featureFiles (relative path → full contents), committed as one commit. The
// diff between main and feature is therefore exactly the featureFiles changes.
// Returns repo dir, base (main) SHA, and head (feature) SHA; HEAD stays on
// feature.
func setupDiffCoverageRepo(t *testing.T, mainFiles, featureFiles map[string]string) (string, string, string) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go toolchain not available: %v", err)
	}
	dir := t.TempDir()
	run := func(args ...string) string { return gitCmd(t, dir, args...) }
	writeRel := func(rel, content string) { mustWrite(t, filepath.Join(dir, rel), content) }

	run("init", "--initial-branch=main")
	writeRel("go.mod", "module example.com/test\n\ngo 1.21\n")
	for rel, content := range mainFiles {
		writeRel(rel, content)
	}
	run("add", "-A")
	run("commit", "-m", "base")
	baseSHA := run("rev-parse", "HEAD")

	run("checkout", "-b", "feature")
	for rel, content := range featureFiles {
		writeRel(rel, content)
	}
	run("add", "-A")
	run("commit", "-m", "feature")
	headSHA := run("rev-parse", "HEAD")
	return dir, baseSHA, headSHA
}

// setupGoModuleRepo creates a git repo containing a Go module whose main branch
// carries a covered source file plus its test, and a feature branch that adds an
// uncovered source file. Leaves HEAD on the feature branch.
// pkgDir controls the package directory name (e.g. "pkg").
func setupGoModuleRepo(t *testing.T, pkgDir string) (string, string, string) {
	t.Helper()
	dir := t.TempDir()

	run := func(args ...string) string { return gitCmd(t, dir, args...) }
	writeRel := func(rel, content string) {
		mustWrite(t, filepath.Join(dir, rel), content)
	}

	run("init", "--initial-branch=main")

	writeRel("go.mod", "module example.com/test\n\ngo 1.21\n")
	writeRel(filepath.Join(pkgDir, "covered.go"), "package pkg\n\n// Covered is exercised by a test.\nfunc Covered() int { return 42 }\n")
	writeRel(filepath.Join(pkgDir, "covered_test.go"), "package pkg\n\nimport \"testing\"\n\nfunc TestCovered(t *testing.T) {\n\tif Covered() != 42 {\n\t\tt.Fatal(\"wrong\")\n\t}\n}\n")

	run("add", "-A")
	run("commit", "-m", "base: covered package")
	baseSHA := run("rev-parse", "HEAD")

	run("checkout", "-b", "feature")
	writeRel(filepath.Join(pkgDir, "uncovered.go"), "package pkg\n\n// Uncovered has no test exercising it.\nfunc Uncovered() int { return 7 }\n")
	run("add", "-A")
	run("commit", "-m", "feature: add uncovered file")
	headSHA := run("rev-parse", "HEAD")

	return dir, baseSHA, headSHA
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func contains(slice []string, want string) bool {
	for _, s := range slice {
		if s == want {
			return true
		}
	}
	return false
}

// jsonPath converts an OS-native path to a forward-slash spelling safe for
// embedding in a JSON string literal. On Windows, filepath.Join produces
// backslash-separated paths whose raw \ would form invalid JSON escape
// sequences (\U, \S, etc.); converting to / avoids that. toRepoRelPOSIX
// handles both separators via filepath.Clean, so the same code path is
// exercised.
func jsonPath(p string) string {
	return filepath.ToSlash(p)
}
