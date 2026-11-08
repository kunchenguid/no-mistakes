package steps

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// noChecksGracePeriod is how long a PR head must report zero checks before
// the monitor asks whether anything could ever report one. It only gates the
// probe: elapsed time alone is never evidence, and a repository with CI that
// can report keeps waiting however long registration takes.
const noChecksGracePeriod = 5 * time.Minute

// noChecksConfiguredFindingID names the ask-user finding the CI step parks on
// when a PR has no checks and nothing in the repository can report one.
const noChecksConfiguredFindingID = "ci-no-checks-configured"

// errCIPresenceUnsupported marks a host the probe cannot reason about. The
// monitor keeps waiting on an empty check list there, as it always has.
var errCIPresenceUnsupported = errors.New("no CI presence probe for this provider")

// ciPresence is the probe's answer for one PR head and base tip.
type ciPresence struct {
	// configured is true when something can still report a check on the PR:
	// a workflow whose triggers can attach a check to the head, a third-party
	// CI configuration file, or a check the forge requires before merge.
	configured bool
	// evidence names what was found (configured) or what was ruled out.
	evidence string
}

// ciPresenceProbe caches one probe answer, keyed by the head and base tip it
// read, so a PR that keeps reporting zero checks is not re-probed every poll.
type ciPresenceProbe struct {
	head, baseTip string
	result        ciPresence
}

// thirdPartyCIConfigs are repository-root entries that configure a CI service
// able to post commit statuses or check runs to a GitHub PR. Their presence is
// enough to keep waiting: this probe cannot see whether the service is wired
// to the repository, so it assumes it is.
var thirdPartyCIConfigs = map[string]bool{
	".appveyor.yml":        true,
	".azure-pipelines":     true,
	".buildkite":           true,
	".circleci":            true,
	".cirrus.star":         true,
	".cirrus.yml":          true,
	".drone.yml":           true,
	".gitlab-ci.yml":       true,
	".semaphore":           true,
	".travis.yml":          true,
	".woodpecker":          true,
	".woodpecker.yml":      true,
	"Jenkinsfile":          true,
	"appveyor.yml":         true,
	"azure-pipelines.yaml": true,
	"azure-pipelines.yml":  true,
	"bitrise.yml":          true,
	"buildkite.yml":        true,
	"codefresh.yml":        true,
}

// nonPRWorkflowEvents are GitHub Actions events whose runs GitHub attaches to
// a commit other than a pull request's head (the default branch tip, a tag,
// or nothing at all). A workflow whose every trigger is in this set cannot
// put a check on a PR head on its own. Every other event - pull_request,
// pull_request_target, push, merge_group, the review and deployment events,
// workflow_run, check_run, status, and any event this list does not name -
// counts as able to report, so an unfamiliar trigger keeps the monitor
// waiting instead of parking it.
var nonPRWorkflowEvents = map[string]bool{
	"branch_protection_rule": true,
	"create":                 true,
	"delete":                 true,
	"discussion":             true,
	"discussion_comment":     true,
	"fork":                   true,
	"gollum":                 true,
	"issue_comment":          true,
	"issues":                 true,
	"label":                  true,
	"member":                 true,
	"milestone":              true,
	"page_build":             true,
	"project":                true,
	"project_card":           true,
	"project_column":         true,
	"public":                 true,
	"registry_package":       true,
	"release":                true,
	"repository_dispatch":    true,
	"schedule":               true,
	"watch":                  true,
	"workflow_call":          true,
	"workflow_dispatch":      true,
}

// probeCIPresence decides whether anything can report a check on a PR whose
// head reports none. It answers "not configured" only on positive evidence
// from every source it reads; anything it cannot read is an error, which the
// monitor treats as "keep waiting".
//
// The workflow and configuration files are read from both the delivered head
// and the PR base's current tip: a pull_request workflow runs from the merge
// of the two, and pull_request_target from the base. The forge side is the
// set of checks the base branch requires before merge.
func probeCIPresence(sctx *pipeline.StepContext, host scm.Host, baseBranch, headSHA, baseTip string) (ciPresence, error) {
	if host.Provider() != scm.ProviderGitHub {
		return ciPresence{}, errCIPresenceUnsupported
	}
	reader, ok := host.(scm.RequiredChecksReader)
	if !ok {
		return ciPresence{}, errCIPresenceUnsupported
	}
	if strings.TrimSpace(headSHA) == "" || strings.TrimSpace(baseTip) == "" {
		return ciPresence{}, errors.New("head or base tip is unknown")
	}
	trees := []string{headSHA}
	if baseTip != headSHA {
		trees = append(trees, baseTip)
	}
	for _, tree := range trees {
		found, err := ciConfiguredInTree(sctx, tree)
		if err != nil {
			return ciPresence{}, err
		}
		if found != "" {
			return ciPresence{configured: true, evidence: found}, nil
		}
	}
	required, err := reader.RequiredChecks(sctx.Ctx, baseBranch)
	if err != nil {
		return ciPresence{}, err
	}
	if len(required) > 0 {
		return ciPresence{configured: true, evidence: fmt.Sprintf("%s requires checks before merge: %s", baseBranch, strings.Join(required, ", "))}, nil
	}
	return ciPresence{evidence: fmt.Sprintf("no GitHub Actions workflow at the PR head or %s's tip triggers on an event that reports on a pull request, no third-party CI configuration is present, and %s requires no checks before merge", baseBranch, baseBranch)}, nil
}

// ciConfiguredInTree returns a description of the first thing in the commit's
// tree able to report a check on a PR, or "" when there is nothing.
func ciConfiguredInTree(sctx *pipeline.StepContext, commit string) (string, error) {
	root, err := stepGitRun(sctx, "ls-tree", "--name-only", commit)
	if err != nil {
		return "", err
	}
	for _, name := range strings.Split(root, "\n") {
		if thirdPartyCIConfigs[strings.TrimSpace(name)] {
			return fmt.Sprintf("third-party CI configuration %s at %s", strings.TrimSpace(name), shortSHA(commit)), nil
		}
	}
	listing, err := stepGitRun(sctx, "ls-tree", "-r", "--name-only", commit, "--", ".github/workflows")
	if err != nil {
		return "", err
	}
	var workflows []string
	for _, file := range strings.Split(listing, "\n") {
		file = strings.TrimSpace(file)
		switch strings.ToLower(path.Ext(file)) {
		case ".yml", ".yaml":
			workflows = append(workflows, file)
		}
	}
	sort.Strings(workflows)
	for _, file := range workflows {
		content, err := stepGitRunRaw(sctx, "show", commit+":"+file)
		if err != nil {
			return "", err
		}
		if event, reports := workflowReportsOnPR([]byte(content)); reports {
			return fmt.Sprintf("workflow %s at %s triggers on %s", file, shortSHA(commit), event), nil
		}
	}
	return "", nil
}

// workflowReportsOnPR reports whether a GitHub Actions workflow file has a
// trigger able to attach a check to a pull request's head, naming the first
// such trigger. A file it cannot read as a workflow counts as reporting, so
// a malformed or unfamiliar file keeps the monitor waiting.
func workflowReportsOnPR(content []byte) (string, bool) {
	var doc yaml.Node
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return "an unparseable workflow file", true
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return "an unrecognized workflow shape", true
	}
	var on *yaml.Node
	mapping := doc.Content[0]
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		// YAML 1.1 readers turn a bare `on` key into true; GitHub reads it as
		// the trigger list, and so does this, by its literal text.
		if key := mapping.Content[i]; key.Kind == yaml.ScalarNode && key.Value == "on" {
			on = mapping.Content[i+1]
			break
		}
	}
	if on == nil {
		return "no declared trigger", true
	}
	var events []string
	switch on.Kind {
	case yaml.ScalarNode:
		events = []string{on.Value}
	case yaml.SequenceNode:
		for _, item := range on.Content {
			if item.Kind != yaml.ScalarNode {
				return "an unrecognized trigger list", true
			}
			events = append(events, item.Value)
		}
	case yaml.MappingNode:
		for i := 0; i < len(on.Content); i += 2 {
			if on.Content[i].Kind != yaml.ScalarNode {
				return "an unrecognized trigger map", true
			}
			events = append(events, on.Content[i].Value)
		}
	default:
		return "an unrecognized trigger", true
	}
	if len(events) == 0 {
		return "an empty trigger list", true
	}
	for _, event := range events {
		if !nonPRWorkflowEvents[strings.TrimSpace(event)] {
			return event, true
		}
	}
	return "", false
}

// noChecksConfiguredPark runs (or reuses) the CI presence probe for a head that
// has reported zero checks for the grace period. It returns the park outcome
// only when the probe positively found nothing able to report a check; every
// other answer - CI is configured, the provider has no probe, the base tip or
// any source could not be read - returns nil and the monitor keeps waiting.
func (s *CIStep) noChecksConfiguredPark(sctx *pipeline.StepContext, host scm.Host, baseBranch, headSHA, baseTip, prURL string, waited time.Duration) *pipeline.StepOutcome {
	if s.presence == nil || s.presence.head != headSHA || s.presence.baseTip != baseTip {
		if strings.TrimSpace(baseTip) == "" {
			sctx.Log("warning: could not check whether CI is configured: the PR base tip is unresolved")
			return nil
		}
		result, err := probeCIPresence(sctx, host, baseBranch, headSHA, baseTip)
		if errors.Is(err, errCIPresenceUnsupported) {
			return nil
		}
		if err != nil {
			sctx.Log(fmt.Sprintf("warning: could not check whether CI is configured: %v", err))
			return nil
		}
		s.presence = &ciPresenceProbe{head: headSHA, baseTip: baseTip, result: result}
		if result.configured {
			sctx.Log(fmt.Sprintf("CI is configured (%s); still waiting for its checks to register", result.evidence))
		}
	}
	if s.presence.result.configured {
		return nil
	}
	sctx.Log(fmt.Sprintf("no CI can report on this PR (%s), parking for a decision", s.presence.result.evidence))
	return ciTerminalRepairOutcome(ciNoChecksConfiguredOutcome(prURL, waited, s.presence.result.evidence), Findings{}, sctx.DeferredFindings)
}

// ciNoChecksConfiguredOutcome parks the CI step when a PR has had no checks
// for the grace period and the probe found nothing able to report one. It is
// never a pass: an approval is recorded as an override naming the missing
// checks, and `no_ci: true` stays the maintainer's durable declaration.
func ciNoChecksConfiguredOutcome(prURL string, waited time.Duration, evidence string) *pipeline.StepOutcome {
	findings := Findings{
		Summary: "no CI is configured to report checks on this PR",
		Items: []Finding{{
			ID:       noChecksConfiguredFindingID,
			Severity: "warning",
			Description: fmt.Sprintf("No CI checks registered for %s after %s, and nothing in the repository can report one: %s. "+
				"Approve to finish the CI step without hosted checks (recorded as an override), "+
				"or declare `no_ci: true` in the default branch's .no-mistakes.yaml so later runs treat a PR with zero checks as ready. "+
				"If a CI service this probe cannot see (a GitHub App or webhook integration) is expected to report, wait for its checks and then approve.",
				prURL, waited.Round(time.Second), evidence),
			Action: types.ActionAskUser,
		}},
	}
	encoded, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{NeedsApproval: true, Findings: string(encoded)}
}
