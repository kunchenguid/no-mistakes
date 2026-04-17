package steps

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ciCheck represents a CI check result from gh pr checks --json.
type ciCheck struct {
	Name       string `json:"name"`
	Status     string `json:"status"`     // legacy fake-test field
	Conclusion string `json:"conclusion"` // legacy fake-test field
	State      string `json:"state"`      // gh CLI field
	Bucket     string `json:"bucket"`     // gh CLI field: pass|fail|pending|skipping|cancel
}

// extractPRNumber extracts the PR number from a GitHub PR URL.
// Handles URLs like "https://github.com/owner/repo/pull/42".
func extractPRNumber(prURL string) (string, error) {
	trimmed := strings.TrimRight(prURL, "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) == 0 {
		return "", fmt.Errorf("invalid PR URL: %s", prURL)
	}
	num := parts[len(parts)-1]
	if num == "" {
		return "", fmt.Errorf("invalid PR URL: %s", prURL)
	}
	if _, err := strconv.Atoi(num); err != nil {
		return "", fmt.Errorf("invalid PR number %q in URL: %s", num, prURL)
	}
	return num, nil
}

// pollInterval returns the polling interval based on elapsed time since CI monitoring started.
// 30s for first 5min, 60s for 5-15min, 120s after.
func pollInterval(elapsed time.Duration) time.Duration {
	switch {
	case elapsed < 5*time.Minute:
		return 30 * time.Second
	case elapsed < 15*time.Minute:
		return 60 * time.Second
	default:
		return 120 * time.Second
	}
}

// hasFailingChecks returns true if any CI check has a failure conclusion.
func hasFailingChecks(checks []ciCheck) bool {
	for _, c := range checks {
		if c.Bucket == "fail" || c.Conclusion == "failure" || c.Conclusion == "action_required" {
			return true
		}
	}
	return false
}

// hasPendingChecks returns true if any CI check is still running or queued.
func hasPendingChecks(checks []ciCheck) bool {
	for _, c := range checks {
		if c.Bucket == "pending" {
			return true
		}
		if c.Bucket != "" {
			continue
		}
		if c.Conclusion == "" && c.Status != "COMPLETED" {
			return true
		}
	}
	return false
}

// failingCheckNames returns the names of failing checks.
func failingCheckNames(checks []ciCheck) []string {
	var names []string
	for _, c := range checks {
		if c.Bucket == "fail" || c.Conclusion == "failure" || c.Conclusion == "action_required" {
			names = append(names, c.Name)
		}
	}
	return names
}

func ciFailureOutcome(failing []string, mergeConflict bool, summary string) *pipeline.StepOutcome {
	findings := Findings{Summary: summary}
	for _, name := range failing {
		findings.Items = append(findings.Items, Finding{
			Severity:    "warning",
			Description: fmt.Sprintf("CI check failing: %s", name),
		})
	}
	if mergeConflict {
		findings.Items = append(findings.Items, Finding{
			Severity:    "warning",
			Description: "PR has merge conflicts with the base branch",
		})
	}
	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		Findings:      string(findingsJSON),
	}
}

func ciMergeabilityOutcome(summary, description string) *pipeline.StepOutcome {
	findings := Findings{
		Summary: summary,
		Items: []Finding{{
			Severity:    "warning",
			Description: description,
			Action:      types.ActionAskUser,
		}},
	}
	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		Findings:      string(findingsJSON),
	}
}

// isMergeConflict returns true if the mergeable state indicates conflicts.
func isMergeConflict(state string) bool {
	return state == "CONFLICTING"
}

func isResolvedMergeableState(state string) bool {
	return state == "MERGEABLE" || state == "CONFLICTING"
}
