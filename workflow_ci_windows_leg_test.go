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
// scan-exclusion step, a four-way shard split (core remainder, git-heavy
// packages without pipeline/steps, and pipeline/steps divided across two
// shards by a complementary -run/-skip pair) so each job's wall stays inside
// the cap, and a per-binary Go timeout well inside that cap so a genuine hang
// lands as a goroutine dump instead of an opaque job cancellation.
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
	args := command.args[1:]
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-run" || arg == "-skip" || arg == "-timeout" {
			i++ // the flag's value is the next field, not a package pattern
			continue
		}
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

	wantWindowsShards := []string{"core", "git", "steps-a", "steps-b"}
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
		t.Fatalf("windows matrix shards %v, want %v so pipeline/steps runs on two shards in parallel with the other git-heavy packages", matrixShards, wantWindowsShards)
	}

	shards := classifyWindowsTestShards(t, job)

	// Every package runs on exactly one shard, except internal/pipeline/steps,
	// which runs on the two complementary split shards (see
	// TestCIWorkflow_WindowsStepsSplitRunsEveryTestOnce).
	stepsPkg := goListPackages(t, "./internal/pipeline/steps")
	citestPkg := goListPackages(t, "./internal/pipeline/steps/citest")
	var stepsFromArgs []string
	for _, command := range []workflowCommand{shards.splitRun.command, shards.splitSkip.command, shards.stepsRest} {
		for _, pkg := range goListPackages(t, goTestPackagePatterns(command)...) {
			if !slices.Contains(stepsFromArgs, pkg) {
				stepsFromArgs = append(stepsFromArgs, pkg)
			}
		}
	}
	slices.Sort(stepsFromArgs)
	if !slices.Equal(stepsFromArgs, goListPackages(t, "./internal/pipeline/steps/...")) {
		t.Fatalf("steps shards run %v, want every package under internal/pipeline/steps", stepsFromArgs)
	}
	gitFromArgs := goListPackages(t, goTestPackagePatterns(shards.git)...)
	if overlap := packagesOverlap(stepsFromArgs, gitFromArgs); len(overlap) > 0 {
		t.Fatalf("git and steps shards must not overlap, also ran %v", overlap)
	}

	exclude := windowsGitExcludePattern(t, job.Steps[shards.core.step])
	all := goListPackages(t, "./...")
	gitHeavyFromFilter := filterPackages(all, exclude, true)
	coreFromFilter := filterPackages(all, exclude, false)
	gitHeavyFromArgs := append(append([]string{}, gitFromArgs...), stepsFromArgs...)
	slices.Sort(gitHeavyFromArgs)
	if !slices.Equal(gitHeavyFromArgs, gitHeavyFromFilter) {
		t.Fatalf("git+steps shard packages %v do not match NM_CI_WINDOWS_GIT_EXCLUDE %q -> %v", gitHeavyFromArgs, exclude, gitHeavyFromFilter)
	}

	stepsShard := func(command workflowCommand) string {
		return matrixShardCondition(job.Steps[command.step].If)
	}
	if shard := stepsShard(shards.stepsRest); shard != "steps-a" && shard != "steps-b" {
		t.Errorf("internal/pipeline/steps subpackages (including citest) run on shard %q; CI-monitor tests stay with pipeline/steps", shard)
	}
	for _, pkg := range append(append([]string{}, stepsPkg...), citestPkg...) {
		if slices.Contains(gitFromArgs, pkg) {
			t.Errorf("git-heavy remainder must not include %s; that package belongs on the steps shards", pkg)
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
			t.Errorf("steps shards must not include %s; that package belongs on the git-heavy remainder", pkg)
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

// TestCIWorkflow_WindowsStepsSplitRunsEveryTestOnce pins how
// internal/pipeline/steps is divided between two Windows shards (#1580). Go
// documents -skip as the inverse of -run, so one shard running `-run R` and the
// other `-skip R` over the same package, with the same R and no other filter,
// runs each top-level test on exactly one shard: a renamed or new test can move
// between shards but can never fall out of CI.
func TestCIWorkflow_WindowsStepsSplitRunsEveryTestOnce(t *testing.T) {
	t.Parallel()

	job := ciTestJob(t)
	shards := classifyWindowsTestShards(t, job)

	runPattern := shards.splitRun.value
	skipPattern := shards.splitSkip.value
	if runPattern != skipPattern {
		t.Fatalf("steps split shards use -run %q and -skip %q; they must use the same pattern or some tests run twice or never", runPattern, skipPattern)
	}
	if strings.TrimSpace(runPattern) == "" {
		t.Fatal("steps split pattern is empty; -run would select every test and -skip none")
	}
	if strings.Contains(runPattern, "/") {
		t.Fatalf("steps split pattern %q contains '/', which filters subtests; split on top-level test names only", runPattern)
	}
	if _, err := regexp.Compile(runPattern); err != nil {
		t.Fatalf("steps split pattern %q does not compile: %v", runPattern, err)
	}

	runShard := matrixShardCondition(job.Steps[shards.splitRun.command.step].If)
	skipShard := matrixShardCondition(job.Steps[shards.splitSkip.command.step].If)
	if runShard == skipShard {
		t.Fatalf("both halves of the steps split run on shard %q; the split only helps when the halves run on separate runners", runShard)
	}
}

type windowsTestFilter struct {
	command workflowCommand
	kind    string // "run" or "skip"
	value   string
}

type windowsTestShards struct {
	core      workflowCommand
	git       workflowCommand
	stepsRest workflowCommand // every package under pipeline/steps except steps itself
	splitRun  windowsTestFilter
	splitSkip windowsTestFilter
}

// classifyWindowsTestShards checks the properties every Windows go test
// invocation shares and sorts the invocations into the shard roles.
func classifyWindowsTestShards(t *testing.T, job *wfJob) windowsTestShards {
	t.Helper()
	tests := windowsGoTestCommands(t)
	jobTimeout := time.Duration(job.TimeoutMinutes) * time.Minute
	stepsPkg := goListPackages(t, "./internal/pipeline/steps")
	var stepsRestPkgs []string
	for _, pkg := range goListPackages(t, "./internal/pipeline/steps/...") {
		if !slices.Contains(stepsPkg, pkg) {
			stepsRestPkgs = append(stepsRestPkgs, pkg)
		}
	}

	var shards windowsTestShards
	perStep := map[int]int{}
	stepShards := map[string]struct{}{}
	for _, command := range tests {
		step := job.Steps[command.step]
		goTimeout := goTestTimeout(t, command)
		if goTimeout >= jobTimeout {
			t.Fatalf("go test -timeout is %s and the job cap is %s; the Go timeout must fire first so a hang produces a goroutine dump instead of an evidence-free cancellation", goTimeout, jobTimeout)
		}
		perStep[command.step]++
		if perStep[command.step] > 1 {
			t.Fatalf("Windows step %q runs more than one go test; pwsh reports only the last exit code, so an earlier failure would be masked", step.Name)
		}
		shard := matrixShardCondition(step.If)
		if shard == "" {
			t.Fatalf("Windows test step %q is not gated on matrix.shard", step.Name)
		}
		stepShards[shard] = struct{}{}

		patterns := goTestPackagePatterns(command)
		if slices.Contains(patterns, "./...") {
			t.Fatalf("Windows shard at step %d still runs ./...; a hang in a late package would cancel the job before go test -timeout fires", command.step)
		}
		filter, hasFilter := goTestFilter(t, job, step, command)
		if len(patterns) == 0 {
			if hasFilter {
				t.Fatalf("Windows remainder shard %q must not filter tests", step.Name)
			}
			if shards.core.name != "" {
				t.Fatal("multiple Windows remainder shards; want one go-list remainder")
			}
			shards.core = command
			continue
		}

		pkgs := goListPackages(t, patterns...)
		switch {
		case hasFilter:
			if !slices.Equal(pkgs, stepsPkg) {
				t.Fatalf("Windows step %q filters tests with -%s over %v; only internal/pipeline/steps is split, and a filter over another package would also drop that package's tests", step.Name, filter.kind, pkgs)
			}
			target := &shards.splitRun
			if filter.kind == "skip" {
				target = &shards.splitSkip
			}
			if target.kind != "" {
				t.Fatalf("multiple Windows steps split internal/pipeline/steps with -%s", filter.kind)
			}
			*target = filter
		case slices.Equal(pkgs, stepsRestPkgs):
			if shards.stepsRest.name != "" {
				t.Fatal("multiple Windows steps run the internal/pipeline/steps subpackages")
			}
			shards.stepsRest = command
		case slices.Equal(pkgs, stepsPkg):
			t.Fatalf("Windows step %q runs all of internal/pipeline/steps unfiltered; it must be split with -run/-skip across the steps shards", step.Name)
		default:
			if shards.git.name != "" {
				t.Fatalf("extra explicit Windows shard packages %v; want one git-heavy remainder besides pipeline/steps", pkgs)
			}
			shards.git = command
		}
	}

	if shards.core.name == "" {
		t.Fatal("Windows tests must keep a go-list remainder shard")
	}
	if shards.git.name == "" {
		t.Fatal("Windows tests must keep a git-heavy remainder shard besides pipeline/steps")
	}
	if shards.stepsRest.name == "" {
		t.Fatalf("Windows tests must run every package under internal/pipeline/steps except steps itself (%v) in one unfiltered step", stepsRestPkgs)
	}
	if shards.splitRun.kind == "" || shards.splitSkip.kind == "" {
		t.Fatal("Windows tests must split internal/pipeline/steps into a -run half and a -skip half")
	}
	wantShards := []string{"core", "git", "steps-a", "steps-b"}
	for _, shard := range wantShards {
		if _, ok := stepShards[shard]; !ok {
			t.Errorf("windows matrix shard %q has no matching test step", shard)
		}
	}
	for shard := range stepShards {
		if !slices.Contains(wantShards, shard) {
			t.Errorf("Windows test step shard %q is not in the matrix", shard)
		}
	}
	return shards
}

// goTestFilter returns the command's -run or -skip filter with any
// $env:NAME reference resolved from the step's, then the job's, env. A
// command may carry at most one filter.
func goTestFilter(t *testing.T, job *wfJob, step wfStep, command workflowCommand) (windowsTestFilter, bool) {
	t.Helper()
	var found []windowsTestFilter
	args := command.args[1:]
	for i := 0; i < len(args); i++ {
		for _, kind := range []string{"run", "skip"} {
			var value string
			switch {
			case strings.HasPrefix(args[i], "-"+kind+"="):
				value = strings.TrimPrefix(args[i], "-"+kind+"=")
			case args[i] == "-"+kind && i+1 < len(args):
				i++
				value = args[i]
			default:
				continue
			}
			found = append(found, windowsTestFilter{command: command, kind: kind, value: resolveWorkflowValue(t, job, step, value)})
		}
	}
	switch len(found) {
	case 0:
		return windowsTestFilter{}, false
	case 1:
		return found[0], true
	default:
		t.Fatalf("Windows step %q combines test filters %v; the steps split needs exactly one -run or -skip", step.Name, found)
		return windowsTestFilter{}, false
	}
}

func resolveWorkflowValue(t *testing.T, job *wfJob, step wfStep, value string) string {
	t.Helper()
	if len(value) >= 2 && (value[0] == '\'' && value[len(value)-1] == '\'' || value[0] == '"' && value[len(value)-1] == '"') {
		value = value[1 : len(value)-1]
	}
	name, ok := strings.CutPrefix(value, "$env:")
	if !ok {
		return value
	}
	if step.Shell != "pwsh" {
		t.Fatalf("Windows step %q reads $env:%s, which only pwsh expands; set shell: pwsh", step.Name, name)
	}
	if resolved, ok := step.Env[name]; ok {
		return resolved
	}
	if resolved, ok := job.Env[name]; ok {
		return resolved
	}
	t.Fatalf("Windows step %q reads $env:%s, which neither the step nor the job defines", step.Name, name)
	return ""
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
	fields := strings.Fields(normalizeWorkflowCondition(ifCond))
	if len(fields) != 7 || fields[0] != "runner.os" || fields[1] != "==" || fields[2] != "'Windows'" ||
		fields[3] != "&&" || fields[4] != "matrix.shard" || fields[5] != "==" {
		return ""
	}
	quotedShard := fields[6]
	if len(quotedShard) < 3 || quotedShard[0] != '\'' || quotedShard[len(quotedShard)-1] != '\'' || strings.Contains(quotedShard[1:len(quotedShard)-1], "'") {
		return ""
	}
	return quotedShard[1 : len(quotedShard)-1]
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
