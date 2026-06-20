package steps

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// jsCoverageProvider is the coverageProvider for JavaScript/TypeScript projects.
// It is active when a package.json sits at the worktree root, runs the project's
// test runner under c8 (V8-native coverage), and parses the canonical Istanbul
// coverage-final.json into repo-relative blocks by stripping the workDir prefix.
// It self-registers in init() so the dispatcher picks it up with no edits to
// shared code.
type jsCoverageProvider struct{}

func init() {
	registerCoverageProvider(jsCoverageProvider{})
}

func (jsCoverageProvider) Name() string { return "js" }

// Active reports whether the worktree is a JS/TS project (package.json at root).
func (jsCoverageProvider) Active(workDir string, _ []string) bool {
	return fileExists(filepath.Join(workDir, "package.json"))
}

// CoverableChangedFiles filters the changed-file list to accountable JS/TS
// source files: *.js|.jsx|.ts|.tsx|.mjs|.cjs, non-test, non-ignored, still on
// disk. Test files are dropped per the JS-specific rule: any basename matching
// *.test.* or *.spec.*, and anything inside a __tests__/, test/, or tests/
// directory.
func (jsCoverageProvider) CoverableChangedFiles(changed []string, workDir string, ignorePatterns []string) []string {
	var out []string
	for _, path := range changed {
		path = strings.TrimSpace(path)
		if path == "" || !isJSSourceFile(path) {
			continue
		}
		if isJSTestFile(path) {
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

// RunCoverage runs the project's test runner under c8 with the json reporter,
// reads the canonical coverage-final.json, and returns its contents plus the
// human-readable tested command. The runner is resolved by resolveJSTestRunner;
// coverage is written to a temp reports dir and removed afterwards so the
// worktree is not polluted.
func (jsCoverageProvider) RunCoverage(sctx *pipeline.StepContext) (string, string, error) {
	if _, err := exec.LookPath("npx"); err != nil {
		return "", "", fmt.Errorf("npx not found on PATH: %w", err)
	}

	runner := resolveJSTestRunner(sctx)
	if runner == "" {
		return "", "", fmt.Errorf("no JS test runner configured (set NM_JS_TEST_RUNNER, commands.test, or add a package.json test script)")
	}

	reportsDir, err := os.MkdirTemp("", "nm-js-coverage-*")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(reportsDir)

	// `sh -c` wraps the runner so multi-word commands (`npm test`, `node --test`,
	// custom scripts) and any shell features keep working uniformly.
	args := []string{
		"--yes", "c8@latest",
		"--reporter=json",
		"--reports-dir=" + reportsDir,
		"--include=src/**",
		"sh", "-c", runner,
	}
	testedCmd := "npx " + strings.Join(args, " ")
	sctx.Log("running coverage: " + testedCmd)

	cmd := exec.CommandContext(sctx.Ctx, "npx", args...)
	cmd.Dir = sctx.WorkDir
	cmd.Env = mergeEnv(sctx.Env)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", testedCmd, fmt.Errorf("c8 run: %w: %s", err, strings.TrimSpace(string(out)))
	}

	data, err := os.ReadFile(filepath.Join(reportsDir, "coverage-final.json"))
	if err != nil {
		// Empty coverage (e.g. no source under src/** matched) is a soft skip:
		// return empty so ParseBlocks yields no blocks, rather than blocking.
		if os.IsNotExist(err) {
			return "", testedCmd, nil
		}
		return "", testedCmd, fmt.Errorf("read coverage-final.json: %w", err)
	}
	return string(data), testedCmd, nil
}

// ParseBlocks parses the canonical Istanbul coverage-final.json into coverBlocks
// keyed by repo-relative POSIX path. Each top-level <ABS_PATH> entry contributes
// one block per statement (statementMap[id] with hit count s[id]); branches are
// folded in for tighter executable-line detection. Path normalization goes
// through toRepoRelPOSIX so keys are byte-identical to `git diff --name-only`
// output — the #1 correctness invariant for every provider.
func (jsCoverageProvider) ParseBlocks(raw string, workDir string) map[string][]coverBlock {
	blocks := make(map[string][]coverBlock)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return blocks
	}
	var fc map[string]istanbulFileCoverage
	if err := json.Unmarshal([]byte(raw), &fc); err != nil {
		return blocks
	}
	for absPath, cov := range fc {
		rel := toRepoRelPOSIX(absPath, workDir)
		if rel == "" || rel == "." || filepath.IsAbs(rel) {
			// Out-of-worktree file (e.g. a transitive dep); drop so it can't
			// collide with git's repo-relative keys.
			continue
		}
		// Statements: one coverBlock per statementMap entry.
		for id, stmt := range cov.StatementMap {
			count := 0
			if c, ok := cov.S[id]; ok {
				count = c
			}
			blocks[rel] = append(blocks[rel], coverBlock{
				startLine: stmt.Start.Line,
				endLine:   stmt.End.Line,
				count:     float64(count),
			})
		}
		// Branches (optional refinement): fold in each branch location so a
		// single-line if/else with a hit on one arm but not the other is still
		// represented. Branch hit counts are arrays (one per location).
		for id, br := range cov.BranchMap {
			hits, ok := cov.B[id]
			if !ok {
				continue
			}
			for i, hit := range hits {
				if i >= len(br.Locations) {
					break
				}
				loc := br.Locations[i]
				blocks[rel] = append(blocks[rel], coverBlock{
					startLine: loc.Start.Line,
					endLine:   loc.End.Line,
					count:     float64(hit),
				})
			}
		}
	}
	return blocks
}

// resolveJSTestRunner picks the command c8 should wrap, in priority order:
//  1. NM_JS_TEST_RUNNER env var (explicit override, always wins)
//  2. sctx.Config.Commands.Test (the project's configured test command)
//  3. `npm test` when package.json declares a "test" script
//  4. `node --test` as the manifest-free fallback
//
// Returns "" when none apply (the caller treats that as a soft skip).
func resolveJSTestRunner(sctx *pipeline.StepContext) string {
	if v, ok := lookupStepEnv(sctx.Env, "NM_JS_TEST_RUNNER"); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if sctx.Config != nil && strings.TrimSpace(sctx.Config.Commands.Test) != "" {
		return strings.TrimSpace(sctx.Config.Commands.Test)
	}
	if pkgHasTestScript(sctx.WorkDir) {
		return "npm test"
	}
	return "node --test"
}

// pkgHasTestScript reports whether package.json at the worktree root defines a
// non-empty "scripts.test" entry. A missing or unreadable package.json returns
// false; the runner then falls back to `node --test`.
func pkgHasTestScript(workDir string) bool {
	data, err := os.ReadFile(filepath.Join(workDir, "package.json"))
	if err != nil {
		return false
	}
	var pkg struct {
		Scripts struct {
			Test string `json:"test"`
		} `json:"scripts"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return false
	}
	return strings.TrimSpace(pkg.Scripts.Test) != ""
}

// isJSSourceFile reports whether path has a JS/TS-family source extension.
func isJSSourceFile(path string) bool {
	exts := []string{".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs"}
	for _, ext := range exts {
		if strings.HasSuffix(path, ext) {
			return true
		}
	}
	return false
}

// isJSTestFile reports whether path is a JS/TS test file: basename contains
// `.test.` or `.spec.` (covering foo.test.js, foo.spec.tsx, foo.test.unit.js,
// etc. across all JS/TS extensions), or any path segment is __tests__, test, or
// tests. This is the JS provider's own rule — the shared isTestFile helper in
// common_diff.go covers a narrower extension set and is used by other steps for
// different purposes.
func isJSTestFile(path string) bool {
	base := filepath.Base(path)
	if strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") {
		return true
	}
	for _, seg := range strings.Split(filepath.ToSlash(path), "/") {
		if seg == "__tests__" || seg == "test" || seg == "tests" {
			return true
		}
	}
	return false
}

// istanbulFileCoverage models the per-file object in coverage-final.json. Only
// the fields the parser consumes are typed; the rest are ignored by the decoder.
type istanbulFileCoverage struct {
	StatementMap map[string]istanbulLocation `json:"statementMap"`
	S            map[string]int              `json:"s"`
	BranchMap    map[string]istanbulBranch   `json:"branchMap"`
	B            map[string][]int            `json:"b"`
}

// istanbulLocation is a [start, end) source range in line/column coordinates.
type istanbulLocation struct {
	Start istanbulPosition `json:"start"`
	End   istanbulPosition `json:"end"`
}

// istanbulPosition is a single line/column point in an istanbulLocation.
type istanbulPosition struct {
	Line   int `json:"line"`
	Column int `json:"column"`
}

// istanbulBranch describes one branch in coverage-final.json. Locations carries
// the per-arm source ranges; the parser emits one coverBlock per location using
// the parallel hit-count array from B[id].
type istanbulBranch struct {
	Locations []istanbulLocation `json:"locations"`
}
