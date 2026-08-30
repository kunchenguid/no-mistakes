package publication

import (
	"context"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// CandidateGuardPort observes the disposable candidate view used by one
// publication defense step. Manager uses this read-only seam only to guard the
// view before and after it.
type CandidateGuardPort interface {
	Inspect(ctx context.Context, publicationID string, step types.StepName) (CandidateSnapshot, error)
}

// CandidatePort owns the complete lifetime of a fresh candidate step view.
// The existing pipeline Executor still owns when the step runs; a publication
// adapter prepares and disposes the view around Manager's guard observations.
type CandidatePort interface {
	CandidateGuardPort
	PrepareStep(ctx context.Context, publicationID string, step types.StepName) (CandidateStepView, error)
	DisposeStep(ctx context.Context, publicationID string, step types.StepName) error
}

// CandidateStepView is one fresh, disposable defense view of the exact
// candidate. WorktreeDir is technically read-only; ScratchDir is a distinct
// writable directory owned by the publication run. WorkContractRaw is copied
// from the exact candidate commit after its request binding is verified.
type CandidateStepView struct {
	WorktreeDir     string
	ScratchDir      string
	WorkContractRaw []byte
}

// CandidateSnapshot contains every candidate property that must remain fixed
// while a publication defense step executes.
type CandidateSnapshot struct {
	CommitSHA         string
	TreeSHA           string
	TrackedClean      bool
	IndexClean        bool
	UntrackedClean    bool
	RefsSHA256        string
	ConfigSHA256      string
	ReplaceRefsSHA256 string
}

func candidateGuardedStep(step types.StepName) bool {
	switch step {
	case types.StepRebase, types.StepReview, types.StepTest, types.StepDocument, types.StepLint:
		return true
	default:
		return false
	}
}

func validateCandidateSnapshot(snapshot CandidateSnapshot, binding CandidateBinding) error {
	if snapshot.CommitSHA != binding.CommitSHA {
		return fmt.Errorf("candidate HEAD drifted: got %s, want %s", snapshot.CommitSHA, binding.CommitSHA)
	}
	if snapshot.TreeSHA != binding.TreeSHA {
		return fmt.Errorf("candidate tree drifted: got %s, want %s", snapshot.TreeSHA, binding.TreeSHA)
	}
	if !snapshot.TrackedClean || !snapshot.IndexClean || !snapshot.UntrackedClean {
		return fmt.Errorf("candidate view is not clean")
	}
	for name, digest := range map[string]string{
		"refs":         snapshot.RefsSHA256,
		"config":       snapshot.ConfigSHA256,
		"replace refs": snapshot.ReplaceRefsSHA256,
	} {
		if !isLowerHex(digest, 64) {
			return fmt.Errorf("candidate %s digest is not a lowercase SHA-256", name)
		}
	}
	return nil
}

func compareCandidateSnapshots(before, after CandidateSnapshot) error {
	if before != after {
		return fmt.Errorf("candidate changed while the defense step executed")
	}
	return nil
}
