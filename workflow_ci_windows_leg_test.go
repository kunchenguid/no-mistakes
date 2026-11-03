package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
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
// scan-exclusion step, a shard split (core remainder, git-heavy packages
// without pipeline/steps/daemon/gate, pipeline/steps alone, daemon alone, and
// gate alone, with steps/daemon/gate each split by test name across matrix
// rows) so each job's wall stays inside the cap, and a per-binary Go timeout
// well inside that cap so a genuine hang lands as a goroutine dump instead of
// an opaque job cancellation.
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
		arg = unquoteWorkflowArg(arg)
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

	wantWindowsShards := []string{"core", "daemon", "gate", "git", "steps"}
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
	matrixShards = slices.Compact(matrixShards)
	if !slices.Equal(matrixShards, wantWindowsShards) {
		t.Fatalf("windows matrix shards %v, want %v so daemon, gate, and pipeline/steps run in parallel with the other git-heavy packages", matrixShards, wantWindowsShards)
	}

	tests := windowsGoTestCommands(t)
	if len(tests) < 5 {
		t.Fatalf("Windows tests must be split across core, git, steps, daemon, and gate shards so one ./... job cannot exceed the cap without a binary hitting -timeout, got %d go test invocations", len(tests))
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
	if len(explicit) != 4 {
		t.Fatalf("Windows tests must list exactly four explicit package shards (git-heavy remainder, pipeline/steps, daemon, and gate), got %d", len(explicit))
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
	daemonWant := goListPackages(t, "./internal/daemon")
	gateWant := goListPackages(t, "./internal/gate")
	var stepsCommand, gitCommand, daemonCommand, gateCommand workflowCommand
	var stepsFromArgs, gitFromArgs, daemonFromArgs, gateFromArgs []string
	for _, command := range explicit {
		pkgs := goListPackages(t, goTestPackagePatterns(command)...)
		switch {
		case slices.Equal(pkgs, stepsWant):
			if stepsCommand.name != "" {
				t.Fatal("multiple Windows shards list only pipeline/steps packages")
			}
			stepsCommand = command
			stepsFromArgs = pkgs
		case slices.Equal(pkgs, daemonWant):
			if daemonCommand.name != "" {
				t.Fatal("multiple Windows shards list only internal/daemon")
			}
			daemonCommand = command
			daemonFromArgs = pkgs
		case slices.Equal(pkgs, gateWant):
			if gateCommand.name != "" {
				t.Fatal("multiple Windows shards list only internal/gate")
			}
			gateCommand = command
			gateFromArgs = pkgs
		default:
			if gitCommand.name != "" {
				t.Fatalf("extra explicit Windows shard packages %v; want one git-heavy remainder besides steps, daemon, and gate", pkgs)
			}
			gitCommand = command
			gitFromArgs = pkgs
		}
	}
	if stepsCommand.name == "" {
		t.Fatal("Windows tests must run ./internal/pipeline/steps/... on its own shard")
	}
	if daemonCommand.name == "" {
		t.Fatal("Windows tests must run ./internal/daemon on its own shard")
	}
	if gateCommand.name == "" {
		t.Fatal("Windows tests must run ./internal/gate on its own shard")
	}
	if gitCommand.name == "" {
		t.Fatal("Windows tests must keep a git-heavy remainder shard besides steps, daemon, and gate")
	}
	explicitSets := [][]string{stepsFromArgs, daemonFromArgs, gateFromArgs, gitFromArgs}
	for i, a := range explicitSets {
		for _, b := range explicitSets[i+1:] {
			if overlap := packagesOverlap(a, b); len(overlap) > 0 {
				t.Fatalf("explicit Windows shards must not overlap, also ran %v", overlap)
			}
		}
	}

	exclude := windowsGitExcludePattern(t, job.Steps[coreCommand.step])
	all := goListPackages(t, "./...")
	gitHeavyFromFilter := filterPackages(all, exclude, true)
	coreFromFilter := filterPackages(all, exclude, false)
	gitHeavyFromArgs := append(append([]string{}, gitFromArgs...), stepsFromArgs...)
	gitHeavyFromArgs = append(gitHeavyFromArgs, daemonFromArgs...)
	gitHeavyFromArgs = append(gitHeavyFromArgs, gateFromArgs...)
	slices.Sort(gitHeavyFromArgs)
	if !slices.Equal(gitHeavyFromArgs, gitHeavyFromFilter) {
		t.Fatalf("git+steps+daemon+gate shard packages %v do not match NM_CI_WINDOWS_GIT_EXCLUDE %q -> %v", gitHeavyFromArgs, exclude, gitHeavyFromFilter)
	}

	requiredOn := []struct {
		owner string
		pkgs  []string
		have  []string
	}{
		{"steps", []string{
			"github.com/kunchenguid/no-mistakes/internal/pipeline/steps",
			"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/citest",
		}, stepsFromArgs},
		{"daemon", []string{
			"github.com/kunchenguid/no-mistakes/internal/daemon",
		}, daemonFromArgs},
		{"gate", []string{
			"github.com/kunchenguid/no-mistakes/internal/gate",
		}, gateFromArgs},
		{"git-heavy remainder", []string{
			"github.com/kunchenguid/no-mistakes/internal/git",
			"github.com/kunchenguid/no-mistakes/internal/branchsync",
		}, gitFromArgs},
	}
	allOwned := map[string]string{}
	for _, group := range requiredOn {
		for _, pkg := range group.pkgs {
			if !slices.Contains(group.have, pkg) {
				t.Errorf("%s shard must include %s", group.owner, pkg)
			}
			if slices.Contains(coreFromFilter, pkg) {
				t.Errorf("core shard must not include git-heavy package %s", pkg)
			}
			allOwned[pkg] = group.owner
		}
	}
	ownedSets := []struct {
		name string
		pkgs []string
	}{
		{"git-heavy remainder", gitFromArgs},
		{"steps", stepsFromArgs},
		{"daemon", daemonFromArgs},
		{"gate", gateFromArgs},
	}
	for pkg, owner := range allOwned {
		for _, set := range ownedSets {
			if set.name == owner {
				continue
			}
			if slices.Contains(set.pkgs, pkg) {
				t.Errorf("%s shard must not include %s; that package belongs on the %s shard", set.name, pkg, owner)
			}
		}
	}

	var union []string
	union = append(union, gitFromArgs...)
	union = append(union, stepsFromArgs...)
	union = append(union, daemonFromArgs...)
	union = append(union, gateFromArgs...)
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

func unquoteWorkflowArg(arg string) string {
	if len(arg) >= 2 && (arg[0] == '"' || arg[0] == '\'') && arg[len(arg)-1] == arg[0] {
		return arg[1 : len(arg)-1]
	}
	return arg
}

type namedTestShard struct {
	name string
	run  *regexp.Regexp
	skip *regexp.Regexp
}

func (s namedTestShard) selects(test string) bool {
	return (s.run == nil || s.run.MatchString(test)) && (s.skip == nil || !s.skip.MatchString(test))
}

type windowsNamedShardSpec struct {
	kind    string
	dir     string
	runKey  string
	skipKey string
	runEnv  string
	skipEnv string
}

func TestCIWorkflow_WindowsStepsShardsRunEveryTestExactlyOnce(t *testing.T) {
	t.Parallel()
	assertWindowsNamedShardsCoverEveryTest(t, windowsNamedShardSpec{
		kind:    "steps",
		dir:     filepath.Join("internal", "pipeline", "steps"),
		runKey:  "steps_run",
		skipKey: "steps_skip",
		runEnv:  "NM_CI_WINDOWS_STEPS_RUN",
		skipEnv: "NM_CI_WINDOWS_STEPS_SKIP",
	})
}

func TestCIWorkflow_WindowsDaemonShardsRunEveryTestExactlyOnce(t *testing.T) {
	t.Parallel()
	assertWindowsNamedShardsCoverEveryTest(t, windowsNamedShardSpec{
		kind:    "daemon",
		dir:     filepath.Join("internal", "daemon"),
		runKey:  "daemon_run",
		skipKey: "daemon_skip",
		runEnv:  "NM_CI_WINDOWS_DAEMON_RUN",
		skipEnv: "NM_CI_WINDOWS_DAEMON_SKIP",
	})
}

func TestCIWorkflow_WindowsGateShardsRunEveryTestExactlyOnce(t *testing.T) {
	t.Parallel()
	assertWindowsNamedShardsCoverEveryTest(t, windowsNamedShardSpec{
		kind:    "gate",
		dir:     filepath.Join("internal", "gate"),
		runKey:  "gate_run",
		skipKey: "gate_skip",
		runEnv:  "NM_CI_WINDOWS_GATE_RUN",
		skipEnv: "NM_CI_WINDOWS_GATE_SKIP",
	})
}

func assertWindowsNamedShardsCoverEveryTest(t *testing.T, spec windowsNamedShardSpec) {
	t.Helper()

	job := ciTestJob(t)
	var shards []namedTestShard
	for _, row := range job.Strategy.Matrix.Include {
		if row["os"] != "windows-latest" || row["shard"] != spec.kind {
			continue
		}
		shards = append(shards, namedTestShard{
			name: row["name"],
			run:  namedShardPattern(t, row, spec.runKey),
			skip: namedShardPattern(t, row, spec.skipKey),
		})
	}
	if len(shards) < 2 {
		t.Fatalf("%s runs on %d Windows matrix rows; want it split across more than one so each go test binary stays well inside -timeout", spec.kind, len(shards))
	}

	var commands []workflowCommand
	for _, command := range windowsGoTestCommands(t) {
		if matrixShardCondition(job.Steps[command.step].If) == spec.kind {
			commands = append(commands, command)
		}
	}
	if len(commands) != 1 {
		t.Fatalf("want one Windows go test command for the %s rows, got %d", spec.kind, len(commands))
	}
	command := commands[0]
	step := job.Steps[command.step]
	if step.Shell != "pwsh" {
		t.Fatalf("%s shard step must run with pwsh so $env: expands, got shell %q", spec.kind, step.Shell)
	}
	for flag, wiring := range map[string]struct{ env, matrix string }{
		"-run":  {spec.runEnv, spec.runKey},
		"-skip": {spec.skipEnv, spec.skipKey},
	} {
		if got, want := step.Env[wiring.env], "${{ matrix."+wiring.matrix+" }}"; got != want {
			t.Errorf("%s shard env %s = %q, want %q", spec.kind, wiring.env, got, want)
		}
		arg := flag + "=$env:" + wiring.env
		if !slices.ContainsFunc(command.args, func(a string) bool { return unquoteWorkflowArg(a) == arg }) {
			t.Errorf("%s shard go test must pass %q so the matrix partition reaches the test binary, got %v", spec.kind, arg, command.args)
		}
	}

	names := append(declaredTestNames(t, spec.dir), "Test", "Test_Probe")
	for c := 'A'; c <= 'Z'; c++ {
		names = append(names, "Test"+string(c)+"Probe")
	}
	for c := '0'; c <= '9'; c++ {
		names = append(names, "Test"+string(c)+"Probe")
	}
	for _, name := range names {
		var selectedBy []string
		for _, shard := range shards {
			if shard.selects(name) {
				selectedBy = append(selectedBy, shard.name)
			}
		}
		if len(selectedBy) != 1 {
			t.Errorf("%s is selected by Windows %s shards %v; every test must run in exactly one", name, spec.kind, selectedBy)
		}
	}
}

func namedShardPattern(t *testing.T, row map[string]string, key string) *regexp.Regexp {
	t.Helper()
	pattern := row[key]
	if pattern == "" {
		return nil
	}
	if strings.Contains(pattern, "/") {
		t.Fatalf("matrix row %q %s %q contains /, which go test splits into subtest levels", row["name"], key, pattern)
	}
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("matrix row %q %s %q: %v", row["name"], key, pattern, err)
	}
	return compiled
}

func declaredTestNames(t *testing.T, root string) []string {
	t.Helper()
	var names []string
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return err
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Recv == nil && isGoTestName(fn.Name.Name) {
				names = append(names, fn.Name.Name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("collect test names under %s: %v", root, err)
	}
	if len(names) == 0 {
		t.Fatalf("found no tests under %s", root)
	}
	return names
}

func isGoTestName(name string) bool {
	for _, prefix := range []string{"Test", "Fuzz", "Example"} {
		rest, ok := strings.CutPrefix(name, prefix)
		if ok && (rest == "" || !(rest[0] >= 'a' && rest[0] <= 'z')) {
			return name != "TestMain"
		}
	}
	return false
}
