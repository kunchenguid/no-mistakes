package steps

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Bounds on the review-bot comment findings one observation may carry. The
// findings payload rides the IPC event stream, and one oversized frame kills
// the whole subscription (see the executor's approval-park comment), so a bot
// that left hundreds of comments is summarized past this point rather than
// rendered in full.
const (
	maxReviewBotCommentFindings = 50
	maxReviewBotCommentBytes    = 2 * 1024
)

// ciIssues is one settled observation of the pull request: what the monitor
// concluded is wrong with the head once every check finished and every
// authorized transient rerun was spent. It is the input to the classifier
// that turns each issue into a finding carrying its action.
type ciIssues struct {
	checks []scm.Check
	// failing is the sorted list of fail-bucket check names. It may carry a
	// name more than once when same-named checks fail together.
	failing []string
	// unresolvedCancelled is the sorted list of provider-attributed checks
	// that no rerun is going to replace (see cancelledAfterRerun and
	// cancelledWithoutRerun).
	unresolvedCancelled []string
	mergeConflict       bool
	// reruns reports how many transient reruns this run spent on a check.
	reruns func(string) int
	// botComments are the unresolved review-thread comments left by
	// registered review bots, fetched only when such a bot's check is red.
	botComments []scm.ReviewComment
}

// ciObservationFindings converts one settled observation into findings, one
// per issue, each carrying the action the executor's shared findings
// machinery acts on:
//
//   - a failing check the provider attributes to the job itself is an
//     auto-fix error: the repair half fetches its logs by provider identity and the fix
//     agent, not this classifier, decides whether the code caused it (its
//     no-code-change conclusion parks for a decision);
//   - a merge conflict is an auto-fix error whose repair always revalidates;
//   - a failing check published by a registered review bot (scm.ReviewBots)
//     is the bot's opinion about the change, not a verdict on it, so it
//     becomes one ask-user warning per unresolved bot comment, anchored to
//     the file and line the comment is about;
//   - a provider-attributed outcome no rerun will replace is an ask-user
//     warning, exactly as before findings existed: nothing a fix agent does
//     can clear it.
//
// The classification reads provider structure only - bucket, state, the
// check suite's app identity - never check names or log text, so it is as
// trustworthy as the status API behind it. An empty app identity is never a
// review bot.
func ciObservationFindings(issues ciIssues) Findings {
	var items []Finding
	codeChecks, botChecks := 0, 0
	for _, check := range selectedFailingChecks(issues.checks, issues.failing) {
		if bot, ok := scm.ReviewBotForApp(check.App); ok {
			botChecks++
			items = append(items, reviewBotFindings(check, bot, issues.botComments)...)
			continue
		}
		codeChecks++
		items = append(items, Finding{
			Severity:    types.FindingSeverityError,
			Action:      types.ActionAutoFix,
			Category:    types.FindingCategoryCICheck,
			Check:       check.Name,
			CheckID:     check.ProviderID,
			Description: ciCheckDescription(check),
		})
	}
	if issues.mergeConflict {
		items = append(items, Finding{
			Severity:    types.FindingSeverityError,
			Action:      types.ActionAutoFix,
			Category:    types.FindingCategoryCIMergeConflict,
			Description: "PR has merge conflicts with the base branch",
		})
	}
	transient, transientSummary := unresolvedTransientFindings(issues.unresolvedCancelled, issues.checks, issues.reruns)
	items = append(items, transient...)

	var parts []string
	switch codeChecks {
	case 0:
	case 1:
		parts = append(parts, "1 CI check failing")
	default:
		parts = append(parts, fmt.Sprintf("%d CI checks failing", codeChecks))
	}
	if issues.mergeConflict {
		parts = append(parts, "PR has merge conflicts with the base branch")
	}
	if botChecks > 0 {
		parts = append(parts, reviewBotSummary(items))
	}
	if len(transient) > 0 {
		parts = append(parts, transientSummary)
	}
	return Findings{Summary: strings.Join(parts, "; "), Items: items}
}

// ciObservationOutcome is the step outcome for a settled observation. A
// blocking severity parks the step unless the executor auto-fixes first;
// AutoFixable is what lets the executor's auto_fix.ci loop pick up the
// auto-fix findings, and it is false when nothing here is one, so a poll
// with only ask-user findings never starts a round.
func ciObservationOutcome(findings Findings) *pipeline.StepOutcome {
	encoded, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{
		NeedsApproval: hasBlockingFindings(findings.Items),
		AutoFixable:   hasAutoFixFindings(findings.Items),
		Findings:      string(encoded),
	}
}

func hasAutoFixFindings(items []Finding) bool {
	for _, item := range items {
		if item.ActionOrDefault() == types.ActionAutoFix {
			return true
		}
	}
	return false
}

func selectedFailingChecks(checks []scm.Check, names []string) []scm.Check {
	used := make([]bool, len(checks))
	selected := make([]scm.Check, 0, len(names))
	for _, name := range names {
		matched := false
		for i, check := range checks {
			if used[i] || !check.Failing() || check.Name != name {
				continue
			}
			selected = append(selected, check)
			used[i] = true
			matched = true
			break
		}
		if !matched {
			selected = append(selected, scm.Check{Name: name, Bucket: scm.CheckBucketFail})
		}
	}
	return selected
}

// ciCheckDescription keeps the "CI check failing: <name>" prefix every earlier
// CI gate used, then adds the provider's own state and details link so a human
// reading the gate can open the check without leaving the finding.
func ciCheckDescription(check scm.Check) string {
	description := fmt.Sprintf("CI check failing: %s", check.Name)
	if state := strings.ToLower(strings.TrimSpace(check.State)); state != "" {
		description += fmt.Sprintf(" - provider reported %s", state)
	}
	if link := strings.TrimSpace(check.Link); link != "" {
		description += " - " + link
	}
	return description
}

// reviewBotFindings renders a red review-bot check as one ask-user finding
// per unresolved comment the bot left, so the human decides comment by
// comment exactly as they do for the review step's own findings, and a
// selected comment reaches the fix agent with its file and line. A red check
// with no unresolved comment still needs a decision, so it becomes one
// finding of its own rather than disappearing.
func reviewBotFindings(check scm.Check, bot scm.ReviewBot, comments []scm.ReviewComment) []Finding {
	var items []Finding
	omitted := 0
	for _, comment := range comments {
		author, ok := scm.ReviewBotForLogin(comment.Author)
		if !ok || author.AppSlug != bot.AppSlug {
			continue
		}
		if len(items) >= maxReviewBotCommentFindings {
			omitted++
			continue
		}
		body := strings.TrimSpace(comment.Body)
		if len(body) > maxReviewBotCommentBytes {
			body = body[:maxReviewBotCommentBytes] + "..."
		}
		items = append(items, Finding{
			Severity:    types.FindingSeverityWarning,
			Action:      types.ActionAskUser,
			Category:    types.FindingCategoryCIReviewBot,
			Check:       check.Name,
			CheckID:     check.ProviderID,
			File:        comment.Path,
			Line:        comment.Line,
			Description: fmt.Sprintf("%s: %s", strings.TrimSpace(comment.Author), body),
		})
	}
	if omitted > 0 {
		items = append(items, Finding{
			Severity:    types.FindingSeverityWarning,
			Action:      types.ActionAskUser,
			Category:    types.FindingCategoryCIReviewBot,
			Check:       check.Name,
			CheckID:     check.ProviderID,
			Description: fmt.Sprintf("%s: %d more unresolved review comments were omitted from this gate; read them on the pull request", check.Name, omitted),
		})
	}
	if len(items) == 0 {
		description := fmt.Sprintf("Review bot check failing: %s", check.Name)
		if link := strings.TrimSpace(check.Link); link != "" {
			description += " - " + link
		}
		description += " - no unresolved review comments were found, so decide whether to proceed"
		items = append(items, Finding{
			Severity:    types.FindingSeverityWarning,
			Action:      types.ActionAskUser,
			Category:    types.FindingCategoryCIReviewBot,
			Check:       check.Name,
			CheckID:     check.ProviderID,
			Description: description,
		})
	}
	return items
}

func reviewBotSummary(items []Finding) string {
	byCheck := map[string]int{}
	var order []string
	for _, item := range items {
		if item.Category != types.FindingCategoryCIReviewBot {
			continue
		}
		if _, ok := byCheck[item.Check]; !ok {
			order = append(order, item.Check)
		}
		byCheck[item.Check]++
	}
	parts := make([]string, 0, len(order))
	for _, name := range order {
		count := byCheck[name]
		if count == 1 {
			parts = append(parts, fmt.Sprintf("review bot check %s needs a decision (1 finding)", name))
			continue
		}
		parts = append(parts, fmt.Sprintf("review bot check %s needs a decision (%d findings)", name, count))
	}
	return strings.Join(parts, "; ")
}

// reviewBotComments fetches the unresolved review-bot comments when a
// registered review bot's check is red and the host can supply them. It is
// best effort: an unreadable comment list leaves the bot's check finding
// without its comments rather than failing the observation.
func reviewBotComments(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR, checks []scm.Check) []scm.ReviewComment {
	botRed := false
	for _, check := range checks {
		if _, ok := scm.ReviewBotForApp(check.App); ok && check.Failing() {
			botRed = true
			break
		}
	}
	if !botRed || !host.Capabilities().ReviewComments {
		return nil
	}
	rch, ok := host.(scm.ReviewCommentsHost)
	if !ok {
		return nil
	}
	comments, err := rch.GetReviewComments(sctx.Ctx, pr)
	if err != nil && err != scm.ErrUnsupported {
		sctx.Log(fmt.Sprintf("warning: could not read review bot comments: %v", err))
		return nil
	}
	return comments
}

// ciFixTargets is what a CI fix round repairs: the findings the executor
// selected for it (the auto-fix subset of the last observation, or whatever
// the human selected at the gate), reduced to the exact checks whose logs the
// repair fetches and whether a merge conflict is among them.
type ciFixTargets struct {
	Findings      Findings
	Checks        []scm.CheckTarget
	MergeConflict bool
}

func parseCIFixTargets(raw string) (ciFixTargets, error) {
	if strings.TrimSpace(raw) == "" {
		return ciFixTargets{}, nil
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return ciFixTargets{}, err
	}
	targets := ciFixTargets{Findings: findings}
	seen := map[string]bool{}
	for _, item := range findings.Items {
		if item.Category == types.FindingCategoryCIMergeConflict {
			targets.MergeConflict = true
		}
		name := strings.TrimSpace(item.Check)
		id := strings.TrimSpace(item.CheckID)
		if name == "" || id != "" && seen[id] {
			continue
		}
		if id != "" {
			seen[id] = true
		}
		targets.Checks = append(targets.Checks, scm.CheckTarget{Name: name, ProviderID: id})
	}
	sort.Slice(targets.Checks, func(i, j int) bool {
		if targets.Checks[i].Name == targets.Checks[j].Name {
			return targets.Checks[i].ProviderID < targets.Checks[j].ProviderID
		}
		return targets.Checks[i].Name < targets.Checks[j].Name
	})
	return targets, nil
}

func (t ciFixTargets) empty() bool { return len(t.Findings.Items) == 0 }

// description names the round's targets the way the CI step log always has.
func (t ciFixTargets) checkNames() []string {
	names := make([]string, 0, len(t.Checks))
	for _, check := range t.Checks {
		names = append(names, check.Name)
	}
	return names
}

func (t ciFixTargets) description() string {
	desc := strings.Join(t.checkNames(), ", ")
	switch {
	case t.MergeConflict && desc != "":
		desc += " + merge conflict"
	case t.MergeConflict:
		desc = "merge conflict"
	case desc == "":
		count := len(t.Findings.Items)
		if count == 1 {
			return "1 selected finding"
		}
		desc = fmt.Sprintf("%d selected findings", count)
	}
	return desc
}

// ciRepairParkOutcome parks the step over the findings a repair round could
// not clear, re-labelled ask-user: the fix agent has already concluded that
// no code change is warranted, or the repair could not be settled, so
// letting the auto_fix.ci loop pick the same findings up again would spend a
// round on a question only a human can answer. summary carries the agent's
// conclusion or the settlement error.
func ciRepairParkOutcome(findings Findings, deferredRaw, summary string) *pipeline.StepOutcome {
	parked := types.FindingsMetadata(findings)
	parked.Summary = summary
	for _, item := range findings.Items {
		item.Action = types.ActionAskUser
		parked.Items = append(parked.Items, item)
	}
	if deferred, err := types.ParseFindingsJSON(deferredRaw); err == nil {
		parked.Items = append(parked.Items, deferred.Items...)
	}
	encoded, _ := json.Marshal(parked)
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		Findings:      string(encoded),
	}
}
