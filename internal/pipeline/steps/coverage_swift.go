package steps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// Swift coverage provider environment knobs. All optional except
// NM_SWIFT_SSH_HOST, which gates the provider off entirely when unset (the Mac
// build executor is required — no-mistakes runs on Linux and has no native
// Swift toolchain). See AGENTS.md §Test Step for the full model.
const (
	// nmSwiftSSHHostEnv: SSH target for the Mac build executor (e.g.
	// "mick@100.88.119.2"). Empty ⇒ provider inactive.
	nmSwiftSSHHostEnv = "NM_SWIFT_SSH_HOST"
	// nmSwiftRemotePathEnv: absolute project root on the Mac.
	nmSwiftRemotePathEnv = "NM_SWIFT_REMOTE_PATH"
	// nmSwiftBuildModeEnv: "swiftpm" (default, works with CLT-only Macs) or
	// "xcode" (requires full Xcode.app, used for .xcodeproj apps).
	nmSwiftBuildModeEnv = "NM_SWIFT_BUILD_MODE"
	// nmSwiftSchemeEnv: Xcode scheme (xcode mode only).
	nmSwiftSchemeEnv = "NM_SWIFT_SCHEME"
	// nmSwiftProjectEnv: optional .xcodeproj/.xcworkspace path (xcode mode).
	nmSwiftProjectEnv = "NM_SWIFT_PROJECT"
	// nmSwiftSSHOptsEnv: extra ssh flags (port, identity file, accept-new...).
	nmSwiftSSHOptsEnv = "NM_SWIFT_SSH_OPTS"
)

// swiftCoverageProvider is the coverageProvider for Swift projects. It is
// active when a Package.swift or .xcodeproj/.xcworkspace sits at the worktree
// root AND NM_SWIFT_SSH_HOST names the Mac build executor — Swift has no native
// toolchain on this Linux VPS, so coverage collection is delegated to the Mac
// over SSH (a dumb executor: we sync the head SHA, build, and stream the
// native coverage JSON back over stdout; parsing is local Go).
//
// Two build modes share one file:
//   - swiftpm (default, works TODAY with CLT only): `swift test
//     --enable-code-coverage` then `xcrun llvm-cov export <test-binary>
//     -instr-profile=<profdata> --format=json`. Covers Package.swift projects.
//   - xcode (gated on full Xcode.app, used for .xcodeproj apps like cmux):
//     `xcodebuild test -enableCodeCoverage ...` then `xcrun xccov view --file
//     <path> --json <resultbundle>` per source file. Errors clearly until
//     Xcode is installed.
//
// It self-registers in init() so the dispatcher picks it up with no edits to
// shared code.
type swiftCoverageProvider struct{}

func init() {
	registerCoverageProvider(swiftCoverageProvider{})
}

func (swiftCoverageProvider) Name() string { return "swift" }

// Active reports whether the worktree is a Swift project we can build. The
// Swift provider is OFF unless BOTH (a) a Swift manifest is present at the
// worktree root (Package.swift for SwiftPM, or a *.xcodeproj/*.xcworkspace for
// Xcode apps) AND (b) NM_SWIFT_SSH_HOST names the Mac build executor. Without
// the Mac there is no Swift toolchain locally, so detection alone is not
// enough — the executor must be configured too.
func (swiftCoverageProvider) Active(workDir string, env []string) bool {
	host, _ := lookupStepEnv(env, nmSwiftSSHHostEnv)
	if strings.TrimSpace(host) == "" {
		return false
	}
	return hasSwiftManifest(workDir)
}

// hasSwiftManifest reports whether the worktree root carries a SwiftPM
// Package.swift or an Xcode project/workspace. Xcode bundles are directories;
// their presence at the root (not nested) is the standard project marker.
func hasSwiftManifest(workDir string) bool {
	if fileExists(filepath.Join(workDir, "Package.swift")) {
		return true
	}
	entries, err := os.ReadDir(workDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".xcodeproj") || strings.HasSuffix(name, ".xcworkspace") {
			return true
		}
	}
	return false
}

// CoverableChangedFiles filters the changed-file list to accountable Swift
// source files: .swift files that are not test files (Tests/ directory,
// *Tests.swift basenames, or *XCTest* names), do not match any ignore
// pattern, and still exist on disk (so pure deletions are not flagged).
func (swiftCoverageProvider) CoverableChangedFiles(changed []string, workDir string, ignorePatterns []string) []string {
	var out []string
	for _, path := range changed {
		path = strings.TrimSpace(path)
		if path == "" || !strings.HasSuffix(path, ".swift") {
			continue
		}
		if isSwiftTestFile(path) {
			continue
		}
		ignored := false
		for _, pattern := range ignorePatterns {
			if matchIgnorePattern(path, pattern) {
				ignored = true
				break
			}
		}
		if ignored {
			continue
		}
		if !fileExists(filepath.Join(workDir, path)) {
			continue
		}
		out = append(out, path)
	}
	return out
}

// isSwiftTestFile identifies Swift test sources by path convention: a Tests/
// path segment (SwiftPM: Tests/FooTests/FooTests.swift), a basename ending in
// Tests.swift (Xcode: FooTests.swift), or a basename mentioning XCTest. These
// are the standard XCTest layouts; non-test Swift sources are kept.
func isSwiftTestFile(path string) bool {
	for _, seg := range strings.Split(path, string(filepath.Separator)) {
		if seg == "Tests" {
			return true
		}
	}
	base := filepath.Base(path)
	if strings.HasSuffix(base, "Tests.swift") {
		return true
	}
	if strings.Contains(base, "XCTest") {
		return true
	}
	return false
}

// RunCoverage delegates Swift coverage collection to the Mac over SSH. The
// Mac is treated as a dumb executor mirroring the PR head: we sync the head
// SHA (failing clearly if its tree is dirty — never hard-reset the user's
// tree), run the language-appropriate build+coverage tool there, and stream
// the native JSON back over stdout for local parsing. Cancellation of sctx.Ctx
// kills the remote build because the SSH client is started with the step's
// context. Errors degrade to a logged no-op in the dispatcher — never block.
func (p swiftCoverageProvider) RunCoverage(sctx *pipeline.StepContext) (string, string, error) {
	host, _ := lookupStepEnv(sctx.Env, nmSwiftSSHHostEnv)
	host = strings.TrimSpace(host)
	if host == "" {
		return "", "", fmt.Errorf("swift coverage: %s not set", nmSwiftSSHHostEnv)
	}
	remotePath, _ := lookupStepEnv(sctx.Env, nmSwiftRemotePathEnv)
	remotePath = strings.TrimSpace(remotePath)
	if remotePath == "" {
		return "", "", fmt.Errorf("swift coverage: %s not set", nmSwiftRemotePathEnv)
	}
	mode, _ := lookupStepEnv(sctx.Env, nmSwiftBuildModeEnv)
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = "swiftpm"
	}
	if mode != "swiftpm" && mode != "xcode" {
		return "", "", fmt.Errorf("swift coverage: unknown %s=%q (want swiftpm or xcode)", nmSwiftBuildModeEnv, mode)
	}
	headSHA := strings.TrimSpace(sctx.Run.HeadSHA)
	if headSHA == "" {
		return "", "", fmt.Errorf("swift coverage: no head SHA on run")
	}
	opts, _ := lookupStepEnv(sctx.Env, nmSwiftSSHOptsEnv)
	sshArgs := splitSSHOpts(opts)

	script := p.buildSwiftScript(remotePath, headSHA, mode, sctx.Env)
	testedCmd := fmt.Sprintf("ssh %s swift coverage (%s mode)", host, mode)
	sctx.Log("running coverage: " + testedCmd)

	raw, err := runSwiftCoverageSSH(sctx.Ctx, host, sshArgs, script)
	if err != nil {
		return "", testedCmd, err
	}
	// Bridge Mac-relative paths in the coverage JSON to the local workDir so
	// ParseBlocks' toRepoRelPOSIX can strip the prefix and key blocks by the
	// same repo-relative path `git diff --name-only` emits. Without this, the
	// block keys stay absolute Mac paths, miss the coverable/added lookup, and
	// the empty-blocks fallback fires a false positive on every changed file.
	raw = strings.ReplaceAll(raw, remotePath, sctx.WorkDir)
	return raw, testedCmd, nil
}

// buildSwiftScript returns the remote bash script (read via stdin by
// `ssh host bash -l`) that syncs the head SHA and emits native coverage JSON
// on stdout. The script is mode-specific. It runs under a login shell so the
// Mac's Homebrew/CLT tools (swift, xcrun, git) are on PATH — the spec calls
// for `ssh host 'bash -lc "..."'`; feeding the script to `bash -l` over stdin
// is the quoting-clean equivalent and handles multi-line scripts safely.
func (p swiftCoverageProvider) buildSwiftScript(remotePath, headSHA, mode string, env []string) string {
	scheme, _ := lookupStepEnv(env, nmSwiftSchemeEnv)
	project, _ := lookupStepEnv(env, nmSwiftProjectEnv)
	var b strings.Builder
	fmt.Fprintf(&b, "set -euo pipefail\n")
	fmt.Fprintf(&b, "REMOTE_PATH=%q\n", remotePath)
	fmt.Fprintf(&b, "HEAD_SHA=%q\n", headSHA)
	// Cleanup the per-run temp dirs (named with the SHA so concurrent runs do
	// not collide). EXIT trap ensures cleanup runs on both success and failure.
	fmt.Fprintf(&b, "cleanup() { rm -rf \"/tmp/nm-dd-$HEAD_SHA\" \"/tmp/nm-r-$HEAD_SHA.xcresult\" 2>/dev/null; }\n")
	fmt.Fprintf(&b, "trap cleanup EXIT\n")
	fmt.Fprintf(&b, "cd \"$REMOTE_PATH\" || { echo 'ERROR: remote path not found' >&2; exit 1; }\n")
	// Dirty-tree guard: refuse to checkout over local modifications — never
	// hard-reset the user's tree (the Mac is a build slave, not a scratchpad).
	fmt.Fprintf(&b, "if ! git diff --quiet || ! git diff --cached --quiet; then\n")
	fmt.Fprintf(&b, "  echo 'ERROR: remote worktree is dirty; refusing to checkout (commit or stash on the Mac)' >&2\n")
	fmt.Fprintf(&b, "  exit 1\n")
	fmt.Fprintf(&b, "fi\n")
	fmt.Fprintf(&b, "git fetch origin >/dev/null 2>&1 || true\n")
	// `git checkout` is the common path; the `-f` fallback only triggers if
	// the SHA is in a different state than HEAD expects (rare). Neither path
	// touches uncommitted changes because the dirty guard above already
	// refused a non-clean tree.
	fmt.Fprintf(&b, "git checkout \"$HEAD_SHA\" >/dev/null 2>&1 || git checkout -f \"$HEAD_SHA\" >/dev/null 2>&1\n")

	switch mode {
	case "xcode":
		// Xcode.app guard: error clearly so the path degrades gracefully
		// until Xcode is installed (vs. a cryptic xcodebuild failure). This
		// is the gate the spec calls out — Xcode is still installing on the
		// Mac, so this branch errors today and only becomes live later.
		fmt.Fprintf(&b, "if ! xcodebuild -version >/dev/null 2>&1; then\n")
		fmt.Fprintf(&b, "  echo 'ERROR: Xcode not installed / still installing; use swiftpm mode' >&2\n")
		fmt.Fprintf(&b, "  exit 1\n")
		fmt.Fprintf(&b, "fi\n")
		fmt.Fprintf(&b, "DD=\"/tmp/nm-dd-$HEAD_SHA\"\n")
		fmt.Fprintf(&b, "RB=\"/tmp/nm-r-$HEAD_SHA.xcresult\"\n")
		fmt.Fprintf(&b, "rm -rf \"$DD\" \"$RB\"\n")
		fmt.Fprintf(&b, "xcodebuild test -enableCodeCoverage -derivedDataPath \"$DD\" -resultBundlePath \"$RB\"")
		if strings.TrimSpace(scheme) != "" {
			fmt.Fprintf(&b, " -scheme %q", scheme)
		}
		if strings.TrimSpace(project) != "" {
			fmt.Fprintf(&b, " -project %q", project)
		}
		fmt.Fprintf(&b, " >/dev/null 2>&1\n")
		// Enumerate source files from the report and emit one ===FILE record
		// per file with its per-line coverage JSON. We cover ALL source files
		// (not just changed ones); the dispatcher's intersection with the
		// coverable changed-file list filters to what matters. python3 ships
		// with macOS CLT; if absent, the script errors clearly.
		fmt.Fprintf(&b, "FILES=$(xcrun xccov view --report --json \"$RB\" | python3 -c 'import json,sys\n")
		fmt.Fprintf(&b, "for t in json.load(sys.stdin).get(\"targets\",[]):\n")
		fmt.Fprintf(&b, "  for f in t.get(\"files\",[]):\n")
		fmt.Fprintf(&b, "    p=f.get(\"path\") or f.get(\"name\") or \"\"\n")
		fmt.Fprintf(&b, "    if p.endswith(\".swift\"): print(p)' 2>/dev/null)\n")
		fmt.Fprintf(&b, "if [ -z \"$FILES\" ]; then echo 'ERROR: no source files in xcresult (python3 missing?)' >&2; exit 1; fi\n")
		// Resolve each name to an absolute path under the project so the
		// parser can relativize it to repo space. `find` falls back to the
		// raw name when no match (basename-only), which the parser still
		// handles (the dispatcher's intersection with coverable filters it).
		fmt.Fprintf(&b, "for f in $FILES; do\n")
		fmt.Fprintf(&b, "  resolved=$(find \"$REMOTE_PATH\" -name \"$(basename \"$f\")\" -type f 2>/dev/null | head -1)\n")
		fmt.Fprintf(&b, "  printf '%%s%%s\\n' %q \"${resolved:-$f}\"\n", xccovFileMarker)
		fmt.Fprintf(&b, "  xcrun xccov view --file \"$f\" --json \"$RB\" 2>/dev/null || printf '{\"coveredLines\":[],\"uncoveredLines\":[]}\\n'\n")
		fmt.Fprintf(&b, "done\n")
	case "swiftpm":
		// SwiftPM: build + run tests with coverage instrumentation, resolve
		// the profdata path, pick a test binary, and export llvm-cov JSON.
		//
		// Note: this mode uses llvm-cov (shipped with CLT), but the test
		// EXECUTION step (swift test) still needs the XCTest or Swift Testing
		// module, which on macOS is bundled with Xcode.app — not CLT. So even
		// swiftpm mode effectively requires Xcode.app today. CLT-only Macs can
		// build Swift sources and export coverage from a profdata, but cannot
		// run an XCTest/Testing-based suite. This is the gap Xcode.app
		// installing in the background closes.
		fmt.Fprintf(&b, "swift test --enable-code-coverage >/dev/null 2>&1\n")
		fmt.Fprintf(&b, "PROF=$(swift test --show-code-coverage-path 2>/dev/null | tail -1 || true)\n")
		fmt.Fprintf(&b, "if [ -z \"$PROF\" ]; then echo 'ERROR: swift test --show-code-coverage-path empty' >&2; exit 1; fi\n")
		// Resolve the test binary. `swift build --show-bin-path` names the
		// build-products dir (platform-qualified, e.g.
		// .build/arm64-apple-macosx/debug); there is no `--test` variant.
		// Prefer <Pkg>PackageTests.xctest (macOS bundle) then
		// <Pkg>PackageTests (Linux), then any *Tests* executable under any
		// platform-qualified .build/*/debug as a fallback. The .xctest
		// extension is a directory on macOS — llvm-cov wants the mach-o
		// inside, so drill down via Contents/MacOS when present.
		fmt.Fprintf(&b, "BIN_PATH=$(swift build --show-bin-path 2>/dev/null || echo .build/debug)\n")
		fmt.Fprintf(&b, "TESTBIN=\"\"\n")
		fmt.Fprintf(&b, "for cand in \"$BIN_PATH\"/*PackageTests.xctest \"$BIN_PATH\"/*PackageTests \"$BIN_PATH\"/*Tests; do\n")
		fmt.Fprintf(&b, "  [ -e \"$cand\" ] && TESTBIN=\"$cand\" && break\n")
		fmt.Fprintf(&b, "done\n")
		fmt.Fprintf(&b, "if [ -z \"$TESTBIN\" ]; then\n")
		fmt.Fprintf(&b, "  for d in .build/*/debug .build/debug; do\n")
		fmt.Fprintf(&b, "    for cand in \"$d\"/*PackageTests.xctest \"$d\"/*PackageTests \"$d\"/*Tests*; do\n")
		fmt.Fprintf(&b, "      [ -e \"$cand\" ] && TESTBIN=\"$cand\" && break 2\n")
		fmt.Fprintf(&b, "    done\n")
		fmt.Fprintf(&b, "  done\n")
		fmt.Fprintf(&b, "fi\n")
		fmt.Fprintf(&b, "if [ -z \"$TESTBIN\" ]; then echo 'ERROR: could not resolve Swift test binary' >&2; exit 1; fi\n")
		// On macOS a .xctest is a bundle directory; llvm-cov wants the binary
		// inside it. Drill into Contents/MacOS/<basename without .xctest>.
		fmt.Fprintf(&b, "if [ -d \"$TESTBIN\" ]; then\n")
		fmt.Fprintf(&b, "  bn=$(basename \"$TESTBIN\" .xctest)\n")
		fmt.Fprintf(&b, "  for inner in \"$TESTBIN/Contents/MacOS/$bn\" \"$TESTBIN/Contents/MacOS/\"* \"$TESTBIN\"/*; do\n")
		fmt.Fprintf(&b, "    [ -f \"$inner\" ] && TESTBIN=\"$inner\" && break\n")
		fmt.Fprintf(&b, "  done\n")
		fmt.Fprintf(&b, "fi\n")
		// On modern SwiftPM (5.8+), `swift test --show-code-coverage-path`
		// already returns the final llvm-cov JSON export path, so cat-ing it
		// yields parse-ready input directly. The previous `xcrun llvm-cov
		// export` call was doubly wrong: --format=json is invalid on recent
		// Xcode toolchains (only text/html; JSON is the default) and
		// -instr-profile expects a .profdata, not the JSON export path.
		fmt.Fprintf(&b, "cat \"$PROF\"\n")
	}
	return b.String()
}

// runSwiftCoverageSSH runs `ssh <opts> host bash -l` and feeds script via
// stdin, returning stdout (the coverage JSON). Using `bash -l` over stdin (vs
// `bash -lc "..."`) avoids nested-quote escaping and still gives a login
// shell, so the Mac's Homebrew/CLT tools are on PATH. exec.CommandContext
// threads the step's context so cancellation kills the remote build.
//
// shellenv.ConfigureShellCommand + OutputShellCommand reap the whole process
// group on every exit path (clean exit, parse error, wait error, and context
// cancellation), not just the direct ssh child on cancellation. Without it, a
// helper the ssh client spawned (or a ControlMaster it left behind) would
// outlive the leader and accumulate across runs.
func runSwiftCoverageSSH(ctx context.Context, host string, opts []string, script string) (string, error) {
	args := make([]string, 0, len(opts)+2)
	args = append(args, opts...)
	args = append(args, host, "bash", "-l")
	cmd := exec.CommandContext(ctx, "ssh", args...)
	shellenv.ConfigureShellCommand(cmd)
	cmd.Stdin = strings.NewReader(script)
	// Separate stderr so remote error messages (the script's `>&2` lines)
	// surface in our error wrapper instead of polluting the JSON stream.
	// OutputShellCommand reaps via RunShellCommand, which (unlike cmd.Output)
	// does not populate (*exec.ExitError).Stderr, so we capture stderr into our
	// own buffer and read it back on failure.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := shellenv.OutputShellCommand(cmd)
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("swift coverage SSH: %w: %s", err, msg)
		}
		return "", fmt.Errorf("swift coverage SSH: %w", err)
	}
	return string(out), nil
}

// splitSSHOpts parses NM_SWIFT_SSH_OPTS (a single shell-style string) into
// individual ssh arguments. Whitespace-separated; respects simple double-quote
// grouping so values like `-o "StrictHostKeyChecking=accept-new"` survive
// intact. Empty/blank input yields nil.
func splitSSHOpts(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var args []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			inQuote = !inQuote
		case (c == ' ' || c == '\t' || c == '\n') && !inQuote:
			if cur.Len() > 0 {
				args = append(args, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		args = append(args, cur.String())
	}
	return args
}

// ParseBlocks parses Swift native coverage JSON into coverBlocks keyed by
// repo-relative POSIX path. The parser is selected by inspecting raw: xccov
// mode emits ===FILE:<path> records (parsed by parseXccovBlocks); otherwise
// raw is treated as llvm-cov export JSON (parsed by parseLLVMCovBlocks). Both
// parsers relativize paths to workDir via toRepoRelPOSIX so keys are
// byte-identical to `git diff --name-only` output (the path-key invariant
// every provider must honor).
func (swiftCoverageProvider) ParseBlocks(raw string, workDir string) map[string][]coverBlock {
	if strings.TrimSpace(raw) == "" {
		return map[string][]coverBlock{}
	}
	if strings.Contains(raw, xccovFileMarker) {
		return parseXccovBlocks(raw, workDir)
	}
	return parseLLVMCovBlocks(raw, workDir)
}

// xccovFileMarker prefixes each per-file record emitted by the xcode mode
// script body. Keeping it as a constant lets both the producer
// (buildSwiftScript) and the consumer (ParseBlocks) reference one spelling.
const xccovFileMarker = "===FILE:"

// parseXccovBlocks parses the concatenated `xcrun xccov view --file --json`
// output produced by the xcode mode script. Each record is introduced by
// `===FILE:<path>` on its own line and followed by a JSON object of shape
// {"coveredLines":[...],"uncoveredLines":[...]}. Each covered line becomes a
// 1-line count=1 block; each uncovered line a 1-line count=0 block.
func parseXccovBlocks(raw, workDir string) map[string][]coverBlock {
	out := make(map[string][]coverBlock)
	for _, record := range strings.Split(raw, xccovFileMarker) {
		record = strings.TrimSpace(record)
		if record == "" {
			continue
		}
		nl := strings.IndexByte(record, '\n')
		if nl < 0 {
			continue
		}
		path := strings.TrimSpace(record[:nl])
		body := strings.TrimSpace(record[nl+1:])
		if path == "" || body == "" {
			continue
		}
		// Truncate to the first JSON object on the line in case the xccov tool
		// appends trailing noise on the same record.
		if end := findJSONEnd(body); end > 0 {
			body = body[:end]
		}
		var ld xccovLineData
		if err := json.Unmarshal([]byte(body), &ld); err != nil {
			continue
		}
		rel := toRepoRelPOSIX(path, workDir)
		for _, ln := range ld.CoveredLines {
			out[rel] = append(out[rel], coverBlock{startLine: ln, endLine: ln, count: 1})
		}
		for _, ln := range ld.UncoveredLines {
			out[rel] = append(out[rel], coverBlock{startLine: ln, endLine: ln, count: 0})
		}
	}
	return out
}

// xccovLineData is the per-file JSON shape from `xcrun xccov view --file
// <path> --json <bundle>`: two flat integer arrays naming the covered and
// uncovered executable line numbers.
type xccovLineData struct {
	CoveredLines   []int `json:"coveredLines"`
	UncoveredLines []int `json:"uncoveredLines"`
}

// findJSONEnd returns the byte length of the first balanced top-level JSON
// object in s (starting at s[0]=='{'), or 0 if none. Used to truncate a
// ===FILE record body to its first object when xccov appends trailing data.
func findJSONEnd(s string) int {
	if len(s) == 0 || s[0] != '{' {
		return 0
	}
	depth := 0
	inStr := false
	escape := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			if escape {
				escape = false
			} else if c == '\\' {
				escape = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return 0
}

// parseLLVMCovBlocks parses `xcrun llvm-cov export --format=json` output
// (SwiftPM mode). The shape is:
//
//	{"data":[{"files":[{"filename":"<path>","segments":[[line,col,count,hasCount,regionCnt],...]}]}]}
//
// Per the parse map: consecutive segments define covered regions, so we emit
// one coverBlock per hasCount segment spanning [seg.line, nextSeg.line-1]
// (the last segment spans just itself). Segments are sorted by line for
// safety, though llvm-cov emits them sorted.
//
// llvm-cov segments are positional JSON arrays; Go's encoding/json does not
// decode arrays into structs positionally, so each segment is decoded as a
// []json.RawMessage and the positional fields are pulled out explicitly via
// decodeLLVMSegment.
func parseLLVMCovBlocks(raw, workDir string) map[string][]coverBlock {
	out := make(map[string][]coverBlock)
	var cov llvmCovExport
	if err := json.Unmarshal([]byte(raw), &cov); err != nil {
		return out
	}
	for _, unit := range cov.Data {
		for _, f := range unit.Files {
			rel := toRepoRelPOSIX(f.Filename, workDir)
			if rel == "" || rel == "." {
				continue
			}
			segs := f.Segments
			if len(segs) == 0 {
				continue
			}
			// Stable-sort by line so the [seg.line, nextSeg.line-1] span is
			// well-defined even if a future emitter shuffles segments.
			sort.SliceStable(segs, func(i, j int) bool {
				return segs[i].line < segs[j].line
			})
			for i := range segs {
				seg := &segs[i]
				if !seg.hasCount {
					continue
				}
				end := seg.line
				if i+1 < len(segs) && segs[i+1].line > seg.line {
					end = segs[i+1].line - 1
				}
				out[rel] = append(out[rel], coverBlock{
					startLine: seg.line,
					endLine:   end,
					count:     float64(seg.count),
				})
			}
		}
	}
	return out
}

// llvmCovExport is the relevant subset of `llvm-cov export --format=json`. Only
// data[].files[].filename and data[].files[].segments are consumed.
type llvmCovExport struct {
	Data []llvmCovUnit `json:"data"`
}

type llvmCovUnit struct {
	Files []llvmCovFile `json:"files"`
}

type llvmCovFile struct {
	Filename string          `json:"filename"`
	Segments []llvmCovSegRaw `json:"segments"`
}

// llvmCovSegRaw holds the raw positional elements of one llvm-cov segment.
// It implements json.Unmarshaler so each segment (a [line,col,count,hasCount,
// regionCnt] array) decodes its positional elements into the named fields.
// Encoding/json arrays do not map onto structs positionally by default, so we
// decode as []json.RawMessage first and pull out the values by index.
type llvmCovSegRaw struct {
	line     int
	count    int64
	hasCount bool
}

func (s *llvmCovSegRaw) UnmarshalJSON(data []byte) error {
	var arr []json.RawMessage
	if err := json.Unmarshal(data, &arr); err != nil {
		return err
	}
	if len(arr) < 4 {
		return fmt.Errorf("llvm-cov segment: expected >=4 elements, got %d", len(arr))
	}
	var line int
	if err := json.Unmarshal(arr[0], &line); err != nil {
		return fmt.Errorf("llvm-cov segment line: %w", err)
	}
	var count int64
	if err := json.Unmarshal(arr[2], &count); err != nil {
		return fmt.Errorf("llvm-cov segment count: %w", err)
	}
	var hc bool
	if err := json.Unmarshal(arr[3], &hc); err != nil {
		return fmt.Errorf("llvm-cov segment hasCount: %w", err)
	}
	s.line = line
	s.count = count
	s.hasCount = hc
	return nil
}

// Compile-time guard that the provider satisfies the interface — catches
// signature drift at build time rather than at dispatcher registration.
var _ coverageProvider = swiftCoverageProvider{}
