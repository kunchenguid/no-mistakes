// Package eval implements the local-only review evaluation toolkit.
//
// It deliberately owns a separate registry under <NM_HOME>/eval, so opening the
// normal pipeline database never creates an eval table or runs an eval
// migration, and nothing here emits telemetry or reaches the network.
//
// The dependency runs one way: the daemon calls AutoCapture when a run finishes
// (see RunManager.autoCaptureEvalCase) and RelabelRun when a source PR merges
// (see RunManager.relabelEvalRun). This package never calls back into the
// daemon, alters a gate, or influences a pipeline decision.
package eval

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	// manifestVersion is 2 because a case no longer carries its own Git
	// bundle: its commits live in a shared per-repository object pool (see
	// Store.poolDir). A version-1 case on disk points at a bundle this code no
	// longer reads, so it is rejected on load rather than half-restored.
	manifestVersion = 2
	// labelsVersion is 2 because the unit of truth is a finding-level
	// confusion-matrix gold label, not a case-level park/pass verdict.
	// Version-1 labels.json files are rejected on load.
	labelsVersion = 2
)

// Gold kinds are the scientific labels written from recorded evidence.
// Capture writes true-positive gold from a user Fix or from an auto-fix
// selection that later landed in a merged PR, false-negative gold from a
// user-added finding, and false-positive gold from a finding that shipped
// unfixed in a merged PR. IngestPostPRMiss writes additional false-negative
// gold after a green review. Adjudicated labels are never inferred from
// pending unmatched candidate findings.
const (
	GoldTruePositive  = "true-positive"
	GoldFalseNegative = "false-negative"
	GoldFalsePositive = "false-positive"
)

const (
	goldSourceUserFix        = "recorded-user-fix"
	goldSourceUserAdded      = "recorded-user-added"
	goldSourceAutoFixMerged  = "recorded-auto-fix-merged"
	goldSourceShippedUnfixed = "recorded-shipped-unfixed"
	goldSourcePostPRMiss     = "recorded-post-pr-miss"
)

// Candidate identifies one agent and model combination under evaluation. The
// canonical command-line spelling is agent+model, for example codex+gpt-5.4.
type Candidate struct {
	Agent types.AgentName `json:"agent"`
	Model string          `json:"model"`
}

func (c Candidate) String() string { return string(c.Agent) + "+" + c.Model }

// ParseCandidate accepts exactly agent+model. Keeping the model explicit makes
// comparison records self-describing rather than silently inheriting a user's
// current agent default.
func ParseCandidate(raw string) (Candidate, error) {
	value := strings.TrimSpace(raw)
	if strings.Count(value, "+") != 1 {
		return Candidate{}, fmt.Errorf("candidate must be agent+model (for example codex+gpt-5.4)")
	}
	agentName, model, _ := strings.Cut(value, "+")
	agentName = strings.TrimSpace(agentName)
	model = strings.TrimSpace(model)
	if agentName == "" || model == "" {
		return Candidate{}, fmt.Errorf("candidate must include both an agent and model")
	}
	name := types.AgentName(agentName)
	if _, ok := types.ACPTargetFor(name); ok {
		return Candidate{}, fmt.Errorf("candidate agent %q cannot enforce an explicit model", name)
	}
	return Candidate{Agent: name, Model: model}, nil
}

// Manifest pins every input needed to recreate a review pass without storing a
// remote URL. The commits it names live in this repository's local object pool
// and the configuration it replays under sits beside it in the case directory.
// It carries no digest of those objects: Git object names are content hashes,
// so the pins are their own integrity check.
type Manifest struct {
	Version          int    `json:"version"`
	ID               string `json:"id"`
	SourceRunID      string `json:"source_run_id"`
	SourceRoundID    string `json:"source_round_id"`
	CapturedAt       int64  `json:"captured_at"`
	RepoFingerprint  string `json:"repo_fingerprint"`
	Branch           string `json:"branch"`
	DefaultBranch    string `json:"default_branch"`
	BaseSHA          string `json:"base_sha"`
	HeadSHA          string `json:"head_sha"`
	StartingHeadSHA  string `json:"starting_head_sha"`
	ReviewedHeadSHA  string `json:"reviewed_head_sha"`
	TrustedConfigSHA string `json:"trusted_config_sha"`
	Intent           string `json:"intent,omitempty"`
	IntentSource     string `json:"intent_source,omitempty"`
	VersionAtCapture string `json:"no_mistakes_version,omitempty"`
	BuildSHA         string `json:"no_mistakes_build_sha,omitempty"`
	ChangedFiles     int    `json:"changed_files"`
	ChangedLines     int    `json:"changed_lines"`
}

// Decision records the human gate evidence available for the exported review
// pass. Approval actions were not persisted in historical rows, so Action can
// be "unknown". The original selections themselves are never guessed.
type Decision struct {
	Action             string   `json:"action"`
	SelectionSource    string   `json:"selection_source,omitempty"`
	SelectedFindingIDs []string `json:"selected_finding_ids,omitempty"`
	HasUserFindings    bool     `json:"has_user_findings"`
}

// FindingGold is one finding-level gold label for an underlying issue.
// Capture writes only the labels goldFromRound can support from recorded
// evidence; unmatched later candidate findings stay unlabeled.
// IngestPostPRMiss writes additional false-negative gold for a confirmed
// miss after a green review.
type FindingGold struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Source      string `json:"source,omitempty"`
	File        string `json:"file,omitempty"`
	Line        int    `json:"line,omitempty"`
	Description string `json:"description,omitempty"`
	Severity    string `json:"severity,omitempty"`
	Action      string `json:"action,omitempty"`
}

// Labels is a local, growing label file. Finding-level gold is the unit of
// truth. Queued candidate findings are kept as evidence for later
// adjudication and are never scored as false positives.
type Labels struct {
	Version                 int           `json:"version"`
	Findings                []FindingGold `json:"findings,omitempty"`
	QueuedCandidateFindings int           `json:"queued_candidate_findings"`
}

func (l Labels) HasGold() bool { return len(l.Findings) > 0 }

func (l Labels) TrueIssueCount() int {
	n := 0
	for _, finding := range l.Findings {
		if isTrueIssueGold(finding.Kind) {
			n++
		}
	}
	return n
}

func (l Labels) FalsePositiveCount() int {
	n := 0
	for _, finding := range l.Findings {
		if finding.Kind == GoldFalsePositive {
			n++
		}
	}
	return n
}

func isTrueIssueGold(kind string) bool {
	return kind == GoldTruePositive || kind == GoldFalseNegative
}

// BaselineMetrics is the recorded source-review performance baseline. A false
// TokensReported means the adapter did not surface trustworthy token usage.
type BaselineMetrics struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	FreshInputTokens int64 `json:"fresh_input_tokens"`
	DurationMS       int64 `json:"duration_ms"`
	TokensReported   bool  `json:"tokens_reported"`
}

// Case is one frozen review pass. Dir is local bookkeeping and never enters a
// manifest or report payload.
type Case struct {
	Manifest
	Labels   Labels          `json:"labels"`
	Decision Decision        `json:"decision"`
	Baseline BaselineMetrics `json:"baseline"`
	Dir      string          `json:"-"`
}

// Evaluation is one candidate replay over one case. Status is "completed" or
// "failed"; failures remain visible in reports and are not silently scored.
// Confusion-matrix fields are finding-level: unmatched candidate findings stay
// in Pending and are never treated as false positives.
type Evaluation struct {
	ID                string `json:"id"`
	SessionID         string `json:"session_id"`
	CaseID            string `json:"case_id"`
	Candidate         string `json:"candidate"`
	Cohort            string `json:"cohort"`
	Repeat            int    `json:"repeat"`
	StartedAt         int64  `json:"started_at"`
	CompletedAt       int64  `json:"completed_at"`
	Status            string `json:"status"`
	Error             string `json:"error,omitempty"`
	HasFindingGold    bool   `json:"has_finding_gold"`
	GoldCount         int    `json:"gold_count"`
	TruePositive      int    `json:"true_positive"`
	TruePositiveExact int    `json:"true_positive_exact,omitempty"`
	TruePositiveFuzzy int    `json:"true_positive_fuzzy,omitempty"`
	FalseNegative     int    `json:"false_negative"`
	FalsePositive     int    `json:"false_positive"`
	FalsePositiveGold int    `json:"false_positive_gold,omitempty"`
	Pending           int    `json:"pending"`
	FindingsJSON      string `json:"findings_json,omitempty"`
	FindingCount      int    `json:"finding_count"`
	InputTokens       int64  `json:"input_tokens"`
	OutputTokens      int64  `json:"output_tokens"`
	CacheReadTokens   int64  `json:"cache_read_tokens"`
	FreshInputTokens  int64  `json:"fresh_input_tokens"`
	TokensReported    bool   `json:"tokens_reported"`
	DurationMS        int64  `json:"duration_ms"`
	Model             string `json:"model,omitempty"`
}

// EvaluationSummary aggregates finding-level scores. A case with no gold is
// unlabeled / pending, never a pass. Unmatched candidate findings stay in
// Pending and do not become false positives.
type EvaluationSummary struct {
	Candidate         string
	Total             int
	Labeled           int
	TruePositive      int
	TruePositiveExact int
	TruePositiveFuzzy int
	FalseNegative     int
	FalsePositive     int
	FalsePositiveGold int
	Pending           int
	Failures          int
	InputTokens       int64
	OutputTokens      int64
	FreshInputTokens  int64
	TokensReported    int
	DurationMS        int64
}

func (s EvaluationSummary) Recall() float64 {
	denom := s.TruePositive + s.FalseNegative
	if denom == 0 {
		return 0
	}
	return float64(s.TruePositive) / float64(denom)
}

func (s EvaluationSummary) ExactRecall() float64 {
	denom := s.TruePositive + s.FalseNegative
	if denom == 0 {
		return 0
	}
	return float64(s.TruePositiveExact) / float64(denom)
}

func (s EvaluationSummary) Precision() float64 {
	denom := s.TruePositive + s.FalsePositive
	if denom == 0 {
		return 0
	}
	return float64(s.TruePositive) / float64(denom)
}

func (s EvaluationSummary) PrecisionLower() float64 {
	denom := s.TruePositive + s.FalsePositive + s.Pending
	if denom == 0 {
		return 0
	}
	return float64(s.TruePositive) / float64(denom)
}

func (s EvaluationSummary) F1() float64 {
	return harmonicMean(s.Recall(), s.Precision())
}

func (s EvaluationSummary) F1Conservative() float64 {
	return harmonicMean(s.Recall(), s.PrecisionLower())
}

func (s EvaluationSummary) HasFalsePositiveGold() bool {
	return s.FalsePositiveGold > 0
}

func harmonicMean(a, b float64) float64 {
	if a <= 0 || b <= 0 {
		return 0
	}
	return 2 * a * b / (a + b)
}

// SummarizeEvaluations scores finding-level gold only. Unmatched candidate
// findings stay pending. A replay with no gold is unlabeled, not a pass.
func SummarizeEvaluations(evaluations []Evaluation) EvaluationSummary {
	var summary EvaluationSummary
	for _, evaluation := range evaluations {
		if summary.Candidate == "" {
			summary.Candidate = evaluation.Candidate
		}
		summary.Total++
		summary.InputTokens += evaluation.InputTokens
		summary.OutputTokens += evaluation.OutputTokens
		summary.FreshInputTokens += evaluation.FreshInputTokens
		summary.DurationMS += evaluation.DurationMS
		if evaluation.TokensReported {
			summary.TokensReported++
		}
		hasFindingGold := evaluation.HasFindingGold || evaluation.GoldCount > 0
		if evaluation.Status != "completed" {
			summary.Failures++
			if hasFindingGold {
				summary.Labeled++
				summary.FalseNegative += evaluation.GoldCount
				summary.FalsePositiveGold += evaluation.FalsePositiveGold
			}
			summary.Pending += evaluation.Pending
			continue
		}
		summary.Pending += evaluation.Pending
		if !hasFindingGold {
			continue
		}
		summary.Labeled++
		summary.TruePositive += evaluation.TruePositive
		summary.TruePositiveExact += evaluation.TruePositiveExact
		summary.TruePositiveFuzzy += evaluation.TruePositiveFuzzy
		summary.FalseNegative += evaluation.FalseNegative
		summary.FalsePositive += evaluation.FalsePositive
		summary.FalsePositiveGold += evaluation.FalsePositiveGold
	}
	return summary
}
