package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
)

func TestAttemptRecordRetainsTypedSchemaField(t *testing.T) {
	started := time.Unix(1, 0)
	err := fmt.Errorf("wrapped: %w", &agent.SchemaViolation{Field: "findings.tested", Message: "expected array"})
	row := attemptRecord(started, started.Add(2*time.Second), nil, err)
	if row["schema_valid"] != false || row["schema_field"] != "findings.tested" || row["wall_ms"] != int64(2000) {
		t.Fatalf("attempt record = %#v", row)
	}
}

func TestAttemptRecordDoesNotFabricateUnreportedUsage(t *testing.T) {
	row := attemptRecord(time.Now(), time.Now(), &agent.Result{}, nil)
	if _, exists := row["usage"]; exists {
		t.Fatalf("unreported usage was recorded: %#v", row)
	}
	if row["schema_field"] != nil {
		t.Fatalf("successful attempt has a failed schema field: %#v", row)
	}
}
