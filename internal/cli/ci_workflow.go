package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/spf13/cobra"
)

const ciWorkflowTemplateWithLint = `name: CI

on:
  push:
    branches: [%s]
  pull_request:

permissions:
  contents: read

concurrency:
  group: ci-${{ github.ref }}
  cancel-in-progress: true

jobs:
  build-test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          check-latest: true

      - name: Lint
        run: |
          %s

      - name: Test
        run: |
          %s
`

const ciWorkflowTemplateTestOnly = `name: CI

on:
  push:
    branches: [%s]
  pull_request:

permissions:
  contents: read

concurrency:
  group: ci-${{ github.ref }}
  cancel-in-progress: true

jobs:
  build-test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          check-latest: true

      - name: Test
        run: |
          %s
`

func newCIWorkflowCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "ci-workflow",
		Short: "Generate .github/workflows/ci.yml from .no-mistakes.yaml commands",
		Long: "Emits .github/workflows/ci.yml that mirrors the canonical checks pinned in .no-mistakes.yaml.\n" +
			"The workflow runs on push to the default branch and on all pull requests, registering\n" +
			"real GitHub checks that the no-mistakes gate's CI step can monitor.\n\n" +
			"Run this from inside a git repository that has been initialized with 'no-mistakes init'.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommand("ci-workflow", func() error {
				return generateCIWorkflow(".", force)
			})
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "overwrite existing workflow file")
	return cmd
}

func generateCIWorkflow(dir string, force bool) error {
	// Load the repo config
	cfg, err := config.LoadRepo(dir)
	if err != nil {
		return fmt.Errorf("ci-workflow: %w", err)
	}

	// Resolve the repository's default branch
	// Use a timeout context for the remote lookup
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defaultBranch := git.DefaultBranch(ctx, dir, "origin")

	// Extract test command (required)
	test := cfg.Commands.Test
	if test == "" {
		return fmt.Errorf("ci-workflow: commands.test not configured in .no-mistakes.yaml")
	}

	// Extract lint command (optional; empty means combined document+lint)
	lint := cfg.Commands.Lint

	// Select template and generate workflow
	var workflow string
	if lint == "" {
		workflow = fmt.Sprintf(ciWorkflowTemplateTestOnly, defaultBranch, indentCommand(test))
	} else {
		workflow = fmt.Sprintf(ciWorkflowTemplateWithLint, defaultBranch, indentCommand(lint), indentCommand(test))
	}

	// Determine output path
	workflowDir := filepath.Join(dir, ".github", "workflows")
	workflowPath := filepath.Join(workflowDir, "ci.yml")

	// Check if file exists
	if _, err := os.Stat(workflowPath); err == nil && !force {
		return fmt.Errorf("ci-workflow: %s already exists (use --force to overwrite)", workflowPath)
	}

	// Create .github/workflows directory if needed
	if err := os.MkdirAll(workflowDir, 0755); err != nil {
		return fmt.Errorf("ci-workflow: create .github/workflows: %w", err)
	}

	// Write the workflow file
	if err := os.WriteFile(workflowPath, []byte(workflow), 0644); err != nil {
		return fmt.Errorf("ci-workflow: write workflow: %w", err)
	}

	fmt.Printf("  %s  %s\n", sGreen.Render("✓"), "CI workflow generated")
	fmt.Printf("  %s  %s\n", sDim.Render("    "), workflowPath)
	if lint == "" {
		fmt.Printf("  %s  %s\n", sDim.Render("    "), "(test only; lint uses combined document+lint)")
	}
	fmt.Println()
	fmt.Printf("  %s\n", sDim.Render("Commit and push this file to enable GitHub checks:"))
	fmt.Printf("  %s\n", sBold.Render("git add .github/workflows/ci.yml && git commit -m 'ci: register workflow'"))

	return nil
}

// ciWorkflowRunIndent matches the leading whitespace before the `%s` placeholder
// in the run: | block scalars above, so a multi-line command's continuation
// lines stay inside the scalar instead of dedenting out of it.
const ciWorkflowRunIndent = "          "

// indentCommand indents every line of cmd after the first so a multi-line
// command stays inside the YAML block scalar it's substituted into.
func indentCommand(cmd string) string {
	lines := strings.Split(cmd, "\n")
	for i := 1; i < len(lines); i++ {
		lines[i] = ciWorkflowRunIndent + lines[i]
	}
	return strings.Join(lines, "\n")
}
