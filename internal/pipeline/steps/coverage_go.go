package steps

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// goCoverageProvider is the coverageProvider for Go modules. It is active when
// a go.mod sits at the worktree root, runs `go test -cover`, and parses the
// coverprofile into repo-relative blocks by stripping the module path. It
// self-registers in init() so the dispatcher picks it up with no edits to
// shared code.
type goCoverageProvider struct{}

func init() {
	registerCoverageProvider(goCoverageProvider{})
}

func (goCoverageProvider) Name() string { return "go" }

// Active reports whether the worktree is a Go module (go.mod at the root).
func (goCoverageProvider) Active(workDir string, _ []string) bool {
	return fileExists(filepath.Join(workDir, "go.mod"))
}

// CoverableChangedFiles filters the changed-file list to accountable Go source
// files (.go, non-test, non-ignored, still on disk).
func (goCoverageProvider) CoverableChangedFiles(changed []string, workDir string, ignorePatterns []string) []string {
	return coverableChangedGoFiles(changed, workDir, ignorePatterns)
}

// RunCoverage runs `go test -cover -coverprofile=<tmp> ./...` and returns the
// coverprofile contents plus the human-readable command for the "tested" log.
func (goCoverageProvider) RunCoverage(sctx *pipeline.StepContext) (string, string, error) {
	return runGoCoverageProfile(sctx)
}

// ParseBlocks parses the coverprofile into blocks keyed by repo-relative path,
// stripping the module-path prefix so keys match `git diff` output.
func (goCoverageProvider) ParseBlocks(raw string, workDir string) map[string][]coverBlock {
	return parseGoCoverProfileBlocks(raw, goModulePath(workDir))
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
