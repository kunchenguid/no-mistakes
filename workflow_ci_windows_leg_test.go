package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// The Windows test leg is process-spawn bound: the git-backed packages run
// thousands of git.exe invocations, and Defender real-time scanning taxes every
// one. Untuned, the job ran 10-25 minutes against its 25-minute cap and was
// cancelled outright on real PRs. These tests pin the two properties that keep
// that from silently coming back - the scan-exclusion step, and a per-binary Go
// timeout well inside the job cap so a genuine hang lands as a goroutine dump
// instead of an opaque job cancellation with no evidence.
//
// The workflow cannot be exercised from `go test` (it needs a Windows runner),
// so it is asserted through a typed workflow and normalized command view.

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
	fields := strings.Fields(condition)
	return len(fields) == 3 && fields[0] == "runner.os" && fields[1] == "==" &&
		(fields[2] == "'Windows'" || fields[2] == `"Windows"`)
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

func findWorkflowInvocation(commands []workflowCommand, name, firstArg string) (workflowCommand, bool) {
	for _, command := range commands {
		if strings.EqualFold(command.name, name) && len(command.args) > 0 && command.args[0] == firstArg {
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

	tests, ok := findWorkflowInvocation(commands, "go", "test")
	if !ok {
		t.Fatal("CI workflow has no Windows test step")
	}
	for _, exclusion := range exclusions {
		if !exclusion.before(tests) {
			t.Errorf("Defender exclusion command at step %d line %d must execute before Windows tests at step %d line %d", exclusion.step, exclusion.line, tests.step, tests.line)
		}
	}
}

func TestCIWorkflow_WindowsHangSurfacesAsGoTimeoutNotJobCancellation(t *testing.T) {
	t.Parallel()

	job := ciTestJob(t)
	if job.TimeoutMinutes <= 0 {
		t.Fatal("test job must set timeout-minutes so a wedged runner cannot burn a full six-hour budget")
	}

	tests, ok := findWorkflowInvocation(workflowCommands(job.Steps), "go", "test")
	if !ok {
		t.Fatal("CI workflow has no Windows test step")
	}
	goTimeout := goTestTimeout(t, tests)
	jobTimeout := time.Duration(job.TimeoutMinutes) * time.Minute
	if goTimeout >= jobTimeout {
		t.Fatalf("go test -timeout is %s and the job cap is %s; the Go timeout must fire first so a hang produces a goroutine dump instead of an evidence-free cancellation", goTimeout, jobTimeout)
	}
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
