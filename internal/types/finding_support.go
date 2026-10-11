package types

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// FindingClaimType identifies the kind of claim a review finding makes. It
// selects one reference below; the reference is never itself proof.
type FindingClaimType string

const (
	FindingClaimSource  FindingClaimType = "source"
	FindingClaimTest    FindingClaimType = "test"
	FindingClaimCI      FindingClaimType = "ci"
	FindingClaimRuntime FindingClaimType = "runtime"
)

const (
	FindingCategoryReviewSupportPending    = "review-support-pending"
	FindingCategoryReviewSupportResolved   = "review-support-resolved"
	FindingCategoryReviewSupportUnresolved = "review-support-unresolved"
	FindingSupportDispositionSupported     = "supported"
	FindingSupportDispositionDisproven     = "disproven"
	FindingSupportDispositionUnresolved    = "unresolved"
)

// PendingCISupport is a handoff to a named external CI verifier. It records
// the Review claim and current comparison; none of these fields proves that
// the historical check has passed on the current head.
type PendingCISupport struct {
	ClaimID           string `json:"claim_id"`
	OriginalFindingID string `json:"original_finding_id"`
	HistoricalCheckID string `json:"historical_check_id"`
	HistoricalHeadSHA string `json:"historical_head_sha"`
	SourceHeadSHA     string `json:"source_head_sha"`
	ForgeHeadSHA      string `json:"forge_head_sha,omitempty"`
	TargetBranch      string `json:"target_branch"`
	TargetSHA         string `json:"target_sha"`
	DiffDigest        string `json:"diff_digest"`
	ReceiptGeneration int64  `json:"receipt_generation"`
	RunID             string `json:"run_id"`
	PRURL             string `json:"pr_url,omitempty"`
	Owner             string `json:"owner"`
}

func IsKnownFindingClaimType(kind FindingClaimType) bool {
	switch kind {
	case FindingClaimSource, FindingClaimTest, FindingClaimCI, FindingClaimRuntime:
		return true
	default:
		return false
	}
}

// FindingSupport carries a typed reference to the observation behind a
// finding. A trusted resolver must check it against repository, test, CI, or
// runtime evidence before treating the claim as established. In particular,
// model-provided heads, run IDs, and times are context, not attestations.
type FindingSupport struct {
	ClaimType FindingClaimType       `json:"claim_type"`
	Source    *FindingSourceSupport  `json:"source,omitempty"`
	Test      *FindingTestSupport    `json:"test,omitempty"`
	CI        *FindingCISupport      `json:"ci,omitempty"`
	Runtime   *FindingRuntimeSupport `json:"runtime,omitempty"`
	// OwnerResult is written only by a trusted Test or CI owner after reading
	// its own evidence. Review rejects this field in agent output.
	OwnerResult *FindingOwnerResult `json:"owner_result,omitempty"`
}

// FindingOwnerResult binds a Test or CI decision to one current Review claim
// and the exact PR comparison receipt. An owner writes it after a trusted
// command or provider read; its presence in model output has no authority.
type FindingOwnerResult struct {
	ReviewFindingID string `json:"review_finding_id"`
	HeadSHA         string `json:"head_sha"`
	TargetSHA       string `json:"target_sha"`
	DiffDigest      string `json:"diff_digest"`
	Generation      int64  `json:"generation"`
	ObservedAt      string `json:"observed_at"`
	Disposition     string `json:"disposition"`
	ExitCode        *int   `json:"exit_code,omitempty"`
	CheckState      string `json:"check_state,omitempty"`
}

// ReviewSupportClaimID is stable across an owner's appended result and
// changes when either the Review finding ID or typed claim changes.
func ReviewSupportClaimID(finding Finding) string {
	if finding.Support == nil {
		return ""
	}
	claim := *finding.Support
	claim.OwnerResult = nil
	encoded, err := json.Marshal(claim)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(append(append([]byte(finding.ID), 0), encoded...))
	return "review-support-" + hex.EncodeToString(digest[:])
}

// FindingSourceSupport points to quoted source. Historical context is
// optional and cannot independently prove that a line existed or was read.
type FindingSourceSupport struct {
	Path       string `json:"path"`
	Line       int    `json:"line"`
	Quote      string `json:"quote"`
	HeadSHA    string `json:"head_sha,omitempty"`
	RunID      string `json:"run_id,omitempty"`
	ObservedAt string `json:"observed_at,omitempty"`
}

// FindingTestSupport identifies the command whose result the finding claims.
type FindingTestSupport struct {
	Command string `json:"command"`
}

// FindingCISupport identifies a provider check on a particular head.
type FindingCISupport struct {
	CheckID string `json:"check_id"`
	HeadSHA string `json:"head_sha"`
}

// FindingRuntimeSupport identifies an observed entity transition.
type FindingRuntimeSupport struct {
	Entity       string `json:"entity"`
	Transition   string `json:"transition"`
	TransitionAt string `json:"transition_at"`
	ObservedAt   string `json:"observed_at"`
	Revision     string `json:"revision"`
}

// Validate checks reference shape only. It does not establish that the
// observation happened, that a SHA was current, or that a source quote matches.
func (s FindingSupport) Validate() error {
	if !IsKnownFindingClaimType(s.ClaimType) {
		return fmt.Errorf("unknown finding claim type %q", s.ClaimType)
	}
	present := 0
	for _, exists := range []bool{s.Source != nil, s.Test != nil, s.CI != nil, s.Runtime != nil} {
		if exists {
			present++
		}
	}
	if present != 1 {
		return fmt.Errorf("finding support must contain exactly one claim reference, got %d", present)
	}
	switch s.ClaimType {
	case FindingClaimSource:
		if s.Source == nil {
			return fmt.Errorf("source finding requires source support")
		}
		if strings.TrimSpace(s.Source.Path) == "" || s.Source.Line < 1 || strings.TrimSpace(s.Source.Quote) == "" {
			return fmt.Errorf("source finding requires path, positive line, and quote")
		}
		if s.Source.ObservedAt != "" && !validFindingObservationTime(s.Source.ObservedAt) {
			return fmt.Errorf("source finding observed_at must be RFC3339")
		}
	case FindingClaimTest:
		if s.Test == nil || strings.TrimSpace(s.Test.Command) == "" {
			return fmt.Errorf("test finding requires command")
		}
	case FindingClaimCI:
		if s.CI == nil || strings.TrimSpace(s.CI.CheckID) == "" || strings.TrimSpace(s.CI.HeadSHA) == "" {
			return fmt.Errorf("CI finding requires check_id and head_sha")
		}
	case FindingClaimRuntime:
		if s.Runtime == nil || strings.TrimSpace(s.Runtime.Entity) == "" || strings.TrimSpace(s.Runtime.Transition) == "" ||
			!validFindingObservationTime(s.Runtime.TransitionAt) || !validFindingObservationTime(s.Runtime.ObservedAt) ||
			!validFindingRevision(s.Runtime.Revision) {
			return fmt.Errorf("runtime finding requires entity, transition, transition time, observation time, and revision")
		}
	}
	return nil
}

func validFindingRevision(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, c := range value {
		if c >= '0' && c <= '9' || c >= 'a' && c <= 'f' {
			continue
		}
		return false
	}
	return true
}

func validFindingObservationTime(value string) bool {
	_, err := time.Parse(time.RFC3339Nano, value)
	return err == nil
}
