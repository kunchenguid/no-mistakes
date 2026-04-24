package main

import (
	"os"
	"strings"
	"testing"
)

func TestCIWorkflowRunsTestsOnAllSupportedDesktopPlatforms(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/ci.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}

	content := string(data)
	for _, osName := range []string{"ubuntu-latest", "macos-latest", "windows-latest"} {
		if !strings.Contains(content, osName) {
			t.Fatalf("CI workflow must test %q", osName)
		}
	}
}

func TestCIWorkflowUsesRaceTestsOnUnixRunners(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/ci.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "if: runner.os != 'Windows'") {
		t.Fatalf("CI workflow must keep the Unix test branch so macOS runs the Unix suite")
	}
	if !strings.Contains(content, "run: go test -race ./...") {
		t.Fatalf("CI workflow must run the race-enabled suite on Unix runners")
	}
}
