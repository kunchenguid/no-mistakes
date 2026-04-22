//go:build e2e

package e2e

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestValidateSkippedSteps(t *testing.T) {
	t.Run("accepts skipped statuses", func(t *testing.T) {
		errs := validateSkippedSteps([]ipc.StepResultInfo{{StepName: types.StepPR, Status: types.StepStatusSkipped}}, types.StepPR)
		if len(errs) != 0 {
			t.Fatalf("expected no errors, got %v", errs)
		}
	})

	t.Run("rejects completed statuses", func(t *testing.T) {
		errs := validateSkippedSteps([]ipc.StepResultInfo{{StepName: types.StepPR, Status: types.StepStatusCompleted}}, types.StepPR)
		if len(errs) != 1 || errs[0] != "expected pr to skip, got completed" {
			t.Fatalf("unexpected errors: %v", errs)
		}
	})
}

func TestValidatePushedHead(t *testing.T) {
	t.Run("accepts matching head shas", func(t *testing.T) {
		errs := validatePushedHead("abc123", "abc123")
		if len(errs) != 0 {
			t.Fatalf("expected no errors, got %v", errs)
		}
	})

	t.Run("rejects empty run head sha", func(t *testing.T) {
		errs := validatePushedHead("", "abc123")
		if len(errs) != 1 || errs[0] != "run completed without a recorded HeadSHA" {
			t.Fatalf("unexpected errors: %v", errs)
		}
	})

	t.Run("rejects upstream mismatch", func(t *testing.T) {
		errs := validatePushedHead("abc123", "def456")
		if len(errs) != 1 || errs[0] != "run HeadSHA = abc123, want upstream def456" {
			t.Fatalf("unexpected errors: %v", errs)
		}
	})
}
