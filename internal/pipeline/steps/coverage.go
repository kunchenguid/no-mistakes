package steps

import (
	"fmt"
	"os"
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
// changed file has no test coverage. The ID is namespaced per language and file
// path — `uncovered-changed-lines:<lang>:<path>` — so multi-language repos keep
// distinct, filterable identities in the TUI. The bare `uncovered-changed-lines:`
// prefix is preserved so downstream prefix matching keeps working. This
// subsumes the file-level check: a brand-new untested file has all lines added
// and uncovered, so it still fires.
const uncoveredChangedLinesIDPrefix = "uncovered-changed-lines:"

// addedLineRange is an inclusive, 1-indexed range of lines added on the new
// (+) side of a diff hunk, parsed from a `@@ -a,b +c,d @@` header (the range is
// [c, c+d-1]).
type addedLineRange struct {
	start, end int
}

// coverBlock is one parsed coverage block: a covered code region with its
// inclusive 1-indexed line span and its execution count. Language-neutral —
// each provider's ParseBlocks produces these keyed by repo-relative POSIX path.
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
// (0/false/no/off) the check, and that override always wins. When unset, the
// check defaults to ON when ANY registered coverage provider is active for the
// worktree (e.g. a go.mod, package.json, or Package.swift is present) and OFF
// otherwise. This keeps the gate language-agnostic: each provider owns its own
// detection in Active(), so adding a language never requires touching this gate.
func coverageCheckEnabled(workDir string, env []string) bool {
	if v, ok := lookupStepEnv(env, "NO_MISTAKES_COVERAGE_CHECK"); ok {
		return isAffirmativeFlag(v)
	}
	for _, p := range coverageProviders {
		if p.Active(workDir, env) {
			return true
		}
	}
	return false
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
// exercised by any count>0 coverage block. This is the shared, language-neutral
// core: it operates purely on the three data inputs every provider produces
// (coverable paths, added-line ranges, coverage blocks) and never references a
// specific language. The dispatcher namespaces each finding's ID per language
// via namespaceFindings after this returns.
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

// runCoverageCheck is the coverage-aware sub-step invoked after the test suite
// passes. It is a pluggable dispatcher: it performs the language-neutral diff
// plumbing once, then loops over every registered coverageProvider, asking each
// active one to filter coverable files, run its native coverage tool, and parse
// the result into blocks. Each provider's blocks feed into the unchanged,
// language-neutral uncoveredChangedLineFindings core, and the resulting
// findings are namespaced per language.
//
// It returns one warning finding per changed source file (in any language) that
// adds code no test exercises — computed at the changed-line (diff) level so a
// new function dropped into an already-tested file is still caught. It is a
// no-op when disabled, when no provider has coverable changed files, or when a
// provider's coverage collection fails (provider errors are logged and the
// check degrades to a no-op for that language — never blocking the pipeline).
func runCoverageCheck(sctx *pipeline.StepContext, baseSHA string) ([]Finding, string, error) {
	if !coverageCheckEnabled(sctx.WorkDir, sctx.Env) {
		return nil, "", nil
	}
	// --- neutral diff plumbing (shared across all providers) ---
	changedRaw, err := git.Run(sctx.Ctx, sctx.WorkDir, "diff", "--name-only", baseSHA+".."+sctx.Run.HeadSHA)
	if err != nil {
		return nil, "", fmt.Errorf("coverage: get changed files: %w", err)
	}
	changed := strings.Split(changedRaw, "\n")
	// Added-line ranges per file from a tight (unified=0) diff. Line numbers on
	// the + side line up with HEAD and therefore with each provider's blocks.
	diffRaw, err := git.Run(sctx.Ctx, sctx.WorkDir, "diff", "--unified=0", baseSHA+".."+sctx.Run.HeadSHA)
	if err != nil {
		return nil, "", fmt.Errorf("coverage: get changed-line diff: %w", err)
	}
	added := parseAddedLineRanges(diffRaw)
	ignores := sctx.Config.IgnorePatterns

	// --- one pass per active language provider ---
	var findings []Finding
	var tested []string
	for _, p := range coverageProviders {
		if !p.Active(sctx.WorkDir, sctx.Env) {
			continue
		}
		coverable := p.CoverableChangedFiles(changed, sctx.WorkDir, ignores)
		if len(coverable) == 0 {
			continue
		}
		raw, testedCmd, err := p.RunCoverage(sctx)
		if err != nil {
			// Errors degrade to a logged no-op for this language — never block
			// the pipeline (matches the test.go call-site contract).
			sctx.Log(fmt.Sprintf("coverage[%s] skipped: %v", p.Name(), err))
			continue
		}
		if testedCmd != "" {
			tested = append(tested, testedCmd)
		}
		blocks := p.ParseBlocks(raw, sctx.WorkDir)
		perLang := uncoveredChangedLineFindings(coverable, added, blocks)
		findings = append(findings, namespaceFindings(p.Name(), perLang)...)
	}
	return findings, strings.Join(tested, "\n"), nil
}
