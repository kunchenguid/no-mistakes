package azuredevops

import (
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// azPR is the subset of `az repos pr show/list/create` JSON output we consume.
type azPR struct {
	PullRequestID int    `json:"pullRequestId"`
	Status        string `json:"status"`      // active | completed | abandoned
	MergeStatus   string `json:"mergeStatus"` // notSet | queued | conflicts | succeeded | rejectedByPolicy | failure
	SourceRefName string `json:"sourceRefName"`
	TargetRefName string `json:"targetRefName"`
	URL           string `json:"url"` // _apis/... endpoint - NOT browsable
	Repository    struct {
		Name    string `json:"name"`
		WebURL  string `json:"webUrl"` // .../_git/{repo} - browsable base
		Project struct {
			Name string `json:"name"`
		} `json:"project"`
	} `json:"repository"`
}

// policyEval is the subset of `az repos pr policy list` evaluation records we
// consume. Branch policy evaluations are Azure DevOps's equivalent of PR checks.
type policyEval struct {
	Status        string `json:"status"` // queued | running | approved | rejected | notApplicable | broken
	StartedDate   string `json:"startedDate"`
	CompletedDate string `json:"completedDate"`
	Configuration struct {
		Type struct {
			DisplayName string `json:"displayName"`
		} `json:"type"`
		Settings struct {
			DisplayName string `json:"displayName"`
		} `json:"settings"`
	} `json:"configuration"`
	Context map[string]any `json:"context"`
}

// isCICheck reports whether a policy evaluation represents an automated check
// the CI monitor can meaningfully gate on and auto-fix, as opposed to a
// human/merge gate. Azure DevOps's automated policy types are "Build" (build
// validation) and "Status" (external status checks). Approval-gating policies
// (Minimum number of reviewers, Required reviewers, Comment requirements, Work
// item linking) and merge-config policies (Require a merge strategy) report a
// blocking/rejected status on a normal open PR that is simply awaiting human
// review; surfacing them as failing checks would drive the CI auto-fix loop
// into pointless attempts it can never satisfy. They are excluded here.
func (e policyEval) isCICheck() bool {
	switch strings.ToLower(strings.TrimSpace(e.Configuration.Type.DisplayName)) {
	case "build", "status":
		return true
	default:
		return false
	}
}

// checkName derives a human-readable check name, preferring the policy's
// configured display name, then the triggered build definition name, then the
// policy type.
func (e policyEval) checkName() string {
	if n := strings.TrimSpace(e.Configuration.Settings.DisplayName); n != "" {
		return n
	}
	if e.Context != nil {
		if v, ok := e.Context["buildDefinitionName"].(string); ok {
			if name := strings.TrimSpace(v); name != "" {
				return name
			}
		}
	}
	if n := strings.TrimSpace(e.Configuration.Type.DisplayName); n != "" {
		return n
	}
	return "policy"
}

func normalizePRState(raw string) scm.PRState {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "active":
		return scm.PRStateOpen
	case "completed":
		return scm.PRStateMerged
	case "abandoned":
		return scm.PRStateClosed
	default:
		return scm.PRState(strings.ToUpper(strings.TrimSpace(raw)))
	}
}

func normalizeMergeableState(raw string) scm.MergeableState {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "succeeded":
		return scm.MergeableOK
	case "conflicts":
		return scm.MergeableConflict
	default:
		// notSet, queued, rejectedByPolicy, failure, and unknown statuses are
		// not git merge conflicts: rejectedByPolicy means branch policies are
		// unsatisfied (surfaced separately as checks), and failure is a generic
		// often-transient async merge computation result. Treating them as
		// pending avoids driving the CI auto-fix loop into pointless rebases.
		return scm.MergeablePending
	}
}

func azStatusBucket(status string) scm.CheckBucket {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "approved":
		return scm.CheckBucketPass
	case "rejected", "broken":
		return scm.CheckBucketFail
	case "queued", "running":
		return scm.CheckBucketPending
	default:
		// notApplicable and unknown statuses are omitted so they never gate CI.
		return ""
	}
}

func parseAzTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t
		}
	}
	return time.Time{}
}
