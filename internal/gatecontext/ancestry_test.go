package gatecontext_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/gatecontext"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestInspectorRefusesInvalidAncestry(t *testing.T) {
	for _, tc := range []struct {
		name   string
		parent func(int) (int, error)
	}{
		{"unavailable process table", func(int) (int, error) { return 0, fmt.Errorf("unavailable") }},
		{"cycle", func(pid int) (int, error) { return pid, nil }},
		{"depth limit", func(pid int) (int, error) { return pid + 1, nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := (gatecontext.Inspector{
				Paths: paths.WithRoot(t.TempDir()), ParentPID: tc.parent,
			}).Inspect(context.Background(), gatecontext.Request{PeerPID: 100, DaemonPID: 2})
			if err == nil || result.RunContext != nil {
				t.Fatalf("invalid ancestry yielded context: %+v, %v", result, err)
			}
		})
	}
}
