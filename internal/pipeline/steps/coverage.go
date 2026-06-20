package steps

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// Coverage finding emitted by the file-level view when a changed file has zero
// coverage. Retained as a tested primitive; the orchestrator now uses the
// finer-grained changed-line check (uncoveredChangedLinesIDPrefix below), which
// subsumes this one.
const uncoveredChangedFileIDPrefix = "uncovered-changed-file:"

// Coverage finding emitted when at least one added (new-side) diff line in a
// changed file has no test coverage. The ID is namespaced with the file path so
// multiple files keep distinct, filterable identities in the TUI. This
// subsumes the file-level check: a brand-new untested file has all lines added
// and uncovered, so it still fires.
const uncoveredChangedLinesIDPrefix = "uncovered-changed-lines:"

// addedLineRange is an inclusive, 1-indexed range of lines added on the new
// (+) side of a diff hunk, parsed from a `@@ -a,b +c,d @@` header (the range is
// [c, c+d-1]).
type addedLineRange struct {
	start, end int
}

// coverBlock is one parsed coverprofile block: a covered code region with its
// inclusive 1-indexed line span (from the coverprofile .col fields, which are
// aligned with HEAD) and its execution count.
type coverBlock struct {
	startLine, endLine int
	count              float64
}

// lookupStepEnv resolves an environment variable from the StepContext's extra
// env first (so tests can inject overrides), then from the process environ.
func lookupStepEnv(env []string, key string) (string, bool) {
	if v, ok := envValue(env, key); ok {
		return v, true
	}
	return os.LookupEnv(key)
}

// isAffirmativeFlag reports whether a flag value should enable a feature.
func isAffirmativeFlag(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

// coverageCheckEnabled decides whether the coverage-aware sub-step runs.
//
// NO_MISTAKES_COVERAGE_CHECK explicitly enables (1/true/yes/on) or disables
// (0/false/no/off) the check. When unset, the check defaults to ON for Go
// projects (a go.mod is present at the worktree root) and OFF otherwise, since
// only Go coverage is wired up today.
func coverageCheckEnabled(workDir string, env []string) bool {
	if v, ok := lookupStepEnv(env, "NO_MISTAKES_COVERAGE_CHECK"); ok {
		return isAffirmativeFlag(v)
	}
	return fileExists(filepath.Join(workDir, "go.mod"))
}

// goModulePath reads the module path from go.mod at the worktree root.
// Returns "" if go.mod is missing or has no module directive.
func goModulePath(workDir string) string {
	data, err := os.ReadFile(filepath.Join(workDir, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "module" {
			return fields[1]
		}
	}
	return ""
}

// parseGoCoverProfile parses the contents of a `go test -coverprofile` file and
// returns the set of repo-relative Go source files that have at least one
// covered code block (block count > 0).
//
// This is the file-level view. The orchestrator's changed-line check uses the
// richer parseGoCoverProfileBlocks plus the diff intersection; this helper is
// retained as a tested primitive for the simpler "is the file covered at all"
// question. A file whose blocks are all zero-count is intentionally absent from
// the map, so it correctly reads as uncovered.
func parseGoCoverProfile(raw, modulePath string) map[string]bool {
	blocks := parseGoCoverProfileBlocks(raw, modulePath)
	covered := make(map[string]bool, len(blocks))
	for file, bs := range blocks {
		for _, b := range bs {
			if b.count > 0 {
				covered[file] = true
				break
			}
		}
	}
	return covered
}

// parseGoCoverProfileBlocks parses a `go test -coverprofile` file into covered
// code blocks keyed by repo-relative source file. Each block carries its
// inclusive 1-indexed start/end line numbers (from the coverprofile .col fields,
// which line up with HEAD since the instrumented build includes the change) and
// its execution count.
//
// Each profile line is: <pkgpath>/<file>:<startLine>.<startCol>,<endLine>.<endCol> <stmts> <count>
// modulePath is stripped from the package-qualified path so keys match the
// repo-relative paths returned by `git diff`. Entries outside the module (e.g.
// vendored dependencies) are ignored.
func parseGoCoverProfileBlocks(raw, modulePath string) map[string][]coverBlock {
	blocks := make(map[string][]coverBlock)
	if strings.TrimSpace(raw) == "" {
		return blocks
	}
	prefix := ""
	if strings.TrimSpace(modulePath) != "" {
		prefix = modulePath + "/"
	}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "mode:") {
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		pkgPath := line[:colon]
		parts := strings.Fields(line[colon+1:])
		if len(parts) < 3 {
			continue
		}
		startLine, endLine, ok := parseCoverLoc(parts[0])
		if !ok {
			continue
		}
		count, err := strconv.ParseFloat(parts[len(parts)-1], 64)
		if err != nil {
			continue
		}
		rel := pkgPath
		if prefix != "" {
			if !strings.HasPrefix(pkgPath, prefix) {
				continue
			}
			rel = strings.TrimPrefix(pkgPath, prefix)
		}
		blocks[rel] = append(blocks[rel], coverBlock{startLine: startLine, endLine: endLine, count: count})
	}
	return blocks
}

// parseCoverLoc parses a coverprofile location "startLine.startCol,endLine.endCol"
// and returns the inclusive start/end line numbers.
func parseCoverLoc(loc string) (int, int, bool) {
	comma := strings.IndexByte(loc, ',')
	if comma < 0 {
		return 0, 0, false
	}
	startLine, ok1 := atoiBeforeDot(loc[:comma])
	endLine, ok2 := atoiBeforeDot(loc[comma+1:])
	if !ok1 || !ok2 {
		return 0, 0, false
	}
	return startLine, endLine, true
}

// atoiBeforeDot parses the leading integer of s up to the first '.' (if any).
func atoiBeforeDot(s string) (int, bool) {
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		s = s[:dot]
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

// parseAddedLineRanges parses `git diff --unified=0` output into per-file
// added-line ranges. Only the new (+) side line counts from `@@ -a,b +c,d @@`
// hunks are used; pure deletions (d==0) contribute no range. The file path is
// taken from the `diff --git a/<path> b/<path>` header (b-side) via
// extractDiffPath, so it matches the repo-relative paths used elsewhere.
func parseAddedLineRanges(diff string) map[string][]addedLineRange {
	out := make(map[string][]addedLineRange)
	if diff == "" {
		return out
	}
	var file string
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			file = extractDiffPath(line)
		case strings.HasPrefix(line, "@@"):
			r, ok := parseHunkAddedRange(line)
			if !ok || file == "" {
				continue
			}
			out[file] = append(out[file], r)
		}
	}
	return out
}

// parseHunkAddedRange parses a unified-diff hunk header line and returns the
// new (+) side added-line range. Returns ok=false when there is no + side or it
// has zero lines (pure deletion). Accepts both "@@ -a,b +c,d @@ ctx" and the
// single-line "@@ -a +c @@" forms.
func parseHunkAddedRange(line string) (addedLineRange, bool) {
	// Body is the text between the leading "@@" and the closing "@@".
	rest := line
	if !strings.HasPrefix(rest, "@@") {
		return addedLineRange{}, false
	}
	rest = rest[2:]
	closeAt := strings.Index(rest, "@@")
	if closeAt < 0 {
		return addedLineRange{}, false
	}
	fields := strings.Fields(strings.TrimSpace(rest[:closeAt]))
	if len(fields) < 2 {
		return addedLineRange{}, false
	}
	plusField := fields[1]
	if !strings.HasPrefix(plusField, "+") {
		return addedLineRange{}, false
	}
	nums := strings.TrimPrefix(plusField, "+")
	startStr, countStr, hasCount := strings.Cut(nums, ",")
	start, err := strconv.Atoi(startStr)
	if err != nil || start < 0 {
		return addedLineRange{}, false
	}
	count := 1
	if hasCount {
		c, err := strconv.Atoi(countStr)
		if err != nil || c < 0 {
			return addedLineRange{}, false
		}
		count = c
	}
	if count == 0 {
		return addedLineRange{}, false
	}
	return addedLineRange{start: start, end: start + count - 1}, true
}

// coverableChangedGoFiles filters the changed-file list down to Go source files
// the coverage check should hold accountable: .go files that are not test
// files, do not match any ignore pattern, and still exist on disk (so pure
// deletions are not flagged).
func coverableChangedGoFiles(changedFiles []string, workDir string, ignorePatterns []string) []string {
	var out []string
	for _, path := range changedFiles {
		path = strings.TrimSpace(path)
		if path == "" || !strings.HasSuffix(path, ".go") {
			continue
		}
		if isTestFile(path) {
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

// uncoveredFileFindings builds a warning finding for each coverable changed
// file that has zero coverage (absent from the covered map or explicitly false).
// This is the file-level view, retained as a tested primitive; the orchestrator
// uses uncoveredChangedLineFindings, which subsumes it.
func uncoveredFileFindings(coverable []string, covered map[string]bool) []Finding {
	var findings []Finding
	for _, path := range coverable {
		if covered[path] {
			continue
		}
		findings = append(findings, Finding{
			ID:          uncoveredChangedFileIDPrefix + path,
			Severity:    "warning",
			File:        path,
			Description: "Changed file has 0% test coverage — add a test that exercises it",
			Action:      "ask-user",
		})
	}
	return findings
}

// uncoveredChangedLineFindings emits one warning finding per coverable changed
// file where at least one added (new-side) diff line is executable but not
// exercised by any count>0 coverprofile block.
//
// Counting rule per added line L in the file:
//   - If a count>0 block spans L → covered, skip.
//   - Else if a count==0 block spans L → executable but unexecuted → count it.
//   - Else (L is in no block, e.g. blank/comment lines) and the file has at
//     least one block → ignore (non-executable).
//   - Else (L is in no block) and the file has NO blocks at all → the file is
//     entirely untested/uninstrumented; count L so brand-new untested files
//     still fire (this is what makes the check subsume file-level coverage).
//
// This avoids blank/comment-line false positives while still flagging any
// changed file that adds untested code, including brand-new untested files.
func uncoveredChangedLineFindings(coverable []string, added map[string][]addedLineRange, blocks map[string][]coverBlock) []Finding {
	var findings []Finding
	for _, path := range coverable {
		ranges, ok := added[path]
		if !ok || len(ranges) == 0 {
			continue
		}
		bs := blocks[path]
		uncovered := 0
		for _, r := range ranges {
			for line := r.start; line <= r.end; line++ {
				if !addedLineCovered(line, bs) && addedLineExecutable(line, bs) {
					uncovered++
				}
			}
		}
		if uncovered > 0 {
			findings = append(findings, Finding{
				ID:          uncoveredChangedLinesIDPrefix + path,
				Severity:    "warning",
				File:        path,
				Description: fmt.Sprintf("%d changed line(s) have no test coverage — add a test that exercises the new code", uncovered),
				Action:      "ask-user",
			})
		}
	}
	return findings
}

// addedLineCovered reports whether line falls within some count>0 block.
func addedLineCovered(line int, blocks []coverBlock) bool {
	for _, b := range blocks {
		if b.count > 0 && line >= b.startLine && line <= b.endLine {
			return true
		}
	}
	return false
}

// addedLineExecutable reports whether line is executable code worth flagging
// when uncovered. A line inside any block (covered or not) is executable. A
// line in no block is only treated as executable when the file has no blocks at
// all — that is the entirely-untested-file case, where every added line counts
// so the finding still fires (subsumption of file-level coverage).
func addedLineExecutable(line int, blocks []coverBlock) bool {
	if len(blocks) == 0 {
		return true
	}
	for _, b := range blocks {
		if line >= b.startLine && line <= b.endLine {
			return true
		}
	}
	return false
}

// runGoCoverageProfile runs `go test -cover -coverprofile=<tmp> ./...` in the
// worktree and returns the coverprofile contents plus the human-readable
// command for the "tested" log. The temp profile is removed afterwards.
func runGoCoverageProfile(sctx *pipeline.StepContext) (string, string, error) {
	tmp, err := os.CreateTemp("", "nm-coverage-*.out")
	if err != nil {
		return "", "", err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	args := []string{"test", "-cover", "-coverprofile=" + tmpPath, "./..."}
	testedCmd := "go " + strings.Join(args, " ")
	sctx.Log("running coverage: " + testedCmd)

	cmd := exec.CommandContext(sctx.Ctx, "go", args...)
	cmd.Dir = sctx.WorkDir
	cmd.Env = mergeEnv(sctx.Env)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", testedCmd, fmt.Errorf("go test -cover: %w: %s", err, strings.TrimSpace(string(out)))
	}
	data, err := os.ReadFile(tmpPath)
	if err != nil {
		return "", testedCmd, fmt.Errorf("read coverprofile: %w", err)
	}
	return string(data), testedCmd, nil
}

// runCoverageCheck is the coverage-aware sub-step invoked after the test suite
// passes. It returns one warning finding per changed Go source file that adds
// code no test exercises — computed at the changed-line (diff) level so a new
// function dropped into an already-tested file is still caught. It is a no-op
// when disabled, when there are no coverable changed files, or when coverage
// collection fails (errors are surfaced to the caller so the step can log and
// continue without blocking the pipeline).
func runCoverageCheck(sctx *pipeline.StepContext, baseSHA string) ([]Finding, string, error) {
	if !coverageCheckEnabled(sctx.WorkDir, sctx.Env) {
		return nil, "", nil
	}
	changedRaw, err := git.Run(sctx.Ctx, sctx.WorkDir, "diff", "--name-only", baseSHA+".."+sctx.Run.HeadSHA)
	if err != nil {
		return nil, "", fmt.Errorf("coverage: get changed files: %w", err)
	}
	coverable := coverableChangedGoFiles(strings.Split(changedRaw, "\n"), sctx.WorkDir, sctx.Config.IgnorePatterns)
	if len(coverable) == 0 {
		return nil, "", nil
	}
	// Added-line ranges per file from a tight (unified=0) diff. Line numbers on
	// the + side line up with HEAD and therefore with the coverprofile blocks.
	diffRaw, err := git.Run(sctx.Ctx, sctx.WorkDir, "diff", "--unified=0", baseSHA+".."+sctx.Run.HeadSHA)
	if err != nil {
		return nil, "", fmt.Errorf("coverage: get changed-line diff: %w", err)
	}
	added := parseAddedLineRanges(diffRaw)
	raw, testedCmd, err := runGoCoverageProfile(sctx)
	if err != nil {
		return nil, testedCmd, err
	}
	blocks := parseGoCoverProfileBlocks(raw, goModulePath(sctx.WorkDir))
	return uncoveredChangedLineFindings(coverable, added, blocks), testedCmd, nil
}
