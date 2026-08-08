package pipeline

import (
	"fmt"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

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

// RunShared carries run-scoped results one step hands to a later step.
// Continuations reconstruct accepted findings and evidence paths from copied
// step records; transient housekeeping state remains in-memory only.
type RunShared struct {
	mu               sync.Mutex
	housekeepingLint *HousekeepingLintResult
	acceptedFindings map[string]struct{}
	evidenceDir      string
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

// FilterAcceptedFindings removes findings already accepted in an earlier
// completed stage. It returns whether anything was removed and whether an
// actionable finding remains.
func (s *RunShared) FilterAcceptedFindings(raw string) (string, bool, bool) {
	if s == nil || raw == "" {
		return raw, false, true
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return raw, false, true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.acceptedFindings) == 0 {
		return raw, false, types.HasActionableFindings(findings)
	}
	items := findings.Items[:0]
	removed := false
	for _, item := range findings.Items {
		if _, ok := s.acceptedFindings[types.FindingFingerprint(item)]; ok {
			removed = true
			continue
		}
		items = append(items, item)
	}
	findings.Items = items
	if removed {
		findings.Summary = fmt.Sprintf("%d distinct findings", len(items))
	}
	normalized, err := types.MarshalFindingsJSON(findings)
	if err != nil {
		return raw, false, types.HasActionableFindings(findings)
	}
	return normalized, removed, types.HasActionableFindings(findings)
}

// AcceptFindings records the identity of findings accepted by a completed
// stage, so later agents cannot reopen the same decision under a new ID.
func (s *RunShared) AcceptFindings(raw string) {
	if s == nil || raw == "" {
		return
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.acceptedFindings == nil {
		s.acceptedFindings = make(map[string]struct{}, len(findings.Items))
	}
	for _, item := range findings.Items {
		s.acceptedFindings[types.FindingFingerprint(item)] = struct{}{}
	}
}

func (s *RunShared) SetEvidenceDir(dir string) {
	if s == nil || dir == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evidenceDir = dir
}

func (s *RunShared) EvidenceDir() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.evidenceDir
}
