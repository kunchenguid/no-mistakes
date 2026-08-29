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
// scan-exclusion step, a git-heavy/core shard split so each job's wall stays
// inside the cap, and a per-binary Go timeout well inside that cap so a genuine
// hang lands as a goroutine dump instead of an opaque job cancellation.
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

func windowsOnly(condition string) bool {
	condition = strings.TrimSpace(condition)
	condition = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(condition, "${{"), "}}"))
	for _, part := range strings.Split(condition, "&&") {
		fields := strings.Fields(strings.TrimSpace(part))
		if len(fields) == 3 && fields[0] == "runner.os" && fields[1] == "==" &&
			(fields[2] == "'Windows'" || fields[2] == `"Windows"`) {
			return true
		}
	}
	return false
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
	var commands []workflowCommand
	for stepIndex, step := range steps {
		if !windowsOnly(step.If) {
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

	tests := windowsGoTestCommands(t)
	if len(tests) < 2 {
		t.Fatalf("Windows tests must be split across shards so one ./... job cannot exceed the cap without a binary hitting -timeout, got %d go test invocations", len(tests))
	}

	jobTimeout := time.Duration(job.TimeoutMinutes) * time.Minute
	var gitCommand workflowCommand
	var coreCommand workflowCommand
	for _, command := range tests {
		goTimeout := goTestTimeout(t, command)
		if goTimeout >= jobTimeout {
			t.Fatalf("go test -timeout is %s and the job cap is %s; the Go timeout must fire first so a hang produces a goroutine dump instead of an evidence-free cancellation", goTimeout, jobTimeout)
		}
		patterns := goTestPackagePatterns(command)
		switch {
		case slices.Contains(patterns, "./..."):
			t.Fatalf("Windows shard at step %d still runs ./...; a hang in a late package would cancel the job before go test -timeout fires", command.step)
		case len(patterns) > 0:
			if gitCommand.name != "" {
				t.Fatalf("multiple Windows shards list explicit packages; want one git-heavy shard and one go-list remainder")
			}
			gitCommand = command
		default:
			if coreCommand.name != "" {
				t.Fatalf("multiple Windows remainder shards; want one git-heavy shard and one go-list remainder")
			}
			coreCommand = command
		}
	}
	if gitCommand.name == "" || coreCommand.name == "" {
		t.Fatal("Windows tests must split git-heavy packages from the remainder")
	}

	exclude := windowsGitExcludePattern(t, job.Steps[coreCommand.step])
	all := goListPackages(t, "./...")
	gitFromFilter := filterPackages(all, exclude, true)
	coreFromFilter := filterPackages(all, exclude, false)
	gitFromArgs := goListPackages(t, goTestPackagePatterns(gitCommand)...)
	if !slices.Equal(gitFromArgs, gitFromFilter) {
		t.Fatalf("git-heavy shard packages %v do not match NM_CI_WINDOWS_GIT_EXCLUDE %q -> %v", gitFromArgs, exclude, gitFromFilter)
	}

	requiredGitHeavy := []string{
		"github.com/kunchenguid/no-mistakes/internal/git",
		"github.com/kunchenguid/no-mistakes/internal/branchsync",
		"github.com/kunchenguid/no-mistakes/internal/pipeline/steps",
		"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/citest",
	}
	for _, pkg := range requiredGitHeavy {
		if !slices.Contains(gitFromFilter, pkg) {
			t.Errorf("git-heavy shard must include %s, the documented Windows wall floor", pkg)
		}
		if slices.Contains(coreFromFilter, pkg) {
			t.Errorf("core shard must not include git-heavy package %s", pkg)
		}
	}

	var union []string
	union = append(union, gitFromFilter...)
	union = append(union, coreFromFilter...)
	slices.Sort(union)
	if !slices.Equal(union, all) {
		t.Fatalf("Windows shards must cover every package: union %v, go list ./... %v", union, all)
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
