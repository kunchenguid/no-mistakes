package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// The Windows test leg is process-spawn bound: the git-backed packages run
// thousands of git.exe invocations, and Defender real-time scanning taxes every
// one. Untuned, a single ./... job compiled every binary and then ran those
// packages sequentially until timeout-minutes cancelled it with no verdict.
// These tests pin the properties that keep that from silently coming back - the
// scan-exclusion step, a three-way shard split (core remainder, git-heavy
// packages without pipeline/steps, and pipeline/steps alone) so each job's wall
// stays inside the cap, and a per-binary Go timeout well inside that cap so a
// genuine hang lands as a goroutine dump instead of an opaque job cancellation.
//
// The workflow cannot be exercised from `go test` (it needs a Windows runner),
// so it is asserted through a typed workflow, `go list` package sets, and a
// normalized command view.

func loadCIWorkflowDoc(t *testing.T) *wfDoc {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read CI workflow: %v", err)
	}
	var wf wfDoc
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse CI workflow: %v", err)
	}
	for name, job := range wf.Jobs {
		job.name = name
	}
	return &wf
}

func ciTestJob(t *testing.T) *wfJob {
	t.Helper()
	job, ok := loadCIWorkflowDoc(t).Jobs["test"]
	if !ok {
		t.Fatal("CI workflow has no test job")
	}
	return job
}

type workflowCommand struct {
	step int
	line int
	name string
	args []string
}

func normalizeWorkflowCondition(condition string) string {
	condition = strings.TrimSpace(condition)
	condition = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(condition, "${{"), "}}"))
	return strings.Join(strings.Fields(condition), " ")
}

func hasRunnerOSCondition(condition, operator, osName string) bool {
	for _, part := range strings.Split(normalizeWorkflowCondition(condition), "&&") {
		fields := strings.Fields(strings.TrimSpace(part))
		if len(fields) == 3 && fields[0] == "runner.os" && fields[1] == operator &&
			(fields[2] == "'"+osName+"'" || fields[2] == `"`+osName+`"`) {
			return true
		}
	}
	return false
}

func exactRunnerOSCondition(condition, operator, osName string) bool {
	normalized := normalizeWorkflowCondition(condition)
	return normalized == "runner.os "+operator+" '"+osName+"'" || normalized == `runner.os `+operator+` "`+osName+`"`
}

func windowsOnly(condition string) bool {
	return hasRunnerOSCondition(condition, "==", "Windows")
}

func windowsGoTestCommands(t *testing.T) []workflowCommand {
	t.Helper()
	var tests []workflowCommand
	for _, command := range workflowCommands(ciTestJob(t).Steps) {
		if strings.EqualFold(command.name, "go") && len(command.args) > 0 && command.args[0] == "test" {
			tests = append(tests, command)
		}
	}
	if len(tests) == 0 {
		t.Fatal("CI workflow has no Windows test step")
	}
	return tests
}

func goListPackages(t *testing.T, patterns ...string) []string {
	t.Helper()
	cmd := exec.Command("go", append([]string{"list"}, patterns...)...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list %s: %v", strings.Join(patterns, " "), err)
	}
	var packages []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			packages = append(packages, line)
		}
	}
	slices.Sort(packages)
	return packages
}

func goTestPackagePatterns(command workflowCommand) []string {
	var patterns []string
	for _, arg := range command.args[1:] {
		if strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "@") {
			continue
		}
		patterns = append(patterns, arg)
	}
	return patterns
}

func workflowCommands(steps []wfStep) []workflowCommand {
	return workflowCommandsMatching(steps, func(step wfStep) bool { return windowsOnly(step.If) })
}

func workflowCommandsMatching(steps []wfStep, include func(wfStep) bool) []workflowCommand {
	var commands []workflowCommand
	for stepIndex, step := range steps {
		if !include(step) {
			continue
		}
		for lineIndex, line := range strings.Split(step.Run, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) == 0 || strings.HasPrefix(fields[0], "$p") || strings.ContainsAny(fields[0], "{}()") {
				continue
			}
			commands = append(commands, workflowCommand{step: stepIndex, line: lineIndex, name: fields[0], args: fields[1:]})
		}
	}
	return commands
}

func findWorkflowCommandWithArg(commands []workflowCommand, name, arg string) (workflowCommand, bool) {
	for _, command := range commands {
		if strings.EqualFold(command.name, name) && command.hasArg(arg) {
			return command, true
		}
	}
	return workflowCommand{}, false
}

func (c workflowCommand) before(other workflowCommand) bool {
	return c.step < other.step || c.step == other.step && c.line < other.line
}

func (c workflowCommand) hasArg(want string) bool {
	for _, arg := range c.args {
		if strings.EqualFold(arg, want) {
			return true
		}
	}
	return false
}

func TestCIWorkflow_WindowsTestsRunWithScanExclusions(t *testing.T) {
	t.Parallel()

	job := ciTestJob(t)
	commands := workflowCommands(job.Steps)
	var exclusions []workflowCommand
	for _, option := range []string{"-ExclusionPath", "-ExclusionProcess"} {
		command, ok := findWorkflowCommandWithArg(commands, "Add-MpPreference", option)
		if !ok {
			t.Fatalf("Windows Defender command must apply %s before tests", option)
		}
		if job.Steps[command.step].Shell != "pwsh" {
			t.Fatalf("Defender exclusions must execute with pwsh, got %q", job.Steps[command.step].Shell)
		}
		exclusions = append(exclusions, command)
	}

	tests := windowsGoTestCommands(t)
	for _, test := range tests {
		for _, exclusion := range exclusions {
			if !exclusion.before(test) {
				t.Errorf("Defender exclusion command at step %d line %d must execute before Windows tests at step %d line %d", exclusion.step, exclusion.line, test.step, test.line)
			}
		}
	}
}

func TestCIWorkflow_WindowsHangSurfacesAsGoTimeoutNotJobCancellation(t *testing.T) {
	t.Parallel()

	job := ciTestJob(t)
	if job.TimeoutMinutes != 40 {
		t.Fatalf("test job timeout-minutes = %d, want 40 so a wedged runner cannot burn a full six-hour budget", job.TimeoutMinutes)
	}

	wantWindowsShards := []string{"core", "git", "steps"}
	var matrixShards []string
	for _, row := range job.Strategy.Matrix.Include {
		if row["os"] != "windows-latest" {
			continue
		}
		shard := row["shard"]
		if shard == "" {
			t.Fatalf("windows matrix row %v is missing shard", row)
		}
		matrixShards = append(matrixShards, shard)
	}
	slices.Sort(matrixShards)
	if !slices.Equal(matrixShards, wantWindowsShards) {
		t.Fatalf("windows matrix shards %v, want %v so pipeline/steps runs in parallel with the other git-heavy packages", matrixShards, wantWindowsShards)
	}

	tests := windowsGoTestCommands(t)
	if len(tests) < 3 {
		t.Fatalf("Windows tests must be split across core, git, and steps shards so one ./... job cannot exceed the cap without a binary hitting -timeout, got %d go test invocations", len(tests))
	}

	jobTimeout := time.Duration(job.TimeoutMinutes) * time.Minute
	var explicit []workflowCommand
	var coreCommand workflowCommand
	stepShards := map[string]struct{}{}
	for _, command := range tests {
		goTimeout := goTestTimeout(t, command)
		if goTimeout >= jobTimeout {
			t.Fatalf("go test -timeout is %s and the job cap is %s; the Go timeout must fire first so a hang produces a goroutine dump instead of an evidence-free cancellation", goTimeout, jobTimeout)
		}
		shard := matrixShardCondition(job.Steps[command.step].If)
		if shard == "" {
			t.Fatalf("Windows test step %q is not gated on matrix.shard", job.Steps[command.step].Name)
		}
		stepShards[shard] = struct{}{}
		patterns := goTestPackagePatterns(command)
		switch {
		case slices.Contains(patterns, "./..."):
			t.Fatalf("Windows shard at step %d still runs ./...; a hang in a late package would cancel the job before go test -timeout fires", command.step)
		case len(patterns) > 0:
			explicit = append(explicit, command)
		default:
			if coreCommand.name != "" {
				t.Fatalf("multiple Windows remainder shards; want one go-list remainder")
			}
			coreCommand = command
		}
	}
	if coreCommand.name == "" {
		t.Fatal("Windows tests must keep a go-list remainder shard")
	}
	if len(explicit) != 2 {
		t.Fatalf("Windows tests must list exactly two explicit package shards (git-heavy remainder and pipeline/steps), got %d", len(explicit))
	}
	for _, shard := range wantWindowsShards {
		if _, ok := stepShards[shard]; !ok {
			t.Errorf("windows matrix shard %q has no matching test step", shard)
		}
	}
	for shard := range stepShards {
		if !slices.Contains(wantWindowsShards, shard) {
			t.Errorf("Windows test step shard %q is not in the matrix", shard)
		}
	}

	stepsWant := goListPackages(t, "./internal/pipeline/steps/...")
	var stepsCommand, gitCommand workflowCommand
	var stepsFromArgs, gitFromArgs []string
	for _, command := range explicit {
		pkgs := goListPackages(t, goTestPackagePatterns(command)...)
		if slices.Equal(pkgs, stepsWant) {
			if stepsCommand.name != "" {
				t.Fatal("multiple Windows shards list only pipeline/steps packages")
			}
			stepsCommand = command
			stepsFromArgs = pkgs
			continue
		}
		if gitCommand.name != "" {
			t.Fatalf("extra explicit Windows shard packages %v; want one git-heavy remainder besides pipeline/steps", pkgs)
		}
		gitCommand = command
		gitFromArgs = pkgs
	}
	if stepsCommand.name == "" {
		t.Fatal("Windows tests must run ./internal/pipeline/steps/... on its own shard")
	}
	if gitCommand.name == "" {
		t.Fatal("Windows tests must keep a git-heavy remainder shard besides pipeline/steps")
	}
	if overlap := packagesOverlap(stepsFromArgs, gitFromArgs); len(overlap) > 0 {
		t.Fatalf("git and steps shards must not overlap, also ran %v", overlap)
	}

	exclude := windowsGitExcludePattern(t, job.Steps[coreCommand.step])
	all := goListPackages(t, "./...")
	gitHeavyFromFilter := filterPackages(all, exclude, true)
	coreFromFilter := filterPackages(all, exclude, false)
	gitHeavyFromArgs := append(append([]string{}, gitFromArgs...), stepsFromArgs...)
	slices.Sort(gitHeavyFromArgs)
	if !slices.Equal(gitHeavyFromArgs, gitHeavyFromFilter) {
		t.Fatalf("git+steps shard packages %v do not match NM_CI_WINDOWS_GIT_EXCLUDE %q -> %v", gitHeavyFromArgs, exclude, gitHeavyFromFilter)
	}

	requiredSteps := []string{
		"github.com/kunchenguid/no-mistakes/internal/pipeline/steps",
		"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/citest",
	}
	for _, pkg := range requiredSteps {
		if !slices.Contains(stepsFromArgs, pkg) {
			t.Errorf("steps shard must include %s so CI-monitor tests stay with pipeline/steps", pkg)
		}
		if slices.Contains(gitFromArgs, pkg) {
			t.Errorf("git-heavy remainder must not include %s; that package belongs on the steps shard", pkg)
		}
		if slices.Contains(coreFromFilter, pkg) {
			t.Errorf("core shard must not include git-heavy package %s", pkg)
		}
	}
	requiredGitRest := []string{
		"github.com/kunchenguid/no-mistakes/internal/git",
		"github.com/kunchenguid/no-mistakes/internal/branchsync",
	}
	for _, pkg := range requiredGitRest {
		if !slices.Contains(gitFromArgs, pkg) {
			t.Errorf("git-heavy remainder must include %s, the documented Windows wall floor", pkg)
		}
		if slices.Contains(stepsFromArgs, pkg) {
			t.Errorf("steps shard must not include %s; that package belongs on the git-heavy remainder", pkg)
		}
		if slices.Contains(coreFromFilter, pkg) {
			t.Errorf("core shard must not include git-heavy package %s", pkg)
		}
	}

	var union []string
	union = append(union, gitFromArgs...)
	union = append(union, stepsFromArgs...)
	union = append(union, coreFromFilter...)
	slices.Sort(union)
	if !slices.Equal(union, all) {
		t.Fatalf("Windows shards must cover every package exactly once: union %v, go list ./... %v", union, all)
	}
}

func windowsGitExcludePattern(t *testing.T, step wfStep) *regexp.Regexp {
	t.Helper()
	pattern := step.Env["NM_CI_WINDOWS_GIT_EXCLUDE"]
	if pattern == "" {
		t.Fatal("Windows core shard must set NM_CI_WINDOWS_GIT_EXCLUDE so the remainder filter is a typed contract")
	}
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("NM_CI_WINDOWS_GIT_EXCLUDE %q: %v", pattern, err)
	}
	return compiled
}

func filterPackages(packages []string, exclude *regexp.Regexp, wantMatch bool) []string {
	var out []string
	for _, pkg := range packages {
		if exclude.MatchString(pkg) == wantMatch {
			out = append(out, pkg)
		}
	}
	return out
}

func packagesOverlap(a, b []string) []string {
	seen := make(map[string]struct{}, len(a))
	for _, pkg := range a {
		seen[pkg] = struct{}{}
	}
	var overlap []string
	for _, pkg := range b {
		if _, ok := seen[pkg]; ok {
			overlap = append(overlap, pkg)
		}
	}
	return overlap
}

func matrixShardCondition(ifCond string) string {
	match := regexp.MustCompile(`matrix\.shard\s*==\s*'([^']+)'`).FindStringSubmatch(ifCond)
	if len(match) != 2 {
		return ""
	}
	return match[1]
}

func goTestTimeout(t *testing.T, command workflowCommand) time.Duration {
	t.Helper()
	for i, arg := range command.args[1:] {
		var value string
		switch {
		case strings.HasPrefix(arg, "-timeout="):
			value = strings.TrimPrefix(arg, "-timeout=")
		case arg == "-timeout" && i+2 < len(command.args):
			value = command.args[i+2]
		default:
			continue
		}
		duration, err := time.ParseDuration(value)
		if err != nil {
			t.Fatalf("parse go test -timeout %q: %v", value, err)
		}
		return duration
	}
	t.Fatalf("Windows test command must pass an explicit -timeout, got %#v", command.args)
	return 0
}
