package cli

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

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
			name: "empty config",
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()

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
				if runContains(run, expectedText) {
					return
				}
			}
			t.Errorf("step %q run does not contain %q", stepName, expectedText)
			return
		}
	}
	t.Errorf("step %q not found", stepName)
}

func runContains(haystack, needle string) bool {
	for i := 0; i <= len(haystack)-len(needle); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
