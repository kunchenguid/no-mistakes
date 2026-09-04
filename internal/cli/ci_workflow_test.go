package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateCIWorkflow(t *testing.T) {
	tests := []struct {
		name        string
		configYAML  string
		force       bool
		wantErr     bool
		wantContent string
		setup       func(dir string) error
	}{
		{
			name: "basic go repo with lint and test",
			configYAML: `commands:
  lint: "go vet ./... && gofmt -l ."
  test: "go test ./... -race"
`,
			wantErr: false,
			wantContent: "go vet ./... && gofmt -l .",
		},
		{
			name: "missing lint command",
			configYAML: `commands:
  test: "go test ./..."
`,
			wantErr: true,
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
			wantErr: true,
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

			// Check output file exists
			workflowPath := filepath.Join(dir, ".github", "workflows", "ci.yml")
			content, err := os.ReadFile(workflowPath)
			if err != nil {
				t.Errorf("workflow file not created: %v", err)
				return
			}

			// Check content
			contentStr := string(content)
			if tt.wantContent != "" && !strings.Contains(contentStr, tt.wantContent) {
				t.Errorf("workflow content missing expected text: %q", tt.wantContent)
			}

			// Verify structure
			if !strings.Contains(contentStr, "name: CI") {
				t.Error("workflow missing name field")
			}
			if !strings.Contains(contentStr, "runs-on: ubuntu-latest") {
				t.Error("workflow missing runs-on field")
			}
			if !strings.Contains(contentStr, "uses: actions/checkout@v4") {
				t.Error("workflow missing checkout step")
			}
			if !strings.Contains(contentStr, "uses: actions/setup-go@v5") {
				t.Error("workflow missing setup-go step")
			}
		})
	}
}
