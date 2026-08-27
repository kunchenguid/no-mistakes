package steps

import (
	"fmt"
	"os"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/safepath"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// prePushCheckOutputMaxBytes bounds how much of a blocking check's output is
// quoted back in the refusal error. The refusal travels through step errors,
// the TUI, and the run record, so the quoted tail stays small; the complete
// output is always written to the step log first.
const prePushCheckOutputMaxBytes = 4 * 1024

// prePushTarget describes the remote branch a push is about to move, plus the
// open pull request that branch is known to belong to. It is the decision
// context handed to a repository's pre_push_check command.
type prePushTarget struct {
	Ref        string
	Branch     string
	BaseBranch string
	// HeadSHA is the commit the pipeline is about to publish.
	HeadSHA string
	// RemoteSHA is the commit the remote branch (and therefore the open PR)
	// currently points at. This is the value an external merge process has
	// already read, approved, or started building.
	RemoteSHA string
	// PRURL and PRNumber identify the open pull request on this branch when
	// no-mistakes could resolve one. Both are empty when the forge could not be
	// consulted; the check command can still look the PR up itself from the
	// branch name and the worktree it runs in.
	PRURL    string
	PRNumber string
}

func (t prePushTarget) env() []string {
	return []string{
		"NO_MISTAKES_REF=" + t.Ref,
		"NO_MISTAKES_BRANCH=" + t.Branch,
		"NO_MISTAKES_BASE_BRANCH=" + t.BaseBranch,
		"NO_MISTAKES_HEAD_SHA=" + t.HeadSHA,
		"NO_MISTAKES_REMOTE_SHA=" + t.RemoteSHA,
		"NO_MISTAKES_PR_URL=" + t.PRURL,
		"NO_MISTAKES_PR_NUMBER=" + t.PRNumber,
	}
}

// describe names the thing being pushed under, preferring the pull request
// identity when one was resolved so the refusal reads the way the operator
// thinks about it.
func (t prePushTarget) describe() string {
	switch {
	case t.PRNumber != "" && t.PRURL != "":
		return fmt.Sprintf("pull request #%s (%s)", t.PRNumber, t.PRURL)
	case t.PRURL != "":
		return fmt.Sprintf("pull request %s", t.PRURL)
	case t.PRNumber != "":
		return fmt.Sprintf("pull request #%s", t.PRNumber)
	default:
		return fmt.Sprintf("existing remote branch %s", t.Branch)
	}
}

// prePushCheckBlockedError reports that the repository's configured
// pre_push_check refused this push. It is deliberately a distinct type: the
// push did not fail, it was declined by a policy the repository asked for, and
// the message has to say so plainly enough that the operator knows the fix is
// to wait or to talk to the external process rather than to retry.
type prePushCheckBlockedError struct {
	command  string
	target   prePushTarget
	exitCode int
	output   string
}

func (e *prePushCheckBlockedError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b,
		"refusing to push: this repository's pre_push_check declined to move %s from %s to %s (%q exited with code %d). "+
			"The branch was left untouched. This guard exists because an external merge process - a merge queue, a batching merge bot, a release train - "+
			"may already own the open pull request, and pushing a new head underneath it invalidates whatever that process has in flight. "+
			"Re-run the gate once the pull request is no longer held, or clear the hold and push manually if this update is intended.",
		e.target.describe(), shortObjectID(e.target.RemoteSHA), shortObjectID(e.target.HeadSHA), e.command, e.exitCode,
	)
	if output := strings.TrimSpace(e.output); output != "" {
		b.WriteString("\npre_push_check output: ")
		b.WriteString(output)
	}
	return b.String()
}

// runConfiguredPrePushCheck runs the repository's pre_push_check before the
// push step moves a branch that already exists on the push remote, and turns a
// non-zero exit into a refusal.
//
// The check is scoped to a push that changes the head of an ALREADY-EXISTING
// remote branch, because that is the only push that can land underneath an
// open pull request. Creating the branch for the first time (decision.newBranch)
// cannot: there is nothing on the remote yet, so there is no pull request and
// no external process to disturb. A push whose head the remote already carries
// (decision.upToDate) moves nothing at all. Both are skipped, so opening a
// brand-new PR is never gated by this hook.
//
// An unset pre_push_check is a complete no-op: no forge lookup, no subprocess,
// no behavior change.
//
// The command runs with sctx.AppRoot (the no-mistakes app root) as its
// working directory, never sctx.WorkDir. pre_push_check is trusted-only
// precisely because it is a security veto a repository relies on to protect
// an externally owned pull request, unlike commands.{test,lint,format}, whose
// entire job is to validate the pushed content itself. Running it inside the
// pushed worktree would let a contributor shadow a repo-relative script
// (e.g. the documented `pre_push_check: "scripts/merge-queue-hold.sh"`) with
// their own branch content and defeat the guard with the daemon's own
// credentials. A repository-relative script therefore will not resolve; the
// check must name an absolute path or a PATH-resolved binary.
func runConfiguredPrePushCheck(sctx *pipeline.StepContext, decision forcePushDecision, target prePushTarget) error {
	if sctx.Config == nil {
		return nil
	}
	command := strings.TrimSpace(sctx.Config.PrePushCheck)
	if command == "" {
		return nil
	}
	if decision.newBranch {
		sctx.Log("skipping pre_push_check: creating a new remote branch, so no existing pull request can be pushed under")
		return nil
	}
	if decision.upToDate {
		sctx.Log("skipping pre_push_check: the remote branch already points at this head, so nothing moves")
		return nil
	}

	target.RemoteSHA = decision.remoteSHA
	target.PRURL, target.PRNumber = resolvePrePushPRIdentity(sctx, target.Branch)
	if actual := livePRBaseBranch(sctx, target.PRURL, target.PRNumber); actual != "" {
		target.BaseBranch = actual
	}

	checkDir := strings.TrimSpace(sctx.AppRoot)
	if checkDir == "" {
		return fmt.Errorf("pre_push_check %q: no app root available to run the check outside the pushed worktree", command)
	}

	sctx.Log(fmt.Sprintf("running pre_push_check before updating %s: %s", target.describe(), command))
	output, exitCode, err := runShellCommandWithProcessEnv(sctx.Ctx, checkDir, prePushCheckEnv(sctx, target), command)
	if err != nil {
		return fmt.Errorf("run pre_push_check %q: %w", command, err)
	}
	if strings.TrimSpace(output) != "" {
		// The complete output belongs in the step log; only a clamped, redacted
		// copy travels with the refusal.
		sctx.LogFile(output)
	}
	if exitCode != 0 {
		return &prePushCheckBlockedError{
			command:  command,
			target:   target,
			exitCode: exitCode,
			output:   redactPrePushOutput(output),
		}
	}
	sctx.Log("pre_push_check passed")
	return nil
}

// prePushCheckEnv layers the decision context on top of the environment the
// step would otherwise run a configured command with, so the check keeps the
// daemon's PATH, credentials, and any step-scoped overrides.
func prePushCheckEnv(sctx *pipeline.StepContext, target prePushTarget) []string {
	base := stepEnvironment(sctx)
	if base == nil {
		base = os.Environ()
	}
	return overrideEnv(base, target.env())
}

// redactPrePushOutput clamps the check's output for embedding in the refusal
// and scrubs the two things the rest of this package already refuses to
// publish: credential-bearing URLs and the operator's home directory.
func redactPrePushOutput(output string) string {
	output = strings.TrimSpace(output)
	if len(output) > prePushCheckOutputMaxBytes {
		output = truncateTextAtLineBoundary(output, prePushCheckOutputMaxBytes, "[pre_push_check output truncated; the complete output is in the push step log]")
	}
	return safepath.RedactText(safeurl.RedactText(output))
}

// resolvePrePushPRIdentity finds the open pull request the branch about to be
// pushed belongs to. It prefers the run's own durably recorded PR URL and
// otherwise asks the forge, which is what makes the identity available on the
// FIRST push of a run against an already-open pull request - the exact push
// this hook exists to guard.
//
// Every failure is best-effort and non-fatal: the check still runs with empty
// PR fields. A forge that is unreachable, unauthenticated, or unsupported must
// not be able to silently disable a guard the repository asked for, and a
// command that needs the PR identity can look it up itself.
func resolvePrePushPRIdentity(sctx *pipeline.StepContext, branch string) (prURL, prNumber string) {
	if sctx.Run != nil && sctx.Run.PRURL != nil {
		if recorded := strings.TrimSpace(*sctx.Run.PRURL); recorded != "" {
			return recorded, prNumberFromURL(recorded)
		}
	}
	host, skipReason := buildHost(sctx, resolvedProvider(sctx))
	if host == nil {
		sctx.LogFile(fmt.Sprintf("pre_push_check: pull request identity unavailable: %s", skipReason))
		return "", ""
	}
	if err := host.Available(sctx.Ctx); err != nil {
		sctx.LogFile(fmt.Sprintf("pre_push_check: pull request identity unavailable: %v", err))
		return "", ""
	}
	pr, err := host.FindPR(sctx.Ctx, branch, "")
	if err != nil {
		sctx.LogFile(fmt.Sprintf("pre_push_check: pull request lookup for %s failed: %v", branch, err))
		return "", ""
	}
	if pr == nil {
		return "", ""
	}
	number := strings.TrimSpace(pr.Number)
	url := strings.TrimSpace(pr.URL)
	if number == "" && url != "" {
		number = prNumberFromURL(url)
	}
	return url, number
}

// prePushBaseBranch reports the configured integration base branch as a
// fallback for when no open pull request (or no live-readable base) is known
// yet. Once a pull request exists, runConfiguredPrePushCheck overrides this
// with its actual live base branch via livePRBaseBranch, matching the
// precedence the CI step already applies: a since-changed pr.base_branch must
// not misdescribe an existing pull request's real target.
func prePushBaseBranch(sctx *pipeline.StepContext) string {
	return effectivePRBaseBranch(sctx)
}

// livePRBaseBranch asks the forge for the open pull request's actual base
// branch, when one is known. An already-open pull request's live target is
// authoritative over a since-changed pr.base_branch for the same reason the
// CI step prefers it (see effectivePRBaseBranch callers in ci.go): the check
// exists to describe the branch's real target to the guard, not a
// hypothetical one a later config edit selected. Best effort: an unsupported
// provider, an unreachable forge, or a lookup failure leaves the caller's
// existing value untouched rather than blocking the check.
func livePRBaseBranch(sctx *pipeline.StepContext, prURL, prNumber string) string {
	if prURL == "" && prNumber == "" {
		return ""
	}
	host, skipReason := buildHost(sctx, resolvedProvider(sctx))
	if host == nil {
		sctx.LogFile(fmt.Sprintf("pre_push_check: live base branch unavailable: %s", skipReason))
		return ""
	}
	reader, ok := host.(scm.PRBaseBranchReader)
	if !ok {
		return ""
	}
	actual, err := reader.GetPRBaseBranch(sctx.Ctx, &scm.PR{Number: prNumber, URL: prURL})
	if err != nil {
		sctx.LogFile(fmt.Sprintf("pre_push_check: live base branch lookup failed: %v", err))
		return ""
	}
	return strings.TrimSpace(actual)
}

// prNumberFromURL extracts the trailing numeric segment every supported
// forge's PR/MR URL ends with, and returns "" for anything else rather than
// guessing.
func prNumberFromURL(prURL string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(prURL), "/")
	idx := strings.LastIndex(trimmed, "/")
	if idx < 0 || idx+1 >= len(trimmed) {
		return ""
	}
	candidate := trimmed[idx+1:]
	for _, r := range candidate {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return candidate
}
