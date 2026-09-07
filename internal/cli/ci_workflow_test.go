package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// initGitRepo makes dir a minimal git repository so generateCIWorkflow's
// git.FindGitRoot resolution (added to root the command at the repo
// toplevel) succeeds against it.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "init", "--quiet", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
}

func TestGenerateCIWorkflow(t *testing.T) {
	tests := []struct {
		name       string
		configYAML string
		force      bool
		wantErr    bool
		validate   func(*testing.T, map[string]interface{})
		setup      func(dir string) error
	}{
		{
			name: "basic go repo with lint and test",
			configYAML: `commands:
  lint: "go vet ./... && gofmt -l ."
  test: "go test ./... -race"
`,
			wantErr: false,
			validate: func(t *testing.T, workflow map[string]interface{}) {
				t.Helper()
				verifyWorkflowName(t, workflow, "CI")
				verifyJobsExist(t, workflow)
				verifyStepNames(t, workflow, []string{"Lint", "Test"})
			},
		},
		{
			name: "empty lint (combined document+lint)",
			configYAML: `commands:
  test: "go test ./... -race"
`,
			wantErr: false,
			validate: func(t *testing.T, workflow map[string]interface{}) {
				t.Helper()
				verifyWorkflowName(t, workflow, "CI")
				verifyJobsExist(t, workflow)
				verifyStepNames(t, workflow, []string{"Test"})
				verifyStepNotPresent(t, workflow, "Lint")
			},
		},
		{
			name: "missing test command",
			configYAML: `commands:
  lint: "go vet ./..."
`,
			wantErr: true,
		},
		{
			name:       "empty config",
			configYAML: ``,
			wantErr:    true,
		},
		{
			name: "file exists, no force",
			configYAML: `commands:
  lint: "go vet ./..."
  test: "go test ./..."
`,
			wantErr: true,
			setup: func(dir string) error {
				workflowDir := filepath.Join(dir, ".github", "workflows")
				if err := os.MkdirAll(workflowDir, 0755); err != nil {
					return err
				}
				return os.WriteFile(
					filepath.Join(workflowDir, "ci.yml"),
					[]byte("existing"),
					0644,
				)
			},
		},
		{
			name: "file exists, force overwrite",
			configYAML: `commands:
  lint: "go vet ./..."
  test: "go test ./... -race"
`,
			force:   true,
			wantErr: false,
			validate: func(t *testing.T, workflow map[string]interface{}) {
				t.Helper()
				verifyWorkflowName(t, workflow, "CI")
			},
		},
		{
			name: "commands with special yaml characters",
			configYAML: `commands:
  lint: "go vet ./... && { test -z \"$(gofmt -l .)\" || exit 1; }"
  test: "go test ./... -race -count=1"
`,
			wantErr: false,
			validate: func(t *testing.T, workflow map[string]interface{}) {
				t.Helper()
				verifyStepRunContains(t, workflow, "Lint", "gofmt")
				verifyStepRunContains(t, workflow, "Test", "-race")
			},
		},
		{
			name: "multi-line test command stays inside the block scalar",
			configYAML: `commands:
  lint: "go vet ./..."
  test: |-
    go build ./...
    go test ./... -race
`,
			wantErr: false,
			validate: func(t *testing.T, workflow map[string]interface{}) {
				t.Helper()
				verifyStepRunContains(t, workflow, "Test", "go build ./...")
				verifyStepRunContains(t, workflow, "Test", "go test ./... -race")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			initGitRepo(t, dir)

			// Setup
			if tt.setup != nil {
				if err := tt.setup(dir); err != nil {
					t.Fatalf("setup failed: %v", err)
				}
			}

			// Write config file
			if err := os.WriteFile(
				filepath.Join(dir, ".no-mistakes.yaml"),
				[]byte(tt.configYAML),
				0644,
			); err != nil {
				t.Fatalf("write config: %v", err)
			}

			// Run
			err := generateCIWorkflow(dir, tt.force)

			// Check error
			if (err != nil) != tt.wantErr {
				t.Errorf("generateCIWorkflow() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr {
				return
			}

			// Check output file exists and is valid YAML
			workflowPath := filepath.Join(dir, ".github", "workflows", "ci.yml")
			content, err := os.ReadFile(workflowPath)
			if err != nil {
				t.Errorf("workflow file not created: %v", err)
				return
			}

			// Parse YAML
			var workflow map[string]interface{}
			if err := yaml.Unmarshal(content, &workflow); err != nil {
				t.Errorf("workflow is not valid YAML: %v", err)
				return
			}

			// Run validation
			if tt.validate != nil {
				tt.validate(t, workflow)
			}
		})
	}
}

func TestGenerateCIWorkflow_RootsAtGitToplevel(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	if err := os.WriteFile(
		filepath.Join(dir, ".no-mistakes.yaml"),
		[]byte("commands:\n  test: \"go test ./...\"\n"),
		0644,
	); err != nil {
		t.Fatalf("write config: %v", err)
	}

	subdir := filepath.Join(dir, "cmd", "sub")
	if err := os.MkdirAll(subdir, 0755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}

	// Run from a subdirectory: the workflow must land at the repo root's
	// .github/workflows, not under the subdirectory, so GitHub discovers it.
	if err := generateCIWorkflow(subdir, false); err != nil {
		t.Fatalf("generateCIWorkflow() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, ".github", "workflows", "ci.yml")); err != nil {
		t.Errorf("workflow not created at repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(subdir, ".github")); !os.IsNotExist(err) {
		t.Errorf("workflow should not be created under the subdirectory, stat err = %v", err)
	}
}

func TestGenerateCIWorkflow_RefusesSymlinkedWorkflowsDir(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".github"), 0755); err != nil {
		t.Fatalf("mkdir .github: %v", err)
	}
	// A repository-controlled symlink standing in for the workflows
	// directory must not be followed to write outside the repo.
	if err := os.Symlink(outside, filepath.Join(dir, ".github", "workflows")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := os.WriteFile(
		filepath.Join(dir, ".no-mistakes.yaml"),
		[]byte("commands:\n  test: \"go test ./...\"\n"),
		0644,
	); err != nil {
		t.Fatalf("write config: %v", err)
	}

	err := generateCIWorkflow(dir, false)
	if err == nil {
		t.Fatal("generateCIWorkflow() expected error for symlinked workflows dir, got nil")
	}
	if _, statErr := os.Stat(filepath.Join(outside, "ci.yml")); !os.IsNotExist(statErr) {
		t.Errorf("workflow should not have been written through the symlink, stat err = %v", statErr)
	}
}

func TestGenerateCIWorkflow_RefusesSymlinkedWorkflowFile(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	outsideFile := filepath.Join(t.TempDir(), "evil.yml")
	if err := os.WriteFile(outsideFile, []byte("original"), 0644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	workflowDir := filepath.Join(dir, ".github", "workflows")
	if err := os.MkdirAll(workflowDir, 0755); err != nil {
		t.Fatalf("mkdir workflows: %v", err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(workflowDir, "ci.yml")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := os.WriteFile(
		filepath.Join(dir, ".no-mistakes.yaml"),
		[]byte("commands:\n  test: \"go test ./...\"\n"),
		0644,
	); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// force=true must still refuse: overwriting the symlink destination
	// would write attacker-chosen content to a path outside the repo.
	err := generateCIWorkflow(dir, true)
	if err == nil {
		t.Fatal("generateCIWorkflow() expected error for symlinked workflow file, got nil")
	}
	content, readErr := os.ReadFile(outsideFile)
	if readErr != nil {
		t.Fatalf("read outside file: %v", readErr)
	}
	if string(content) != "original" {
		t.Errorf("outside file was overwritten through the symlink: %q", content)
	}
}

// Workflow validation helpers

func verifyWorkflowName(t *testing.T, workflow map[string]interface{}, expected string) {
	t.Helper()
	name, ok := workflow["name"].(string)
	if !ok || name != expected {
		t.Errorf("workflow name: got %q, want %q", name, expected)
	}
}

func verifyJobsExist(t *testing.T, workflow map[string]interface{}) {
	t.Helper()
	jobs, ok := workflow["jobs"].(map[string]interface{})
	if !ok || len(jobs) == 0 {
		t.Error("workflow missing jobs")
	}
}

func verifyStepNames(t *testing.T, workflow map[string]interface{}, stepNames []string) {
	t.Helper()
	jobs, ok := workflow["jobs"].(map[string]interface{})
	if !ok {
		t.Fatal("workflow missing jobs")
	}

	buildTestJob, ok := jobs["build-test"].(map[string]interface{})
	if !ok {
		t.Fatal("workflow missing build-test job")
	}

	stepsInterface, ok := buildTestJob["steps"].([]interface{})
	if !ok {
		t.Fatal("job missing steps")
	}

	foundSteps := make(map[string]bool)
	for _, s := range stepsInterface {
		step, ok := s.(map[string]interface{})
		if !ok {
			continue
		}
		if name, ok := step["name"].(string); ok {
			foundSteps[name] = true
		}
	}

	for _, wantName := range stepNames {
		if !foundSteps[wantName] {
			t.Errorf("workflow missing step: %q", wantName)
		}
	}
}

func verifyStepNotPresent(t *testing.T, workflow map[string]interface{}, stepName string) {
	t.Helper()
	jobs, ok := workflow["jobs"].(map[string]interface{})
	if !ok {
		return
	}

	buildTestJob, ok := jobs["build-test"].(map[string]interface{})
	if !ok {
		return
	}

	stepsInterface, ok := buildTestJob["steps"].([]interface{})
	if !ok {
		return
	}

	for _, s := range stepsInterface {
		step, ok := s.(map[string]interface{})
		if !ok {
			continue
		}
		if name, ok := step["name"].(string); ok && name == stepName {
			t.Errorf("workflow should not contain step: %q", stepName)
		}
	}
}

func verifyStepRunContains(t *testing.T, workflow map[string]interface{}, stepName string, expectedText string) {
	t.Helper()
	jobs, ok := workflow["jobs"].(map[string]interface{})
	if !ok {
		t.Fatal("workflow missing jobs")
	}

	buildTestJob, ok := jobs["build-test"].(map[string]interface{})
	if !ok {
		t.Fatal("workflow missing build-test job")
	}

	stepsInterface, ok := buildTestJob["steps"].([]interface{})
	if !ok {
		t.Fatal("job missing steps")
	}

	for _, s := range stepsInterface {
		step, ok := s.(map[string]interface{})
		if !ok {
			continue
		}
		if name, ok := step["name"].(string); ok && name == stepName {
			if run, ok := step["run"].(string); ok {
				if strings.Contains(run, expectedText) {
					return
				}
			}
			t.Errorf("step %q run does not contain %q", stepName, expectedText)
			return
		}
	}
	t.Errorf("step %q not found", stepName)
}
