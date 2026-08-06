package gatecontext

import (
	"errors"
	"fmt"
	"testing"
)

// preMigrationSchemaError fails an advisory read of the recursive-gate guard
// open, so it must attribute the missing column to a migration exactly. These
// cases pin the identifier comparison itself; the end-to-end degrade behavior
// is covered by TestInspectorPreMigrationSchemaDegradesActiveAgentSteps.
func TestPreMigrationSchemaErrorMatchesWholeColumnIdentifiers(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		tables []string
		want   bool
	}{
		{
			name:   "migration column on the named table degrades",
			err:    errors.New("get active runs: SQL logic error: no such column: submitted_head_sha (1)"),
			tables: []string{"runs"},
			want:   true,
		},
		{
			name:   "table-qualified migration column degrades",
			err:    errors.New("SQL logic error: no such column: runs.review_approved_head_sha (1)"),
			tables: []string{"runs"},
			want:   true,
		},
		{
			name:   "wrapped error is still attributed",
			err:    fmt.Errorf("list steps: %w", errors.New("no such column: auto_fix_limit (1)")),
			tables: []string{"step_results"},
			want:   true,
		},
		{
			// The old substring match treated any allowlist entry occurring in
			// the message as a hit, so `parked_ms` authorized a degrade for a
			// column that no migration adds.
			name:   "column merely prefixed by a migration column propagates",
			err:    errors.New("SQL logic error: no such column: parked_ms_extra (1)"),
			tables: []string{"runs"},
			want:   false,
		},
		{
			name:   "base-schema column propagates",
			err:    errors.New("SQL logic error: no such column: status (1)"),
			tables: []string{"runs"},
			want:   false,
		},
		{
			name:   "migration column of another table propagates",
			err:    errors.New("SQL logic error: no such column: agent_pid (1)"),
			tables: []string{"runs"},
			want:   false,
		},
		{
			name:   "non-schema error propagates",
			err:    errors.New("database is locked (5)"),
			tables: []string{"runs", "step_results"},
			want:   false,
		},
		{
			name:   "nil error is not a schema error",
			err:    nil,
			tables: []string{"runs"},
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := preMigrationSchemaError(tc.err, tc.tables...); got != tc.want {
				t.Fatalf("preMigrationSchemaError(%v, %v) = %v, want %v", tc.err, tc.tables, got, tc.want)
			}
		})
	}
}
