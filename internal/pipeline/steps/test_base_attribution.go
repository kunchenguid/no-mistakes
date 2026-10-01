package steps

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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

var (
	ansiEscape         = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")
	testDurationSuffix = regexp.MustCompile(`\s*\(\d+(?:\.\d+)?\s*(?:s|ms|µs|us)\)|\s+\d+(?:\.\d+)?s$`)
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
	baseFailures := map[string]bool{}
	if baseExitCode != 0 {
		for _, line := range testFailureLines(baseOutput) {
			baseFailures[line] = true
		}
	}
	for _, line := range testFailureLines(headOutput) {
		if baseFailures[line] {
			attribution.preexisting = append(attribution.preexisting, line)
		} else {
			attribution.introduced = append(attribution.introduced, line)
		}
	}
	return attribution, nil
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
		output, exitCode, err := runShellCommandWithProcessEnv(sctx.Ctx, checkout, env, prepareCmd)
		if err != nil {
			return "", 0, fmt.Errorf("prepare base checkout: %w", err)
		}
		if exitCode != 0 {
			logCommandOutput(sctx, output, "Prepare (base)", types.StepTest)
			return "", 0, fmt.Errorf("prepare command exited with code %d on the base checkout", exitCode)
		}
	}
	output, exitCode, err := runShellCommandWithProcessEnv(sctx.Ctx, checkout, env, testCmd)
	if err != nil {
		return "", 0, fmt.Errorf("run test command on base: %w", err)
	}
	logCommandOutput(sctx, output, "Test (base)", types.StepTest)
	return output, exitCode, nil
}

// testFailureLines returns the recognized failure lines of output in order,
// deduplicated, and normalized so run-to-run noise (durations, TAP ordinals,
// colors, indentation) does not make the same failure look new.
func testFailureLines(output string) []string {
	seen := map[string]bool{}
	var lines []string
	for _, raw := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(ansiEscape.ReplaceAllString(raw, ""))
		if !testFailureLine.MatchString(line) {
			continue
		}
		line = tapOrdinal.ReplaceAllString(line, "not ok")
		line = strings.TrimSpace(testDurationSuffix.ReplaceAllString(line, ""))
		if line == "" || seen[line] {
			continue
		}
		seen[line] = true
		lines = append(lines, line)
	}
	return lines
}

// render is the attribution the Test step puts ahead of the failing command's
// output in the findings summary and in the evidence prompt.
func (a baseAttribution) render() string {
	if a.unavailable != "" {
		return "Base attribution unavailable (" + a.unavailable + "); every failure is attributed to this change."
	}
	base := shortSHA(a.baseSHA)
	if a.baseExitCode == 0 {
		return "Base attribution: commands.test passes on base commit " + base + ", so this change introduced the failure." + renderAttributedList("Introduced by this change", a.introduced)
	}
	header := fmt.Sprintf("Base attribution: commands.test also fails on base commit %s (exit code %d).", base, a.baseExitCode)
	if len(a.introduced) == 0 && len(a.preexisting) == 0 {
		return header + " No individual failures were recognized, so introduced and pre-existing failures could not be separated."
	}
	return header + renderAttributedList("Introduced by this change", a.introduced) + renderAttributedList("Pre-existing on the base commit", a.preexisting)
}

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
