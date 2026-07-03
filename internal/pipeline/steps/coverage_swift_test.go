package steps

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// TestSwiftProvider_Active verifies the dual gate: a Swift manifest alone is
// not enough — NM_SWIFT_SSH_HOST must also be set or the provider stays OFF
// (no Mac ⇒ no Swift toolchain locally).
func TestSwiftProvider_Active(t *testing.T) {
	t.Parallel()
	swiftPM := t.TempDir()
	mustWrite(t, filepath.Join(swiftPM, "Package.swift"), "// swift-tools-version:5.9\n")
	xcodeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(xcodeDir, "App.xcodeproj"), 0o755); err != nil {
		t.Fatal(err)
	}
	neither := t.TempDir()

	tests := []struct {
		name    string
		workDir string
		env     []string
		want    bool
	}{
		{"swiftpm + host", swiftPM, []string{"NM_SWIFT_SSH_HOST=mick@100.88.119.2"}, true},
		{"xcode + host", xcodeDir, []string{"NM_SWIFT_SSH_HOST=mick@100.88.119.2"}, true},
		{"swiftpm without host", swiftPM, nil, false},
		{"xcode without host", xcodeDir, nil, false},
		{"no manifest + host", neither, []string{"NM_SWIFT_SSH_HOST=mick@100.88.119.2"}, false},
		{"empty host string", swiftPM, []string{"NM_SWIFT_SSH_HOST=  "}, false},
	}
	p := swiftCoverageProvider{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := p.Active(tc.workDir, tc.env); got != tc.want {
				t.Errorf("Active = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSwiftProvider_CoverableChangedFiles checks the .swift + non-test + not-
// ignored + exists-on-disk filter, mirroring the Go provider's contract.
func TestSwiftProvider_CoverableChangedFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "Sources/App/foo.swift"), "struct Foo {}\n")
	mustWrite(t, filepath.Join(dir, "Sources/App/bar.swift"), "struct Bar {}\n")
	mustWrite(t, filepath.Join(dir, "Tests/AppTests/AppTests.swift"), "import XCTest\n")
	mustWrite(t, filepath.Join(dir, "Sources/App/FooTests.swift"), "struct FooTests {}\n")
	mustWrite(t, filepath.Join(dir, "gen.swift"), "struct Gen {}\n")
	mustWrite(t, filepath.Join(dir, "readme.md"), "readme\n")

	changed := []string{
		"Sources/App/foo.swift",         // coverable
		"Sources/App/bar.swift",         // coverable, will be ignored below
		"Tests/AppTests/AppTests.swift", // test (Tests/ segment): skipped
		"Sources/App/FooTests.swift",    // *Tests.swift basename: skipped
		"gen.swift",                     // coverable, will be ignored
		"readme.md",                     // non-Swift: skipped
		"deleted.swift",                 // not on disk: skipped
		"",                              // blank: skipped
	}
	p := swiftCoverageProvider{}
	got := p.CoverableChangedFiles(changed, dir, nil)
	if len(got) != 3 || !contains(got, "Sources/App/foo.swift") || !contains(got, "Sources/App/bar.swift") || !contains(got, "gen.swift") {
		t.Errorf("expected foo, bar, gen; got %v", got)
	}

	// Ignore patterns remove matching files.
	got = p.CoverableChangedFiles(changed, dir, []string{"gen.swift", "Sources/App/bar.swift"})
	if len(got) != 1 || got[0] != "Sources/App/foo.swift" {
		t.Errorf("expected only foo.swift after ignores, got %v", got)
	}
}

// TestIsSwiftTestFile exercises the three test-file conventions: Tests/
// segment (SwiftPM), *Tests.swift basename (Xcode), and *XCTest* name.
func TestIsSwiftTestFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path string
		want bool
	}{
		{"Tests/FooTests/FooTests.swift", true},
		{"Sources/Foo/Foo.swift", false},
		{"FooTests.swift", true},
		{"Foo.swift", false},
		{"XCTestCase.swift", true},
		{"App/Views/Root.swift", false},
		// path containing Tests/ segment but in deeper nesting
		{"sub/Tests/x.swift", true},
	}
	for _, tc := range tests {
		if got := isSwiftTestFile(tc.path); got != tc.want {
			t.Errorf("isSwiftTestFile(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// TestHasSwiftManifest covers Package.swift and the xcodeproj/xcworkspace
// directory checks, plus the negative case.
func TestHasSwiftManifest(t *testing.T) {
	t.Parallel()
	swiftPM := t.TempDir()
	mustWrite(t, filepath.Join(swiftPM, "Package.swift"), "// swift\n")
	if !hasSwiftManifest(swiftPM) {
		t.Errorf("Package.swift root should be detected")
	}

	xcproj := t.TempDir()
	if err := os.MkdirAll(filepath.Join(xcproj, "App.xcodeproj"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !hasSwiftManifest(xcproj) {
		t.Errorf(".xcodeproj root should be detected")
	}

	xcws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(xcws, "App.xcworkspace"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !hasSwiftManifest(xcws) {
		t.Errorf(".xcworkspace root should be detected")
	}

	plain := t.TempDir()
	mustWrite(t, filepath.Join(plain, "main.swift"), "print(1)\n")
	if hasSwiftManifest(plain) {
		t.Errorf("lone main.swift should not trigger manifest detection")
	}
}

// TestSplitSSHOpts verifies whitespace splitting with simple double-quote
// grouping, used to expand NM_SWIFT_SSH_OPTS into individual ssh flags.
func TestSplitSSHOpts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"-p 2222", []string{"-p", "2222"}},
		{"-i ~/.ssh/m1", []string{"-i", "~/.ssh/m1"}},
		{`-o "StrictHostKeyChecking=accept-new"`, []string{"-o", "StrictHostKeyChecking=accept-new"}},
		{"-p 2222 -i key", []string{"-p", "2222", "-i", "key"}},
	}
	for _, tc := range tests {
		got := splitSSHOpts(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("splitSSHOpts(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitSSHOpts(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

// TestParseLLVMCovBlocks verifies the SwiftPM parse map (report §4.2B):
// llvm-cov export JSON segments → coverBlock per hasCount segment spanning
// [seg.line, nextSeg.line-1]. Paths are relativized to workDir.
func TestParseLLVMCovBlocks(t *testing.T) {
	t.Parallel()
	work := t.TempDir()
	// Create source files so the paths can be resolved/relativized.
	mustWrite(t, filepath.Join(work, "Sources", "App", "foo.swift"), "struct Foo {}\n")
	mustWrite(t, filepath.Join(work, "Sources", "App", "bar.swift"), "struct Bar {}\n")

	fooAbs := filepath.Join(work, "Sources", "App", "foo.swift")
	barAbs := filepath.Join(work, "Sources", "App", "bar.swift")

	// llvm-cov segments are arrays: [line, col, count, hasCount, regionCnt].
	// Construct a file with two covered regions on different lines.
	raw := `{
  "data": [
    {
      "files": [
        {
          "filename": "` + jsonPath(fooAbs) + `",
          "segments": [
            [1, 0, 1, true, 0],
            [5, 0, 0, true, 0],
            [5, 10, 2, true, 0],
            [9, 0, 0, false, 0]
          ]
        },
        {
          "filename": "` + jsonPath(barAbs) + `",
          "segments": [
            [1, 0, 3, true, 0],
            [4, 0, 0, false, 0]
          ]
        }
      ]
    }
  ]
}`
	blocks := parseLLVMCovBlocks(raw, work)

	foo := blocks["Sources/App/foo.swift"]
	if foo == nil {
		t.Fatalf("expected blocks for Sources/App/foo.swift, got %v", blocks)
	}
	// Segments (sorted by line): [1,*,1,true], [5,*,0,true], [5,*,2,true], [9,*,0,false]
	// Per the parse map, a hasCount segment spans [seg.line, nextSeg.line-1]
	// (the next segment is the immediately-following one in sorted order,
	// regardless of its own hasCount). So:
	//   line1 cnt1 → [1, 5-1=4]
	//   line5 cnt0 → [5, 5-1=4] collapses to [5,5]
	//   line5 cnt2 → [5, 9-1=8]
	//   line9 (hasCount=false) → skipped
	wantFoo := []coverBlock{
		{startLine: 1, endLine: 4, count: 1},
		{startLine: 5, endLine: 5, count: 0},
		{startLine: 5, endLine: 8, count: 2},
	}
	if len(foo) != len(wantFoo) {
		t.Fatalf("expected %d foo.swift blocks, got %d: %+v", len(wantFoo), len(foo), foo)
	}
	for i, w := range wantFoo {
		if foo[i] != w {
			t.Errorf("foo.swift block[%d] = %+v, want %+v", i, foo[i], w)
		}
		if foo[i].endLine < foo[i].startLine {
			t.Errorf("block %+v has endLine < startLine", foo[i])
		}
	}

	bar := blocks["Sources/App/bar.swift"]
	if bar == nil {
		t.Fatalf("expected blocks for Sources/App/bar.swift, got %v", blocks)
	}
	// Segments: [1,*,3,true], [4,*,0,false]. The count=3 segment spans
	// [1, 4-1=3]; the hasCount=false segment is skipped.
	if len(bar) != 1 || bar[0].startLine != 1 || bar[0].endLine != 3 || bar[0].count != 3 {
		t.Errorf("bar.swift expected single block [1,3] count=3, got %+v", bar)
	}
}

// TestParseLLVMCovBlocks_NonRepoPathDropped verifies that a source path not
// under workDir is still emitted (as an absolute POSIX path) — the dispatcher's
// intersection with coverable naturally filters it. This documents the
// "outside-workDir" branch of toRepoRelPOSIX rather than asserting silence.
func TestParseLLVMCovBlocks_NonRepoPath(t *testing.T) {
	t.Parallel()
	work := t.TempDir()
	raw := `{"data":[{"files":[
		{"filename":"/other/place/x.swift","segments":[[1,0,1,true,0]]}
	]}]}`
	blocks := parseLLVMCovBlocks(raw, work)
	if _, ok := blocks["/other/place/x.swift"]; !ok {
		t.Errorf("expected absolute-path key for non-workDir file, got %v", blocks)
	}
}

// TestParseXccovBlocks verifies the xcode parse map (report §4.2A): each
// coveredLines entry → 1-line count=1 block; each uncoveredLines entry →
// 1-line count=0 block. Paths are relativized to workDir.
func TestParseXccovBlocks(t *testing.T) {
	t.Parallel()
	work := t.TempDir()
	mustWrite(t, filepath.Join(work, "Sources", "App", "foo.swift"), "struct Foo {}\n")
	mustWrite(t, filepath.Join(work, "Sources", "App", "bar.swift"), "struct Bar {}\n")

	fooAbs := filepath.Join(work, "Sources", "App", "foo.swift")
	barAbs := filepath.Join(work, "Sources", "App", "bar.swift")

	// Two ===FILE records, each followed by a JSON object on its own line.
	raw := xccovFileMarker + fooAbs + "\n" +
		`{"coveredLines":[7,8,12,13],"uncoveredLines":[9,10,11]}` + "\n" +
		xccovFileMarker + barAbs + "\n" +
		`{"coveredLines":[1,2],"uncoveredLines":[]}` + "\n"

	blocks := parseXccovBlocks(raw, work)

	foo := blocks["Sources/App/foo.swift"]
	if foo == nil {
		t.Fatalf("expected blocks for Sources/App/foo.swift, got %v", blocks)
	}
	wantFoo := map[int]float64{7: 1, 8: 1, 12: 1, 13: 1, 9: 0, 10: 0, 11: 0}
	gotFoo := map[int]float64{}
	for _, b := range foo {
		if b.startLine != b.endLine {
			t.Errorf("xccov block should be 1-line, got %+v", b)
		}
		gotFoo[b.startLine] = b.count
	}
	for ln, want := range wantFoo {
		if gotFoo[ln] != want {
			t.Errorf("foo.swift line %d count = %v, want %v", ln, gotFoo[ln], want)
		}
	}

	bar := blocks["Sources/App/bar.swift"]
	if len(bar) != 2 {
		t.Fatalf("expected 2 covered blocks for bar.swift, got %+v", bar)
	}
	for _, b := range bar {
		if b.count != 1 {
			t.Errorf("bar.swift covered block should be count=1, got %+v", b)
		}
	}
}

// TestParseXccovBlocks_TrailingNoise confirms findJSONEnd truncates a record
// body to its first JSON object when the xccov tool emits trailing data on
// the same line.
func TestParseXccovBlocks_TrailingNoise(t *testing.T) {
	t.Parallel()
	work := t.TempDir()
	mustWrite(t, filepath.Join(work, "f.swift"), "struct F {}\n")
	raw := xccovFileMarker + filepath.Join(work, "f.swift") + "\n" +
		`{"coveredLines":[1],"uncoveredLines":[]} trailing garbage` + "\n"
	blocks := parseXccovBlocks(raw, work)
	if b := blocks["f.swift"]; len(b) != 1 || b[0].startLine != 1 || b[0].count != 1 {
		t.Errorf("expected single covered block on line 1, got %+v", b)
	}
}

// TestParseBlocks_Dispatch verifies the parser-selection logic: raw containing
// the xccov marker routes to the xccov parser; otherwise llvm-cov. Also
// confirms empty input returns an empty (non-nil) map.
func TestParseBlocks_Dispatch(t *testing.T) {
	t.Parallel()
	p := swiftCoverageProvider{}
	work := t.TempDir()
	mustWrite(t, filepath.Join(work, "f.swift"), "struct F {}\n")

	if got := p.ParseBlocks("", work); len(got) != 0 {
		t.Errorf("ParseBlocks('') = %v, want empty", got)
	}

	llvmRaw := `{"data":[{"files":[{"filename":"` + jsonPath(filepath.Join(work, "f.swift")) + `","segments":[[1,0,1,true,0]]}]}]}`
	got := p.ParseBlocks(llvmRaw, work)
	if _, ok := got["f.swift"]; !ok {
		t.Errorf("llvm-cov dispatch missed f.swift, got %v", got)
	}

	xccovRaw := xccovFileMarker + filepath.Join(work, "f.swift") + "\n" + `{"coveredLines":[1],"uncoveredLines":[]}` + "\n"
	got = p.ParseBlocks(xccovRaw, work)
	if _, ok := got["f.swift"]; !ok {
		t.Errorf("xccov dispatch missed f.swift, got %v", got)
	}
}

// TestBuildSwiftScript_SwiftPM checks the generated remote script contains the
// key invariants: login-shell-safe invocation, dirty-tree guard, head SHA
// checkout, profdata resolution, and llvm-cov export. It does not run the
// script — that path is exercised live over SSH (gated behind
// NM_SWIFT_SSH_HOST).
func TestBuildSwiftScript_SwiftPM(t *testing.T) {
	t.Parallel()
	p := swiftCoverageProvider{}
	script := p.buildSwiftScript("/Users/mick/src/app", "abc123", "swiftpm", nil)
	mustContain := []string{
		"set -euo pipefail",
		"REMOTE_PATH=\"/Users/mick/src/app\"",
		"HEAD_SHA=\"abc123\"",
		"trap cleanup EXIT",
		"git diff --quiet",
		"git checkout \"$HEAD_SHA\"",
		"swift test --enable-code-coverage",
		"swift test --show-code-coverage-path",
		// Bug #2 fix: cat the JSON export that --show-code-coverage-path names
		// (the old `xcrun llvm-cov export --format=json` was doubly wrong).
		"cat \"$PROF\"",
		"/tmp/nm-dd-",
		"/tmp/nm-r-",
	}
	for _, sub := range mustContain {
		if !strings.Contains(script, sub) {
			t.Errorf("script missing %q\n--- script ---\n%s", sub, script)
		}
	}
}

// TestBuildSwiftScript_Xcode checks the Xcode path includes the Xcode.app
// presence guard, the xcodebuild invocation with scheme/project, and the
// per-file xccov view loop.
func TestBuildSwiftScript_Xcode(t *testing.T) {
	t.Parallel()
	p := swiftCoverageProvider{}
	env := []string{
		"NM_SWIFT_SCHEME=cmux",
		"NM_SWIFT_PROJECT=cmux.xcodeproj",
	}
	script := p.buildSwiftScript("/Users/mick/src/cmux", "deadbeef", "xcode", env)
	mustContain := []string{
		// Bug #5 fix: gate via xcodebuild -version (respects xcode-select,
		// works for versioned installs like /Applications/Xcode-26.5.0.app).
		"xcodebuild -version",
		"xcodebuild test -enableCodeCoverage",
		"-scheme \"cmux\"",
		"-project \"cmux.xcodeproj\"",
		"xcrun xccov view --report --json",
		"xcrun xccov view --file",
		"-resultBundlePath \"$RB\"",
	}
	for _, sub := range mustContain {
		if !strings.Contains(script, sub) {
			t.Errorf("xcode script missing %q\n--- script ---\n%s", sub, script)
		}
	}
}

// TestRunSwiftCoverageSSH_MissingHost verifies the SSH helper fails cleanly
// when ssh itself can't connect (no such host). The Mac SSH path is exercised
// only by the optional live e2e test below; this confirms the local-side
// plumbing returns a useful error rather than panicking.
func TestRunSwiftCoverageSSH_MissingHost(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Use an unroutable host with a short ssh timeout via -o ConnectTimeout.
	_, err := runSwiftCoverageSSH(ctx, "swift-coverage-nonexistent-host.invalid", []string{"-o", "ConnectTimeout=3", "-o", "BatchMode=yes"}, "echo hi\n")
	if err == nil {
		t.Skipf("ssh unexpectedly succeeded to a bogus host; skipping")
	}
}

// TestSwiftProvider_Name is a tiny guard so a future rename is caught loudly
// (finding IDs are namespaced by this string).
func TestSwiftProvider_Name(t *testing.T) {
	t.Parallel()
	if got := (swiftCoverageProvider{}).Name(); got != "swift" {
		t.Errorf("Name = %q, want \"swift\"", got)
	}
}

// TestSwiftProvider_ParseBlocks_FeedsCore is the integration check that the
// provider's output is consumable by the shared, language-neutral
// uncoveredChangedLineFindings core. It builds a tiny coverable/added/blocks
// triple as the dispatcher would and asserts the expected finding fires —
// proving the path-key invariant (parser keys == diff keys) holds end-to-end.
func TestSwiftProvider_ParseBlocks_FeedsCore(t *testing.T) {
	t.Parallel()
	work := t.TempDir()
	rel := "Sources/App/foo.swift"
	mustWrite(t, filepath.Join(work, rel), "struct Foo {}\n")

	// Segments (sorted by line):
	//   [1,*,1,true]  → span [1, 5-1=4]   (covered)
	//   [5,*,0,true]  → span [5, 9-1=8]   (uncovered executable)
	//   [9,*,0,false] → skipped
	// Added lines [4,6]: line 4 is covered (in [1,4] count=1); lines 5 and 6
	// are uncovered executable (in [5,8] count=0). Expect a finding reporting
	// 2 uncovered changed lines.
	raw := `{"data":[{"files":[{"filename":"` + jsonPath(filepath.Join(work, rel)) + `","segments":[
		[1,0,1,true,0],
		[5,0,0,true,0],
		[9,0,0,false,0]
	]}]}]}`
	blocks := (swiftCoverageProvider{}).ParseBlocks(raw, work)

	coverable := []string{rel}
	added := map[string][]addedLineRange{rel: {{4, 6}}}
	findings := uncoveredChangedLineFindings(coverable, added, blocks)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(findings), findings)
	}
	if findings[0].File != rel {
		t.Errorf("finding file = %q, want %q", findings[0].File, rel)
	}
	if !strings.Contains(findings[0].Description, "2 changed line(s)") {
		t.Errorf("description %q should report 2 uncovered lines", findings[0].Description)
	}
}

// TestBuildSwiftScript_DirtyTreeGuardClosesWithFi is a regression guard for
// bug #1: the dirty-tree `if` block must close with `fi`, not `}`. A `}` closer
// is a fatal bash syntax error — the remote script aborts at parse time before
// any line executes, and the EXIT trap masks the non-zero status (bug #3), so
// SSH exits 0 with empty stdout and the dispatcher sees a silent no-op.
func TestBuildSwiftScript_DirtyTreeGuardClosesWithFi(t *testing.T) {
	t.Parallel()
	p := swiftCoverageProvider{}
	script := p.buildSwiftScript("/Users/mick/src/app", "abc123", "swiftpm", nil)
	dirty := "refusing to checkout (commit or stash on the Mac)' >&2\n  exit 1\n"
	idx := strings.Index(script, dirty)
	if idx < 0 {
		t.Fatalf("dirty-tree guard not found in script:\n%s", script)
	}
	closer := script[idx+len(dirty):]
	if !strings.HasPrefix(closer, "fi\n") {
		t.Errorf("dirty-tree guard must close with \"fi\" not \"}\" (bug #1):\n%s", script)
	}
}

// TestSwiftRunCoverage_PathBridgeRewritesMacPaths is the highest-value
// regression test (bug #4). The Swift provider delegates coverage collection to
// a remote Mac, so the JSON it gets back carries Mac-relative source paths. But
// ParseBlocks receives the LOCAL VPS workDir, so toRepoRelPOSIX cannot strip
// the prefix — block keys become absolute Mac paths that miss the
// repo-relative coverable/added lookup, and the empty-blocks fallback fires a
// false positive ("all changed lines uncovered"). The RunCoverage fix bridges
// the two by rewriting remotePath → sctx.WorkDir in the raw JSON before
// returning. This test simulates that bridge and asserts ParseBlocks then keys
// by the LOCAL repo-relative path, not the absolute Mac path.
func TestSwiftRunCoverage_PathBridgeRewritesMacPaths(t *testing.T) {
	t.Parallel()
	localWorkDir := t.TempDir()
	rel := filepath.Join("Sources", "swift-cov-fixture", "Calculator.swift")
	mustWrite(t, filepath.Join(localWorkDir, rel), "struct Calculator {}\n")
	remotePath := "/Users/mick/src/swift-cov-fixture"

	// Raw JSON as the Mac emits it: source paths anchored at remotePath.
	macAbs := remotePath + "/" + filepath.ToSlash(rel)
	macRaw := `{"data":[{"files":[{"filename":"` + macAbs + `","segments":[
		[4,0,1,true,0],
		[8,0,0,true,0],
		[9,0,0,false,0]
	]}]}]}`

	// WITHOUT the bridge: ParseBlocks cannot relativize Mac paths to the local
	// workDir, so the block key is the raw absolute Mac path (the false positive).
	unbridged := (swiftCoverageProvider{}).ParseBlocks(macRaw, localWorkDir)
	if _, ok := unbridged[filepath.ToSlash(rel)]; ok {
		t.Fatalf("unbridged parse should NOT key by local rel (expected absolute Mac path); got %v", unbridged)
	}

	// Bug #4 fix: RunCoverage rewrites remotePath → local workDir in raw. The
	// workDir is slash-ified because remotePath sits inside a JSON string
	// literal; a native Windows workDir (C:\Users\...) would inject backslashes
	// that are invalid JSON escapes, corrupting the document.
	bridged := strings.ReplaceAll(macRaw, remotePath, filepath.ToSlash(localWorkDir))

	// ParseBlocks with the LOCAL workDir must now key by repo-relative path.
	blocks := (swiftCoverageProvider{}).ParseBlocks(bridged, localWorkDir)

	wantKey := filepath.ToSlash(rel)
	bs, ok := blocks[wantKey]
	if !ok {
		var keys []string
		for k := range blocks {
			keys = append(keys, k)
		}
		t.Fatalf("path bridge failed: want key %q, got keys %v", wantKey, keys)
	}
	// `unused` func (line 8) should land in a count=0 block (uncovered).
	var line8Count float64 = -1
	for _, b := range bs {
		if b.startLine <= 8 && b.endLine >= 8 {
			line8Count = b.count
		}
	}
	if line8Count != 0 {
		t.Errorf("line 8 (unused func) should be count=0 (uncovered), got %v in %+v", line8Count, bs)
	}
}

// stubSwiftCoverageProvider wraps swiftCoverageProvider to stub out the SSH
// transport (Active + RunCoverage) while inheriting Name, CoverableChangedFiles,
// and ParseBlocks from the real provider. Used by the dispatcher e2e test to
// feed a recorded coverage JSON through the real parse→core→namespace path
// without a live Mac.
type stubSwiftCoverageProvider struct {
	swiftCoverageProvider
	raw string
}

func (stubSwiftCoverageProvider) Active(string, []string) bool { return true }
func (s stubSwiftCoverageProvider) RunCoverage(*pipeline.StepContext) (string, string, error) {
	return s.raw, "stub swift coverage (recorded JSON)", nil
}

// TestRunCoverageCheck_SwiftDispatcherEndToEnd drives the full
// runCoverageCheck dispatcher against a real on-disk git repo with a stub
// coverage provider returning a recorded JSON. It asserts BOTH the finding ID
// (namespaced per language) and the N changed-line count match ground truth:
// the count must be the actually-uncovered executable lines (2), not all added
// lines (6) — the latter is the false positive that bugs #1–#4 produced before
// the fix.
func TestRunCoverageCheck_SwiftDispatcherEndToEnd(t *testing.T) {
	// NOT parallel: swaps the package-level coverageProviders registry.
	workDir := t.TempDir()
	run := func(args ...string) string { return gitCmd(t, workDir, args...) }

	run("init", "--initial-branch=main")
	mustWrite(t, filepath.Join(workDir, "README"), "base\n")
	run("add", "-A")
	run("commit", "-m", "base")
	baseSHA := run("rev-parse", "HEAD")

	// Head: a new Swift source file. All 6 lines are added (brand-new file).
	run("checkout", "-b", "feature")
	rel := filepath.Join("Sources", "App", "calc.swift")
	mustWrite(t, filepath.Join(workDir, rel),
		"struct Calc {\n"+
			"    func a() { return 1 }\n"+
			"    func b() { return 2 }\n"+
			"    func unc1() { return 3 }\n"+
			"    func unc2() { return 4 }\n"+
			"}\n")
	run("add", "-A")
	run("commit", "-m", "head: add calc.swift")
	headSHA := run("rev-parse", "HEAD")

	// Recorded coverage JSON (post-bridge, local paths). Segments:
	//   [2,*,1,true] → [2,2] count=1   (func a, covered)
	//   [3,*,1,true] → [3,3] count=1   (func b, covered)
	//   [4,*,0,true] → [4,5] count=0   (unc1+unc2, uncovered)
	//   [6,*,0,false] → skipped
	// Added lines 1–6: lines 4,5 are the only uncovered executable ones → count 2.
	calcAbs := filepath.Join(workDir, rel)
	canned := `{"data":[{"files":[{"filename":"` + jsonPath(calcAbs) + `","segments":[
		[2,0,1,true,0],
		[3,0,1,true,0],
		[4,0,0,true,0],
		[6,0,0,false,0]
	]}]}]}`

	sctx := &pipeline.StepContext{
		Ctx:     context.Background(),
		Run:     &db.Run{HeadSHA: headSHA},
		WorkDir: workDir,
		Env:     []string{"NO_MISTAKES_COVERAGE_CHECK=1"},
		Config:  &config.Config{},
		Log:     func(string) {},
	}

	stub := stubSwiftCoverageProvider{raw: canned}
	saved := coverageProviders
	coverageProviders = []coverageProvider{stub}
	defer func() { coverageProviders = saved }()

	findings, _, err := runCoverageCheck(sctx, baseSHA)
	if err != nil {
		t.Fatalf("runCoverageCheck: %v", err)
	}

	wantID := "uncovered-changed-lines:swift:" + filepath.ToSlash(rel)
	var got *Finding
	for i := range findings {
		if findings[i].ID == wantID {
			got = &findings[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("expected finding %q, got %+v", wantID, findings)
	}
	if !strings.Contains(got.Description, "2 changed line(s)") {
		t.Errorf("description = %q; want \"2 changed line(s)\" (actually-uncovered executable lines, not all 6 added)", got.Description)
	}
	if strings.Contains(got.Description, "6 changed line(s)") {
		t.Errorf("description = %q; the false-positive \"6\" (all added lines) means the path bridge or parse is broken", got.Description)
	}
}
