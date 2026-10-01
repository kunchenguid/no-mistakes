package steps

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// baseAttributionMaxListed bounds each attributed failure list, and
// baseAttributionMaxLineRunes each listed line, so a suite that fails
// thousands of tests cannot grow the finding past what findings, IPC, and
// repair prompts carry. The complete outputs stay in the step log.
const (
	baseAttributionMaxListed    = 20
	baseAttributionMaxLineRunes = 300
)

// testFailureLine recognizes the per-test failure lines common runners print:
// go test (`--- FAIL: TestX`), pytest (`FAILED path::test`), jest/vitest
// (`✕ name`), TAP (`not ok N - name`), and cargo (`test name ... FAILED`).
// Package- or file-level aggregates (`FAIL\tpkg`, jest's `FAIL file`) are
// deliberately not matched: they fail on both sides whenever any test in them
// does, so they would list a package holding a new regression as
// pre-existing. A suite whose failures match none of these gets an honest
// "could not separate" attribution instead of a guessed one.
var testFailureLine = regexp.MustCompile(`^(?:--- FAIL: |FAILED |not ok |[✕✗×] )|\.\.\. FAILED$`)

// goPackageResult is the `FAIL\tpkg` / `ok  \tpkg` line that closes a go test
// package's block. It names the package of the per-test lines printed before
// it, which carry only the test name, so same-named tests in different
// packages are told apart. The tab keeps jest's space-separated `FAIL file`
// header out.
var goPackageResult = regexp.MustCompile(`^(?:FAIL|ok)\s*\t(\S+)`)

var (
	ansiEscape         = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")
	testDurationSuffix = regexp.MustCompile(`\s*\(\d+(?:\.\d+)?\s*(?:s|ms|µs|us)\)|\s+\d+(?:\.\d+)?s$|\s*# time=\d+(?:\.\d+)?m?s$`)
	tapOrdinal         = regexp.MustCompile(`^not ok \d+`)
)

// baseAttribution is the result of re-running a failing commands.test on the
// run's base commit: which of the head's failures the change introduced and
// which already fail without it.
type baseAttribution struct {
	baseSHA      string
	baseExitCode int
	introduced   []string
	preexisting  []string
	// ambiguous holds failures that also fail on the base but carry no
	// package or file identity, so the match may be a different test sharing
	// the name.
	ambiguous []string
	// unattributed holds head package results that failed without any
	// per-test line (a build failure, a TestMain or init panic) and did not
	// fail that way on the base, so no test inside them can be attributed.
	unattributed []string
	// unavailable explains why no base result exists; every failure then
	// stays attributed to the change, exactly as without the opt-in.
	unavailable string
}

// attributeTestFailures runs testCmd on a disposable checkout of baseSHA and
// diffs its failures against the head's. Attribution only informs: the head
// failure keeps its severity, action, and description (which is also the
// configured-command override reason), so an approval over a pre-existing
// failure is still a recorded waiver. The only error returned is the step's
// own cancellation; any other problem yields an unavailable attribution and
// the run continues.
func attributeTestFailures(sctx *pipeline.StepContext, testCmd, baseSHA, headOutput string) (baseAttribution, error) {
	attribution := baseAttribution{baseSHA: baseSHA}
	if strings.TrimSpace(baseSHA) == "" || baseSHA == git.EmptyTreeSHA || git.IsZeroSHA(baseSHA) {
		attribution.unavailable = "the run has no base commit"
		return attribution, nil
	}
	sctx.Log(fmt.Sprintf("configured test command failed; re-running it on base commit %s to attribute failures...", shortSHA(baseSHA)))
	baseOutput, baseExitCode, err := runTestCommandOnBase(sctx, testCmd, baseSHA)
	if ctxErr := sctx.Ctx.Err(); ctxErr != nil {
		return attribution, ctxErr
	}
	if err != nil {
		sctx.Log(fmt.Sprintf("warning: base attribution unavailable: %v", err))
		attribution.unavailable = err.Error()
		return attribution, nil
	}
	attribution.baseExitCode = baseExitCode
	attribution.classify(headOutput, baseOutput)
	return attribution, nil
}

// classify splits the head's recognized failures by whether they also fail in
// baseOutput. A failing base with no recognized failure lines leaves every
// list empty, so render reports that the two could not be separated rather
// than listing every head failure as introduced.
func (a *baseAttribution) classify(headOutput, baseOutput string) {
	baseFailures := map[string]bool{}
	baseUnattributed := map[string]bool{}
	if a.baseExitCode != 0 {
		lines, _, unattributed := testFailureLines(baseOutput)
		if len(lines) == 0 {
			return
		}
		for _, line := range lines {
			baseFailures[line] = true
		}
		for _, line := range unattributed {
			baseUnattributed[line] = true
		}
	}
	// A key carries its package or file in its own text, so a head line that
	// is qualified matches only an identically qualified base line.
	headLines, qualified, headUnattributed := testFailureLines(headOutput)
	for _, line := range headUnattributed {
		if !baseUnattributed[line] {
			a.unattributed = append(a.unattributed, line)
		}
	}
	for _, line := range headLines {
		switch {
		case !baseFailures[line]:
			a.introduced = append(a.introduced, line)
		case !qualified[line]:
			a.ambiguous = append(a.ambiguous, line)
		default:
			a.preexisting = append(a.preexisting, line)
		}
	}
}

// runTestCommandOnBase checks baseSHA out into a throwaway clone that shares
// the run worktree's object store, runs the trusted commands.prepare there
// when configured, then testCmd. A separate clone rather than a linked
// worktree keeps the gate repository and the run worktree untouched.
func runTestCommandOnBase(sctx *pipeline.StepContext, testCmd, baseSHA string) (string, int, error) {
	scratch, err := os.MkdirTemp("", "no-mistakes-test-base-")
	if err != nil {
		return "", 0, fmt.Errorf("create base checkout directory: %w", err)
	}
	defer os.RemoveAll(scratch)
	checkout := filepath.Join(scratch, "base")
	source, err := filepath.Abs(sctx.WorkDir)
	if err != nil {
		return "", 0, fmt.Errorf("resolve run worktree: %w", err)
	}
	if _, err := stepGitRun(sctx, "clone", "--quiet", "--shared", "--no-checkout", source, checkout); err != nil {
		return "", 0, fmt.Errorf("clone base checkout: %w", err)
	}
	if _, err := stepGitRun(sctx, "-C", checkout, "checkout", "--quiet", "--detach", baseSHA); err != nil {
		return "", 0, fmt.Errorf("check out base commit %s: %w", shortSHA(baseSHA), err)
	}
	env := stepEnvironment(sctx)
	if prepareCmd := strings.TrimSpace(sctx.Config.Commands.Prepare); prepareCmd != "" {
		output, exitCode, err := runShellCommandWithPriority(sctx.Ctx, checkout, env, prepareCmd, sctx.Config.CommandOverrides["prepare"].Nice)
		if err != nil {
			return "", 0, fmt.Errorf("prepare base checkout: %w", err)
		}
		if output != "" {
			logCommandOutput(sctx, output, "Prepare (base)", types.StepTest)
		}
		if exitCode != 0 {
			return "", 0, fmt.Errorf("prepare command exited with code %d on the base checkout", exitCode)
		}
	}
	output, exitCode, err := runShellCommandWithPriority(sctx.Ctx, checkout, env, testCmd, sctx.Config.CommandOverrides["test"].Nice)
	if err != nil {
		return "", 0, fmt.Errorf("run test command on base: %w", err)
	}
	logCommandOutput(sctx, output, "Test (base)", types.StepTest)
	return output, exitCode, nil
}

// testFailureLines returns the recognized failure lines of output in order,
// deduplicated, and normalized so run-to-run noise (durations, TAP ordinals,
// colors, indentation, pytest's failure reason) does not make the same
// failure look new. A go test line is prefixed with the package its block
// closes with. qualified names the lines that carry a package or file
// identity (a packaged go test or a pytest `path::test` id); any other line
// may be a different test sharing the name. unattributed lists the go
// `FAIL pkg` results that closed a block without any per-test failure line.
func testFailureLines(output string) (lines []string, qualified map[string]bool, unattributed []string) {
	var failures []string
	qualified = map[string]bool{}
	var pendingGo []int
	for _, raw := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(ansiEscape.ReplaceAllString(raw, ""))
		if m := goPackageResult.FindStringSubmatch(line); m != nil {
			if len(pendingGo) == 0 && strings.HasPrefix(line, "FAIL") {
				result := strings.Join(strings.Fields(testDurationSuffix.ReplaceAllString(line, "")), " ")
				if !slices.Contains(unattributed, result) {
					unattributed = append(unattributed, result)
				}
			}
			for _, i := range pendingGo {
				failures[i] = m[1] + ": " + failures[i]
				qualified[failures[i]] = true
			}
			pendingGo = nil
			continue
		}
		if !testFailureLine.MatchString(line) {
			continue
		}
		line = tapOrdinal.ReplaceAllString(line, "not ok")
		line = strings.TrimSpace(testDurationSuffix.ReplaceAllString(line, ""))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "--- FAIL: ") {
			pendingGo = append(pendingGo, len(failures))
		}
		if strings.HasPrefix(line, "FAILED ") {
			line = pytestTestID(line)
			if strings.Contains(line, "::") {
				qualified[line] = true
			}
		}
		failures = append(failures, line)
	}
	seen := map[string]bool{}
	for _, line := range failures {
		if !seen[line] {
			seen[line] = true
			lines = append(lines, line)
		}
	}
	return lines, qualified, unattributed
}

// pytestTestID drops the " - reason" pytest appends to a FAILED line. The
// reason starts at the first " - " outside any [...] parameter id, so a
// parametrized id that itself contains " - " stays whole.
func pytestTestID(line string) string {
	depth := 0
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '[':
			depth++
		case ']':
			if depth > 0 {
				depth--
			}
		case ' ':
			if depth == 0 && strings.HasPrefix(line[i:], " - ") {
				return line[:i]
			}
		}
	}
	return line
}

// render is the attribution the Test step puts ahead of the failing command's
// output in the findings summary and in the evidence prompt.
func (a baseAttribution) render() string {
	if a.unavailable != "" {
		return "Base attribution unavailable (" + a.unavailable + "); every failure is attributed to this change."
	}
	base := shortSHA(a.baseSHA)
	if a.baseExitCode == 0 {
		return "Base attribution: commands.test passes on base commit " + base + ", so this change introduced the failure." + renderAttributedList("Introduced by this change", a.introduced) + renderAttributedList(unattributedLabel, a.unattributed)
	}
	header := fmt.Sprintf("Base attribution: commands.test also fails on base commit %s (exit code %d).", base, a.baseExitCode)
	if len(a.introduced) == 0 && len(a.preexisting) == 0 && len(a.ambiguous) == 0 && len(a.unattributed) == 0 {
		return header + " Individual failures were not recognized in both outputs, so introduced and pre-existing failures could not be separated."
	}
	return header + renderAttributedList("Introduced by this change", a.introduced) + renderAttributedList("Pre-existing on the base commit", a.preexisting) + renderAttributedList("Ambiguous, could not attribute", a.ambiguous) + renderAttributedList(unattributedLabel, a.unattributed)
}

const unattributedLabel = "Failures without a per-test line (could not be attributed)"

func renderAttributedList(label string, lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n%s (%d):", label, len(lines))
	for i, line := range lines {
		if i == baseAttributionMaxListed {
			fmt.Fprintf(&b, "\n- ... and %d more (see the step log)", len(lines)-i)
			break
		}
		if runes := []rune(line); len(runes) > baseAttributionMaxLineRunes {
			line = string(runes[:baseAttributionMaxLineRunes]) + "..."
		}
		b.WriteString("\n- " + line)
	}
	return b.String()
}
