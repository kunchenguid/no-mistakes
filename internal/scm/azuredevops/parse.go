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
			// StatusGenre/StatusName identify a "Status" policy's underlying status
			// check by stable, non-localized keys (e.g. genre
			// "microsoft-policy-service", name "CodeReviewCompliancePolicy"). Azure
			// DevOps posts several human sign-off gates through the generic Status
			// policy type, so the displayName alone cannot distinguish them from a
			// content status check - these keys can. See humanStatusGate.
			StatusGenre string `json:"statusGenre"`
			StatusName  string `json:"statusName"`
		} `json:"settings"`
	} `json:"configuration"`
	Context map[string]any `json:"context"`
}

// humanPolicyTypes are Azure DevOps policy type displayNames that represent
// human review / sign-off / attestation gates rather than content-influenced
// automation. They are the captain's responsibility in the ADO UI, not a CI
// signal firstmate/no-mistakes gates on, and report a blocking "rejected"
// status on a normal open PR that is simply awaiting human action; surfacing
// them as failing checks would drive the CI auto-fix loop into pointless
// attempts it can never satisfy. Ownership Enforcer and Proof Of Presence are
// their OWN policy types (confirmed live on ADO PR 16330529), NOT Status
// checks, so a Build/Status allow-list alone would exclude them only by
// accident - they are named here so the intent is explicit and robust.
var humanPolicyTypes = map[string]bool{
	"minimum number of reviewers": true,
	"required reviewers":          true,
	"comment requirements":        true,
	"require a merge strategy":    true,
	"work item linking":           true,
	"ownership enforcer":          true,
	"proof of presence":           true,
}

// humanStatusGate reports whether a "Status"-type policy is actually a human
// review / sign-off gate posted through the generic status surface rather than
// a content check. Code Review Compliance Policy (additional human sign-off on
// the latest iteration) arrives as a Status policy with a stable genre/name of
// (microsoft-policy-service, CodeReviewCompliancePolicy), so a naive
// Build/Status allow-list lets its blocking "rejected" pollute the content
// signal. Match on the stable (genre, name) identity, never the localizable
// displayName, so a renamed/localized gate is not misclassified.
func humanStatusGate(genre, name string) bool {
	switch {
	case strings.EqualFold(strings.TrimSpace(genre), "microsoft-policy-service") &&
		strings.EqualFold(strings.TrimSpace(name), "CodeReviewCompliancePolicy"):
		return true
	default:
		return false
	}
}

// isCICheck reports whether a policy evaluation represents a content-influenced
// automation check the CI monitor can meaningfully gate on and auto-fix, as
// opposed to a human review / sign-off / attestation gate. no-mistakes gates on
// content-influenced automation only - build/test/e2e build validation, code
// coverage, and Component Governance (security + license) status checks - whose
// result is a function of the PR's changed content. Human gates (see
// humanPolicyTypes and humanStatusGate) are excluded even when they report a
// blocking "rejected", because they are satisfied by human action in the ADO
// UI, not by anything the pipeline can change about the code.
//
// Classification is by stable policy identity, not a localizable display
// string: own-type human gates are matched by their type displayName (a stable
// English policy-type name in the az payload, unlike the user-facing
// settings.displayName), and Status-type human sign-off is matched by its
// (genre, name) status identity. Everything else that is a Build or Status
// policy stays a content check, preserving the fail-safe posture: an
// unrecognized gate is treated as content (surfaced), never silently dropped.
func (e policyEval) isCICheck() bool {
	typeName := strings.ToLower(strings.TrimSpace(e.Configuration.Type.DisplayName))
	if humanPolicyTypes[typeName] {
		return false
	}
	switch typeName {
	case "build":
		return true
	case "status":
		return !humanStatusGate(e.Configuration.Settings.StatusGenre, e.Configuration.Settings.StatusName)
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
	normalized := strings.ToLower(strings.TrimSpace(status))
	switch normalized {
	case "approved":
		return scm.CheckBucketPass
	case "rejected", "broken":
		return scm.CheckBucketFail
	case "queued", "running":
		return scm.CheckBucketPending
	case "notapplicable":
		// notApplicable is a path-scoped build policy whose evaluation does not
		// apply to this PR's changed paths - it is genuinely not
		// content-influenced, so it is correctly ignored (dropped) and never
		// gates CI.
		return ""
	case "":
		// No status reported at all: nothing to gate on, drop it.
		return ""
	default:
		// Any other NON-EMPTY status is one this switch does not recognize
		// (a future or provider-specific value). Unlike notApplicable it is not
		// known-irrelevant, so it must never be dropped as pass-equivalent:
		// surface it as pending so an unexpected status can never silently
		// vanish into an empty check list and manufacture a vacuous green. (B2)
		return scm.CheckBucketPending
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
