//go:build unit

package main

import (
	"os"
	"strings"
	"testing"
)

func TestCIWorkflowRunsUnitTestsOnWindows(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/ci.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "test-windows:") {
		t.Fatalf("ci workflow must define a dedicated Windows test job")
	}
	if !strings.Contains(content, "go test -tags unit ./...") {
		t.Fatalf("windows ci must run unit-tagged tests")
	}
	if !strings.Contains(content, "go test -tags integration,e2e ./...") {
		t.Fatalf("windows ci must keep integration and e2e coverage")
	}
}
