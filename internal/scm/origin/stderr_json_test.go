package origin

import (
	"context"
	"strings"
	"testing"
)

// A harmless warning on stderr must not corrupt the JSON read from stdout.
func TestFindPRIgnoresStderrWarning(t *testing.T) {
	t.Parallel()

	host := New(originTestCmdFactory(map[string]originTestResponse{
		listKey("feature/x"): {
			stdout: `[{"number":7,"url":"https://cursor.com/codebase/owner/repo/pull/7","status":"open","headRef":"feature/x","baseRef":"main"}]`,
			stderr: "tool wrapper warning\n",
		},
	}), nil, "owner/repo", false)
	pr, err := host.FindPR(context.Background(), "feature/x", "")
	if err != nil || pr == nil || pr.Number != "7" {
		t.Fatalf("FindPR() = %+v, %v; want PR 7", pr, err)
	}
}

func TestFindPRFailureKeepsStderrDiagnostic(t *testing.T) {
	t.Parallel()

	host := New(originTestCmdFactory(map[string]originTestResponse{
		listKey("feature/x"): {stderr: "synthetic list failure\n", code: 1},
	}), nil, "owner/repo", false)
	if _, err := host.FindPR(context.Background(), "feature/x", ""); err == nil || !strings.Contains(err.Error(), "synthetic list failure") {
		t.Fatalf("FindPR() error = %v, want stderr diagnostic", err)
	}
}
