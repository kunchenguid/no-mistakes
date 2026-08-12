// Package eval implements the local-only review evaluation toolkit.
//
// It deliberately owns a separate registry under <NM_HOME>/eval, so opening the
// normal pipeline database never creates an eval table or runs an eval
// migration, and nothing here emits telemetry or reaches the network.
//
// The dependency runs one way: the daemon calls AutoCapture when a run finishes
// (see RunManager.autoCaptureEvalCase), and this package never calls back into
// the daemon, alters a gate, or influences a pipeline decision.
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
	labelsVersion   = 1
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

// VerdictLabel is intentionally only verdict-level in the MVP. Finding-level
// valid/invalid labels and their adjudication UI belong to phase 1.
type VerdictLabel struct {
	Known      bool   `json:"known"`
	ShouldPark bool   `json:"should_park"`
	Source     string `json:"source,omitempty"`
}

// Labels is a local, growing label file. Queued candidate findings are kept as
// evidence for a future adjudication pass, never scored as false positives.
type Labels struct {
	Version                 int          `json:"version"`
	Verdict                 VerdictLabel `json:"verdict"`
	QueuedCandidateFindings int          `json:"queued_candidate_findings"`
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
type Evaluation struct {
	ID               string `json:"id"`
	SessionID        string `json:"session_id"`
	CaseID           string `json:"case_id"`
	Candidate        string `json:"candidate"`
	Cohort           string `json:"cohort"`
	Repeat           int    `json:"repeat"`
	StartedAt        int64  `json:"started_at"`
	CompletedAt      int64  `json:"completed_at"`
	Status           string `json:"status"`
	Error            string `json:"error,omitempty"`
	ExpectedPark     *bool  `json:"expected_park,omitempty"`
	CandidateParked  bool   `json:"candidate_parked"`
	FindingsJSON     string `json:"findings_json,omitempty"`
	FindingCount     int    `json:"finding_count"`
	InputTokens      int64  `json:"input_tokens"`
	OutputTokens     int64  `json:"output_tokens"`
	CacheReadTokens  int64  `json:"cache_read_tokens"`
	FreshInputTokens int64  `json:"fresh_input_tokens"`
	TokensReported   bool   `json:"tokens_reported"`
	DurationMS       int64  `json:"duration_ms"`
	Model            string `json:"model,omitempty"`
}

// EvaluationSummary is deliberately three-valued for a human-pass label:
// an unexpected candidate park is queued rather than declared wrong before
// finding-level adjudication exists.
type EvaluationSummary struct {
	Candidate        string
	Total            int
	Labeled          int
	Conclusive       int
	Correct          int
	Misses           int
	UnexpectedParks  int
	Failures         int
	InputTokens      int64
	OutputTokens     int64
	FreshInputTokens int64
	TokensReported   int
	DurationMS       int64
}

func (s EvaluationSummary) ConfirmedAccuracy() float64 {
	if s.Conclusive == 0 {
		return 0
	}
	return float64(s.Correct) / float64(s.Conclusive)
}

// LowerBoundAccuracy counts a queued unexpected park in the denominator but
// not the numerator. It is the conservative number suitable for comparing
// candidates before phase-1 finding adjudication is available.
func (s EvaluationSummary) LowerBoundAccuracy() float64 {
	if s.Labeled == 0 {
		return 0
	}
	return float64(s.Correct) / float64(s.Labeled)
}

// SummarizeEvaluations applies the MVP verdict policy without inferring that a
// new finding is invalid merely because the original human passed the run.
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
		if evaluation.Status != "completed" {
			summary.Failures++
			if evaluation.ExpectedPark != nil {
				summary.Labeled++
				summary.Conclusive++
				summary.Misses++
			}
			continue
		}
		if evaluation.ExpectedPark == nil {
			continue
		}
		summary.Labeled++
		switch {
		case *evaluation.ExpectedPark && evaluation.CandidateParked:
			summary.Conclusive++
			summary.Correct++
		case *evaluation.ExpectedPark && !evaluation.CandidateParked:
			summary.Conclusive++
			summary.Misses++
		case !*evaluation.ExpectedPark && !evaluation.CandidateParked:
			summary.Conclusive++
			summary.Correct++
		case !*evaluation.ExpectedPark && evaluation.CandidateParked:
			summary.UnexpectedParks++
		}
	}
	return summary
}
