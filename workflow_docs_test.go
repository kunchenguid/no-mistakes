package main

import (
	"os"
	"strings"
	"testing"
)

func TestDocsWorkflowUsesScopedConcurrencyGroup(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/docs.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "group: pages-${{ github.event_name }}-${{ github.ref }}") {
		t.Fatalf("docs workflow must scope concurrency by event and ref")
	}
	if strings.Contains(content, "group: pages\n") {
		t.Fatalf("docs workflow must not use a global concurrency group")
	}
}
