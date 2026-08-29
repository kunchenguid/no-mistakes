package steps

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type lastFixedIssues struct {
	Checks         []string `json:"checks,omitempty"`
	MergeConflict  bool     `json:"mergeConflict,omitempty"`
	ReviewComments []string `json:"reviewComments,omitempty"`
}

const maxCIFindingsBytes = 64 * 1024

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

// hasFailingChecks returns true if any CI check is in the fail bucket.
func hasFailingChecks(checks []scm.Check) bool {
	for _, c := range checks {
		if c.Failing() {
			return true
		}
	}
	return false
}

// hasPendingChecks returns true if any CI check is still running or queued.
func hasPendingChecks(checks []scm.Check) bool {
	for _, c := range checks {
		if c.Pending() {
			return true
		}
	}
	return false
}

func hasUnresolvedChecks(checks []scm.Check) bool {
	for _, c := range checks {
		switch c.Bucket {
		case scm.CheckBucketPass, scm.CheckBucketFail, scm.CheckBucketSkip:
		default:
			return true
		}
	}
	return false
}

func allChecksPassed(checks []scm.Check) bool {
	if len(checks) == 0 {
		return false
	}
	for _, c := range checks {
		if c.Bucket != scm.CheckBucketPass && c.Bucket != scm.CheckBucketSkip {
			return false
		}
	}
	return true
}

// failingCheckNames returns the names of failing checks.
func failingCheckNames(checks []scm.Check) []string {
	var names []string
	for _, c := range checks {
		if c.Failing() {
			names = append(names, c.Name)
		}
	}
	return names
}

// terminalFailureCompletionTimes snapshots when each terminally failed check
// finished, so a later poll can tell that CI has re-run since the fix push.
//
// It covers the whole terminal-failure set rather than just the fail bucket
// because a cancelled check can be a fix target too (see the CI step's
// fixTargets). Keyed on the fail bucket alone would leave a
// cancelled-only fix round with no completion evidence at all, and the step
// would then have no way to notice its own re-run.
func terminalFailureCompletionTimes(checks []scm.Check) map[string]time.Time {
	completedAt := make(map[string]time.Time)
	for _, c := range checks {
		if !checkFailedTerminally(c) {
			continue
		}
		if c.CompletedAt.IsZero() {
			continue
		}
		previous := completedAt[c.Name]
		if previous.IsZero() || c.CompletedAt.After(previous) {
			completedAt[c.Name] = c.CompletedAt
		}
	}
	if len(completedAt) == 0 {
		return nil
	}
	return completedAt
}

func terminalFailureCompletedAfter(checks []scm.Check, after map[string]time.Time) bool {
	if len(after) == 0 {
		return false
	}
	for _, c := range checks {
		if !checkFailedTerminally(c) || c.CompletedAt.IsZero() {
			continue
		}
		previous, ok := after[c.Name]
		if ok && c.CompletedAt.After(previous) {
			return true
		}
	}
	return false
}

func pendingCheckMatchesLastFixed(checks []scm.Check, lastFixedChecks string) bool {
	issues, ok := decodeLastFixedChecks(lastFixedChecks)
	if !ok {
		return false
	}

	failedNames := map[string]struct{}{}
	for _, name := range issues.Checks {
		if name == "" {
			continue
		}
		failedNames[name] = struct{}{}
	}
	if len(failedNames) == 0 {
		return issues.MergeConflict && hasPendingChecks(checks)
	}

	for _, c := range checks {
		if !c.Pending() {
			continue
		}
		if _, ok := failedNames[c.Name]; ok {
			return true
		}
	}

	return false
}

func encodeLastFixedChecks(failing []string, mergeConflict bool, optionalReviews ...[]scm.ReviewComment) string {
	var reviewComments []scm.ReviewComment
	if len(optionalReviews) > 0 {
		reviewComments = optionalReviews[0]
	}
	var commentKeys []string
	for _, c := range reviewComments {
		commentKeys = append(commentKeys, reviewCommentKey(c))
	}
	sort.Strings(commentKeys)
	if len(failing) == 0 && !mergeConflict && len(commentKeys) == 0 {
		return ""
	}
	encoded, err := json.Marshal(lastFixedIssues{
		Checks:         failing,
		MergeConflict:  mergeConflict,
		ReviewComments: commentKeys,
	})
	if err != nil {
		return ""
	}
	return string(encoded)
}

func reviewCommentKey(c scm.ReviewComment) string {
	key := strings.TrimSpace(c.ID)
	if key == "" {
		key = fmt.Sprintf("%s:%s:%d", c.Author, c.Path, c.Line)
	}
	return key
}

func reviewCommentsMatchingKey(comments []scm.ReviewComment, raw string) []scm.ReviewComment {
	issues, ok := decodeLastFixedChecks(raw)
	if !ok || len(issues.ReviewComments) == 0 {
		return nil
	}
	allowed := make(map[string]bool, len(issues.ReviewComments))
	for _, key := range issues.ReviewComments {
		allowed[key] = true
	}
	matched := make([]scm.ReviewComment, 0, len(comments))
	for _, comment := range comments {
		if allowed[reviewCommentKey(comment)] {
			matched = append(matched, comment)
		}
	}
	return matched
}

func decodeLastFixedChecks(raw string) (lastFixedIssues, bool) {
	if raw == "" {
		return lastFixedIssues{}, false
	}
	var issues lastFixedIssues
	if err := json.Unmarshal([]byte(raw), &issues); err != nil {
		return lastFixedIssues{}, false
	}
	if len(issues.Checks) == 0 && !issues.MergeConflict && len(issues.ReviewComments) == 0 {
		return lastFixedIssues{}, false
	}
	return issues, true
}

func sanitizeReviewFindingText(text string) string {
	text = ansi.Strip(text)
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, text)
}

func reviewProviderErrorSummary(err error) string {
	if err == nil {
		return ""
	}
	text := sanitizeReviewFindingText(err.Error())
	text = safeurl.RedactText(text)
	text = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return ' '
		default:
			return r
		}
	}, text)
	return trimCommentBody(text, maxCommentBodyBytes)
}

func selectedReviewComments(comments []scm.ReviewComment, previousFindings string) []scm.ReviewComment {
	if len(comments) == 0 || strings.TrimSpace(previousFindings) == "" {
		return nil
	}
	findings, err := types.ParseFindingsJSON(previousFindings)
	if err != nil {
		return nil
	}
	selectedIDs := make(map[string]bool)
	selectedIdentifiers := make(map[string]bool)
	selectedOmittedAggregate := false
	selectedOmittedExclusions := make(map[string]bool)
	selectedDetails := make(map[string]bool)
	for _, finding := range findings.Items {
		if strings.HasPrefix(finding.ID, "review-comment-") {
			selectedIDs[finding.ID] = true
		}
		for _, identifier := range finding.ReviewCommentTargets.IDs() {
			if identifier != "" {
				selectedIdentifiers[identifier] = true
			}
		}
		if finding.ReviewCommentAggregate || finding.ID == "review-comments-omitted" {
			if finding.ReviewCommentAggregate {
				selectedOmittedAggregate = true
				for _, identifier := range finding.ReviewCommentExclusions.IDs() {
					selectedOmittedExclusions[identifier] = true
				}
			} else {
				for _, identifier := range omittedReviewCommentIdentifiers(finding.Description) {
					selectedIdentifiers[identifier] = true
				}
			}
		}
		if finding.File != "" && strings.HasPrefix(finding.Description, "unresolved PR review comment from ") {
			selectedDetails[fmt.Sprintf("%s\x00%d\x00%s", finding.File, finding.Line, finding.Description)] = true
		}
	}
	matched := make([]scm.ReviewComment, 0, len(comments))
	for _, comment := range comments {
		finding := reviewCommentFinding(comment)
		include := selectedOmittedAggregate && !selectedOmittedExclusions[reviewCommentIdentifier(comment)]
		if (finding.ID != "" && selectedIDs[finding.ID]) ||
			selectedIdentifiers[reviewCommentIdentifier(comment)] ||
			selectedDetails[fmt.Sprintf("%s\x00%d\x00%s", finding.File, finding.Line, finding.Description)] {
			include = true
		}
		if include {
			matched = append(matched, comment)
		}
	}
	return matched
}

func omittedReviewCommentIdentifiers(description string) []string {
	const prefix = " (identifiers: "
	start := strings.Index(description, prefix)
	if start < 0 {
		return nil
	}
	rest := description[start+len(prefix):]
	end := strings.LastIndex(rest, ")")
	if end < 0 {
		return nil
	}
	identifiers := make([]string, 0)
	for _, identifier := range strings.Split(rest[:end], ",") {
		identifier = strings.TrimSpace(identifier)
		if identifier == "" || strings.HasSuffix(identifier, "... [truncated]") {
			continue
		}
		identifiers = append(identifiers, identifier)
	}
	return identifiers
}

func reviewCommentFinding(c scm.ReviewComment) Finding {
	loc := c.Path
	if c.Line > 0 {
		loc = fmt.Sprintf("%s:%d", c.Path, c.Line)
	}
	author := strings.TrimSpace(c.Author)
	if author == "" {
		author = "review bot"
	}
	description := fmt.Sprintf("unresolved PR review comment from @%s on %s", author, loc)
	if body := trimCommentBody(c.Body, maxCommentBodyBytes); body != "" {
		description += ": " + body
	}
	if c.URL != "" {
		description += fmt.Sprintf(" (see %s)", c.URL)
	}
	description = sanitizeReviewFindingText(description)
	finding := Finding{
		Severity:    "warning",
		File:        sanitizeReviewFindingText(c.Path),
		Line:        c.Line,
		Description: description,
		Action:      types.ActionAskUser,
	}
	if c.ID != "" {
		finding.ID = "review-comment-" + c.ID
	}
	return finding
}

func reviewCommentIdentifier(c scm.ReviewComment) string {
	if id := strings.TrimSpace(c.ID); id != "" {
		return id
	}
	loc := strings.TrimSpace(c.Path)
	if c.Line > 0 {
		loc = fmt.Sprintf("%s:%d", c.Path, c.Line)
	}
	if loc == "" {
		return "unknown"
	}
	return loc
}

func reviewCommentsOmittedFinding(comments, excluded []scm.ReviewComment) Finding {
	identifiers := make([]string, 0, len(comments))
	for _, comment := range comments {
		identifiers = append(identifiers, sanitizeReviewFindingText(reviewCommentIdentifier(comment)))
	}
	excludedIdentifiers := make([]string, 0, len(excluded))
	for _, comment := range excluded {
		excludedIdentifiers = append(excludedIdentifiers, reviewCommentIdentifier(comment))
	}
	var exclusions types.ReviewCommentExclusions
	if len(excludedIdentifiers) > 0 {
		encoded, _ := json.Marshal(excludedIdentifiers)
		exclusions = types.ReviewCommentExclusions(string(encoded))
	}
	description := fmt.Sprintf("%d additional unresolved PR review comments omitted from gate details", len(comments))
	if len(identifiers) > 0 {
		description += fmt.Sprintf(" (identifiers: %s)", trimCommentBody(strings.Join(identifiers, ", "), maxCommentBodyBytes))
	}
	description = sanitizeReviewFindingText(description)
	return Finding{
		ID:                      "review-comments-omitted",
		Severity:                "warning",
		Description:             description,
		Action:                  types.ActionAskUser,
		ReviewCommentAggregate:  true,
		ReviewCommentExclusions: exclusions,
	}
}

func ciFindingsOmittedFinding(findings []Finding) Finding {
	details := make([]string, 0, len(findings))
	for _, finding := range findings {
		detail := strings.TrimSpace(finding.Description)
		if finding.File != "" {
			location := finding.File
			if finding.Line > 0 {
				location = fmt.Sprintf("%s:%d", finding.File, finding.Line)
			}
			detail = fmt.Sprintf("%s: %s", location, detail)
		}
		if detail != "" {
			details = append(details, sanitizeReviewFindingText(detail))
		}
	}
	description := fmt.Sprintf("%d CI findings omitted from gate details", len(findings))
	if len(details) > 0 {
		description += fmt.Sprintf(" (details: %s)", trimCommentBody(strings.Join(details, "; "), maxCommentBodyBytes))
	}
	return Finding{
		ID:          "ci-findings-omitted",
		Severity:    "warning",
		Description: sanitizeReviewFindingText(description),
		Action:      types.ActionAskUser,
	}
}

func marshalCIFindingsWithinLimit(findings Findings, reviewComments []scm.ReviewComment) []byte {
	if len(reviewComments) == 0 {
		encoded, _ := json.Marshal(findings)
		return encoded
	}
	baseItems := append([]Finding(nil), findings.Items...)
	retained := make([]Finding, 0, len(reviewComments))
	omittedAt := len(reviewComments)
	var encoded []byte
	for i, comment := range reviewComments {
		items := make([]Finding, 0, len(baseItems)+len(retained)+1)
		items = append(items, baseItems...)
		items = append(items, retained...)
		items = append(items, reviewCommentFinding(comment))
		findings.Items = items
		encoded, _ = json.Marshal(findings)
		if len(encoded) > maxCIFindingsBytes {
			omittedAt = i
			break
		}
		retained = append(retained, reviewCommentFinding(comment))
	}
	if omittedAt == len(reviewComments) {
		return encoded
	}

	for {
		items := make([]Finding, 0, len(baseItems)+len(retained)+1)
		items = append(items, baseItems...)
		items = append(items, retained...)
		items = append(items, reviewCommentsOmittedFinding(reviewComments[len(retained):], reviewComments[:len(retained)]))
		findings.Items = items
		encoded, _ = json.Marshal(findings)
		if len(encoded) <= maxCIFindingsBytes {
			return encoded
		}
		if len(retained) == 0 {
			break
		}
		retained = retained[:len(retained)-1]
	}

	findings.Items = []Finding{reviewCommentsOmittedFinding(reviewComments, nil)}
	if len(baseItems) > 0 {
		findings.Items = append([]Finding{ciFindingsOmittedFinding(baseItems)}, findings.Items...)
	}
	encoded, _ = json.Marshal(findings)
	return encoded
}

func ciFailureOutcome(failing []string, mergeConflict bool, reviewComments []scm.ReviewComment, summary string) *pipeline.StepOutcome {
	if len(failing) == 0 && !mergeConflict && len(reviewComments) > 0 {
		switch summary {
		case "CI timed out with known failures still present":
			summary = "CI monitoring timed out with unresolved PR review comments"
		case "CI fix produced no changes - failures require manual intervention":
			summary = "CI fix produced no changes - PR review comments require manual intervention"
		case "CI failures still present after auto-fix attempts":
			summary = "PR review comments still present after auto-fix attempts"
		default:
			summary = "PR review comments require manual intervention"
		}
	}
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
	findingsJSON := marshalCIFindingsWithinLimit(findings, reviewComments)
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		Findings:      string(findingsJSON),
	}
}

// consecutiveCheckErrorLimit bounds consecutive GetChecks failures before the
// CI step parks at an ask-user gate. At the default 30s poll this is ~3 minutes
// of a provider read that keeps failing, making a broken gh (e.g. < v2.50, which
// rejects `gh pr checks --json`) an actionable stop instead of an invisible
// spin to ci_timeout.
const consecutiveCheckErrorLimit = 6

func ciCheckReadFailureOutcome(err error, reviewComments []scm.ReviewComment) *pipeline.StepOutcome {
	findings := Findings{
		Summary: "CI checks could not be read from the provider",
		Items: []Finding{{
			Severity:    "warning",
			Description: fmt.Sprintf("CI checks could not be read from the provider: %v. Verify that the provider CLI or credentials are installed, authenticated, and support the required check-reading command. For GitHub errors involving 'pr checks --json', gh >= 2.50 is required.", err),
			Action:      types.ActionAskUser,
		}},
	}
	findingsJSON := marshalCIFindingsWithinLimit(findings, reviewComments)
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		Findings:      string(findingsJSON),
	}
}

// ciFixAgentTimeoutOutcome parks the CI step for a decision after the auto-fix
// agent burned its whole invocation budget without finishing.
//
// The previous behaviour downgraded this to a log warning and let the poll loop
// issue the same request again on the next tick, up to auto_fix.ci attempts -
// each one another full agent budget, all of it invisible except for warning
// lines inside the CI step log, until ci_timeout ended the run hours later with
// nothing to act on. That is the same invisible spin consecutiveCheckErrorLimit
// exists to prevent, and repeating an invocation that has already proven it
// cannot finish is a blind retry, not a recovery.
//
// Parking instead is bounded (one budget, then a decision), keeps the run and
// its worktree alive rather than tearing them down, and leaves any further
// attempt to the operator, who can respond with a fix selection to spend
// another budget deliberately.
func ciFixAgentTimeoutOutcome(issueDesc string, dirtyWorktree string, err error, reviewTargets []scm.ReviewComment) *pipeline.StepOutcome {
	description := fmt.Sprintf(
		"The CI auto-fix agent did not finish within its invocation budget while repairing: %s. "+
			"Reported: %v. Re-running the same request costs another full budget, so no further attempt is made automatically. "+
			"Check that the configured agent CLI is authenticated and responsive, then respond with a fix selection to spend another budget, or resolve the CI failure outside the pipeline.",
		issueDesc, err)
	if dirtyWorktree != "" {
		description += fmt.Sprintf(" The timed-out agent left uncommitted changes in the run worktree at %s; they are not committed or pushed.", dirtyWorktree)
	}
	timeoutFinding := Finding{
		Severity:    "warning",
		Description: description,
		Action:      types.ActionAskUser,
	}
	identifiers := make([]string, 0, len(reviewTargets))
	for _, comment := range reviewTargets {
		if identifier := reviewCommentIdentifier(comment); identifier != "" {
			identifiers = append(identifiers, identifier)
		}
	}
	if len(identifiers) > 0 {
		encoded, _ := json.Marshal(identifiers)
		timeoutFinding.ReviewCommentTargets = types.ReviewCommentExclusions(string(encoded))
	}
	findings := Findings{
		Summary: "CI auto-fix agent exceeded its invocation budget",
		Items:   []Finding{timeoutFinding},
	}
	findingsJSON, _ := json.Marshal(findings)
	if len(findingsJSON) > maxCIFindingsBytes && len(identifiers) > 0 {
		timeoutFinding.ReviewCommentTargets = ""
		timeoutFinding.ReviewCommentAggregate = true
		findings.Items = []Finding{timeoutFinding}
		findingsJSON, _ = json.Marshal(findings)
	}
	if len(findingsJSON) > maxCIFindingsBytes {
		timeoutFinding.Description = trimCommentBody(timeoutFinding.Description, maxCIFindingsBytes-1024)
		findings.Items = []Finding{timeoutFinding}
		findingsJSON, _ = json.Marshal(findings)
	}
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		Findings:      string(findingsJSON),
	}
}

func ciHeadMismatchOutcome(expected, observed string) *pipeline.StepOutcome {
	findings := Findings{
		Summary: "PR head no longer matches the commit this run delivered",
		Items: []Finding{{
			Severity:    "warning",
			Description: fmt.Sprintf("PR head changed: expected head %s, observed %s on the pull request", expected, observed),
			Action:      types.ActionAskUser,
		}},
	}
	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		Findings:      string(findingsJSON),
	}
}

func ciReviewReadFailureOutcome(err error) *pipeline.StepOutcome {
	errorSummary := reviewProviderErrorSummary(err)
	findings := Findings{
		Summary: "PR review comments could not be read from the provider",
		Items: []Finding{{
			Severity:    "warning",
			Description: fmt.Sprintf("PR review comments could not be read from the provider: %s. Verify that the provider CLI or credentials are authenticated and have permissions to read pull request reviews.", errorSummary),
			Action:      types.ActionAskUser,
		}},
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

func ciMonitoringTimeoutOutcome() *pipeline.StepOutcome {
	findings := Findings{
		Summary: "CI monitoring timed out before PR was merged or closed",
		Items: []Finding{{
			Severity:    "warning",
			Description: "PR was still open when CI monitoring timed out",
			Action:      types.ActionAskUser,
		}},
	}
	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		Findings:      string(findingsJSON),
	}
}
