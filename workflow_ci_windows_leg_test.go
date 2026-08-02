package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

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
// so it is asserted through a typed view of the declarative file it owns.

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

// windowsStepIndex reports the index of the first step gated to Windows whose
// run body contains want, or -1.
func windowsStepIndex(steps []wfStep, want string) int {
	for i, step := range steps {
		if !strings.Contains(step.If, "runner.os == 'Windows'") {
			continue
		}
		if strings.Contains(step.Run, want) {
			return i
		}
	}
	return -1
}

func TestCIWorkflow_WindowsTestsRunWithScanExclusions(t *testing.T) {
	t.Parallel()

	steps := ciTestJob(t).Steps

	exclusions := windowsStepIndex(steps, "Add-MpPreference")
	if exclusions < 0 {
		t.Fatal("Windows test leg must exclude its build and test trees from Defender scanning; without it the job overruns timeout-minutes and is cancelled")
	}
	for _, want := range []string{"-ExclusionPath", "-ExclusionProcess"} {
		if !strings.Contains(steps[exclusions].Run, want) {
			t.Errorf("Defender exclusion step missing %s; both the ephemeral trees and the spawned toolchain processes must be excluded", want)
		}
	}

	tests := windowsStepIndex(steps, "go test")
	if tests < 0 {
		t.Fatal("CI workflow has no Windows test step")
	}
	if exclusions > tests {
		t.Errorf("Defender exclusion step is at index %d, after the Windows test step at %d; exclusions must be applied before the suite runs", exclusions, tests)
	}
}

func TestCIWorkflow_WindowsHangSurfacesAsGoTimeoutNotJobCancellation(t *testing.T) {
	t.Parallel()

	job := ciTestJob(t)
	if job.TimeoutMinutes <= 0 {
		t.Fatal("test job must set timeout-minutes so a wedged runner cannot burn a full six-hour budget")
	}

	tests := windowsStepIndex(job.Steps, "go test")
	if tests < 0 {
		t.Fatal("CI workflow has no Windows test step")
	}
	goTimeout := goTestTimeoutMinutes(t, job.Steps[tests].Run)
	if goTimeout >= job.TimeoutMinutes {
		t.Fatalf("go test -timeout is %dm and the job cap is %dm; the Go timeout must fire first so a hang produces a goroutine dump instead of an evidence-free cancellation", goTimeout, job.TimeoutMinutes)
	}
}

var goTestTimeoutPattern = regexp.MustCompile(`-timeout[= ]([0-9]+)m\b`)

func goTestTimeoutMinutes(t *testing.T, run string) int {
	t.Helper()
	match := goTestTimeoutPattern.FindStringSubmatch(run)
	if match == nil {
		t.Fatalf("Windows test step must pass an explicit -timeout in minutes, got %q", run)
	}
	minutes, err := strconv.Atoi(match[1])
	if err != nil {
		t.Fatalf("parse -timeout from %q: %v", run, err)
	}
	return minutes
}
