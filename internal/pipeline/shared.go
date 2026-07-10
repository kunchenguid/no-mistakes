package pipeline

import "sync"

// HousekeepingLintResult is the lint assessment produced by the combined
// document+lint housekeeping pass: the document step performs both duties in
// one agent invocation and hands the lint half to the lint step so it does
// not pay a second cold agent pass.
type HousekeepingLintResult struct {
	// FindingsJSON holds the lint-category findings (possibly an empty set)
	// in the same JSON shape the lint step produces itself.
	FindingsJSON string
	// Summary is the housekeeping pass's one-line lint summary.
	Summary string
}

// RunShared carries in-memory run-scoped results one step hands to a later
// step in the same run. It lives on the executor for the run's lifetime and
// is never persisted: on any process boundary the consuming step simply
// falls back to doing its own work.
type RunShared struct {
	mu               sync.Mutex
	housekeepingLint *HousekeepingLintResult
}

// SetHousekeepingLint records the combined pass's lint assessment for the
// lint step. It replaces any previous assessment (a document fix round
// re-runs the combined pass and re-stashes a fresh result).
func (s *RunShared) SetHousekeepingLint(result HousekeepingLintResult) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.housekeepingLint = &result
}

// ClearHousekeepingLint discards a previous combined-pass lint assessment
// before a document pass starts, so a later lint step never consumes stale
// findings.
func (s *RunShared) ClearHousekeepingLint() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.housekeepingLint = nil
}

// TakeHousekeepingLint returns and consumes the combined pass's lint
// assessment. The second call returns false so a lint fix round re-assesses
// with its own agent pass instead of trusting a stale result.
func (s *RunShared) TakeHousekeepingLint() (HousekeepingLintResult, bool) {
	if s == nil {
		return HousekeepingLintResult{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.housekeepingLint == nil {
		return HousekeepingLintResult{}, false
	}
	result := *s.housekeepingLint
	s.housekeepingLint = nil
	return result, true
}
