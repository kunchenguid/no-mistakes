package steps

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type fixExecutionOptions struct {
	RequirePreviousFindings bool
	MissingFindingsError    string
	LogMessage              string
	Prompt                  string
	ErrorPrefix             string
	FallbackSummary         string
	AfterAgentRun           func(*agent.Result) error
	// RunAgent overrides the agent-call seam while leaving preparation and
	// post-agent commit work on the step context. Review uses it to apply a
	// fresh review_agent_timeout stall budget at the instant each fixer starts.
	RunAgent func(agent.RunOpts) (*agent.Result, error)
	// SessionRole, when set, runs the fix turn in that durable review-loop
	// session (the review step's fixer role). Steps outside the review loop
	// leave it empty and stay session-isolated.
	SessionRole pipeline.SessionRole
	// Purpose labels the invocation for local performance telemetry.
	Purpose string
	// Workload records the bounded size of the change under fix for local
	// telemetry. Optional; nil leaves the invocation's workload unknown.
	Workload *agent.InvocationWorkload
	// ReportBlockedVerification asks the fixer for the optional
	// verification_blocked result field and logs it. Only the Review fix turn
	// sets it: it is the one fix prompt that orders edits before verification.
	ReportBlockedVerification bool
}

type commitSummary struct {
	Summary string `json:"summary"`
}

var errRejectedCommitSummary = errors.New("rejected commit summary")

const (
	// NoChangesAppliedSummary is the fix result of a round that changed
	// nothing; it is not a fix the pipeline applied.
	NoChangesAppliedSummary = "no changes applied"
	changesAppliedSummary   = "changes applied"
)

const fixerRemovalRule = `

Removal-first rule:
- When a problem can be solved by removing a code path that is not strictly required to satisfy the intent - an extra acceptance or matching branch, a fallback, an alias, a second definition of something the code already defines once, or handling for an input nobody intends - fix it by removing that path, not by validating, hardening, or documenting it. Judge what the intent strictly requires against the User intent section when present, otherwise against the change's own stated purpose. Later recorded human fix decisions supersede conflicting original intent. Removal is the smallest fix for such a path: hardening it leaves the unrequired path in place for the next review to find another hole in.`

// fixerPrompt wraps every shared fix-turn prompt with the two rules that apply
// to all of them: the removal-first rule and the limit on independently
// initiated memory-file edits. Review, Test, Lint, and custom-gate fix turns
// route through executeFixMode, and the Lint agent pass and the CI repair wrap
// their prompts the same way, so this is the insertion point for fix-turn rules.
func fixerPrompt(prompt string) string {
	return prompt + fixerRemovalRule + agent.MemoryFilesRule
}

const fixRoundHistoryRule = `

History rule:
- Leave the existing branch history exactly as it is: never rebase, reset, or amend, and do not check out another commit. Make your fixes in the working tree; the pipeline commits them on top of the current HEAD, and any commit of your own must also go on top of it. A fix round that rewrites history the pipeline already recorded is discarded and fails the run.`

// Committing a rewritten round would build on a history that dropped the
// recorded head, and failing without the restore would leave the rewrite
// checked out, where terminalization cannot verify the run's head.
func refuseHistoryRewrite(sctx *pipeline.StepContext, stepName types.StepName) error {
	recorded := strings.TrimSpace(sctx.Run.HeadSHA)
	if recorded == "" {
		return nil
	}
	ctx := sctx.Ctx
	head, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return fmt.Errorf("resolve head after %s fix round: %w", stepName, err)
	}
	leftRebase := rebaseInProgress(ctx, sctx.WorkDir)
	descends := head == recorded || isAncestor(ctx, sctx.WorkDir, recorded, head)
	if descends && !leftRebase {
		return nil
	}
	rewrite := fmt.Sprintf("HEAD %s does not descend from the run's recorded head %s", head, recorded)
	if descends {
		rewrite = fmt.Sprintf("a rebase was left in progress on top of the run's recorded head %s", recorded)
	}
	cause := fmt.Errorf("%s fix round rewrote branch history: %s; a fix round may only add commits on top (never rebase, reset, or amend), so its result was discarded and the worktree restored to %s", stepName, rewrite, recorded)
	if leftRebase {
		_, _ = git.Run(ctx, sctx.WorkDir, "rebase", "--abort")
	}
	return restorePreMergeHead(ctx, sctx, recorded, cause)
}

const (
	commitSubjectLimit      = 72
	minFixSummaryBudget     = 20
	fixSummaryBudgetProbe   = "x"
	fixSummaryRuleTemplate  = "- The summary must be one concise sentence fragment suitable for a git commit subject.\n- Keep the summary under 10 words and at most %d characters: the commit subject adds a prefix and must stay within %d characters."
	fixSummaryRuleUnbounded = "- The summary must be one concise sentence fragment suitable for a git commit subject.\n- Keep the summary under 10 words."
)

// fixSummaryRules renders the summary rules of a fix prompt, including the
// character budget the configured commit.fix_message leaves for the summary.
func fixSummaryRules(sctx *pipeline.StepContext, stepName types.StepName) string {
	rendered, err := sctx.Config.Commit.RenderFixMessageForBranch(stepName, fixSummaryBudgetProbe, sctx.Run.Branch)
	if err != nil {
		return fixSummaryRuleUnbounded
	}
	doubled, err := sctx.Config.Commit.RenderFixMessageForBranch(stepName, fixSummaryBudgetProbe+fixSummaryBudgetProbe, sctx.Run.Branch)
	if err != nil {
		return fixSummaryRuleUnbounded
	}
	occurrences := utf8.RuneCountInString(doubled) - utf8.RuneCountInString(rendered)
	if occurrences < 1 {
		return fixSummaryRuleUnbounded
	}
	fixed := utf8.RuneCountInString(rendered) - occurrences*len(fixSummaryBudgetProbe)
	budget := (commitSubjectLimit - fixed) / occurrences
	return fmt.Sprintf(fixSummaryRuleTemplate, max(budget, minFixSummaryBudget), commitSubjectLimit)
}

var commitSummarySchema = json.RawMessage(fmt.Sprintf(`{
	"type": "object",
	"properties": {
		"summary": {"type": "string", "maxLength": %d}
	},
	"required": ["summary"]
}`, config.MaxFixMessageSummaryBytes))

// maxVerificationBlockedBytes bounds the optional verification_blocked field
// of a fix result. The field only reaches the step log.
const maxVerificationBlockedBytes = 300

// fixSummarySchema is commitSummarySchema plus the optional verification_blocked
// field a fixer fills when the tool that would verify its edits is missing. It
// stays optional so a fixer with nothing to report omits it.
var fixSummarySchema = json.RawMessage(fmt.Sprintf(`{
	"type": "object",
	"properties": {
		"summary": {"type": "string", "maxLength": %d},
		"verification_blocked": {"type": "string", "maxLength": %d}
	},
	"required": ["summary"]
}`, config.MaxFixMessageSummaryBytes, maxVerificationBlockedBytes))

// blockedVerification returns the bounded, single-line verification_blocked
// text of a fix result, or "" when the agent reported none or its output does
// not parse. A parse failure is not worth failing a committed fix over: the
// summary extraction already reports unparsable output.
func blockedVerification(result *agent.Result) string {
	if result == nil || result.Output == nil {
		return ""
	}
	var parsed struct {
		VerificationBlocked string `json:"verification_blocked"`
	}
	if err := json.Unmarshal(result.Output, &parsed); err != nil {
		return ""
	}
	blocked := strings.Join(strings.Fields(parsed.VerificationBlocked), " ")
	if len(blocked) > maxVerificationBlockedBytes {
		blocked = strings.ToValidUTF8(blocked[:maxVerificationBlockedBytes], "")
	}
	return blocked
}

// logBlockedVerification makes a fix round whose verification could not run
// visible in the step log. A round that produced a commit, whether the
// pipeline committed the fixer's edits or the fixer ran `git commit` itself, is
// labelled unverified; a round that changed nothing is labelled distinctly from
// the ordinary "no agent changes to commit", which reads as "nothing needed
// fixing".
func logBlockedVerification(sctx *pipeline.StepContext, blocked string, produced bool) {
	switch {
	case blocked == "":
	case produced:
		sctx.Log(fmt.Sprintf("fixes committed unverified, verification blocked: %s", blocked))
	default:
		sctx.Log(fmt.Sprintf("agent made no edits and reported verification blocked: %s; the findings were not fixed", blocked))
	}
}

// hasBlockingFindings returns true if any finding has error or warning severity.
func hasBlockingFindings(items []Finding) bool {
	for _, f := range items {
		if f.Severity == "error" || f.Severity == "warning" {
			return true
		}
	}
	return false
}

// reviewedPathsCoverReviewable reports whether reviewedPaths (a review turn's
// self-reported coverage) exactly covers reviewablePaths. Comparison is by
// cleaned path so "./x" and "x" match.
func reviewedPathsCoverReviewable(reviewedPaths, reviewablePaths []string) bool {
	allowed := make(map[string]bool, len(reviewablePaths))
	for _, candidate := range reviewablePaths {
		normalized := normalizeReviewedPath(candidate)
		if normalized == "" {
			return false
		}
		allowed[normalized] = true
	}
	covered := make(map[string]bool, len(reviewedPaths))
	for _, reviewed := range reviewedPaths {
		normalized := normalizeReviewedPath(reviewed)
		if normalized == "" || !allowed[normalized] {
			return false
		}
		covered[normalized] = true
	}
	for candidate := range allowed {
		if !covered[candidate] {
			return false
		}
	}
	return true
}

// uncoveredReviewablePaths returns the reviewable paths that no reviewed_paths
// entry covers, in reviewable order. Out-of-scope entries cannot cover
// anything, so they are ignored here; the strict
// reviewedPathsCoverReviewable check still fails the round for them.
func uncoveredReviewablePaths(reviewedPaths, reviewablePaths []string) []string {
	covered := make(map[string]bool, len(reviewedPaths))
	for _, reviewed := range reviewedPaths {
		covered[normalizeReviewedPath(reviewed)] = true
	}
	var missing []string
	for _, candidate := range reviewablePaths {
		if !covered[normalizeReviewedPath(candidate)] {
			missing = append(missing, candidate)
		}
	}
	return missing
}

// hasInvalidReviewedPath reports whether a coverage record contains an entry
// that does not normalize to a path at all (empty, whitespace, or "."). Such
// an entry is invalid coverage evidence: reviewedPathsCoverReviewable fails on
// it, so the round can only park, and no further review can cure it.
func hasInvalidReviewedPath(reviewedPaths []string) bool {
	for _, reviewed := range reviewedPaths {
		if normalizeReviewedPath(reviewed) == "" {
			return true
		}
	}
	return false
}

// mergeReviewedPaths unions two coverage records, keeping each path's first
// spelling and order. It is the deterministic merge behind the focused
// coverage pass: the round's record is what its turns actually examined
// together, and the strict coverage check re-runs on the union.
//
// An entry that does not normalize to a path is kept, not folded away: it is
// invalid coverage evidence, and dropping it would let the completion turn
// launder the invalidity out of the union and certify a record that never had
// positive coverage. Keeping one such entry makes reviewedPathsCoverReviewable
// keep failing on the union, so the round parks with it named.
func mergeReviewedPaths(first, second []string) []string {
	seen := make(map[string]bool, len(first)+len(second))
	var merged []string
	invalidKept := false
	for _, path := range append(append([]string(nil), first...), second...) {
		key := normalizeReviewedPath(path)
		if key == "" {
			if invalidKept {
				continue
			}
			invalidKept = true
		} else {
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		merged = append(merged, path)
	}
	return merged
}

// uncoveredReviewMessage names why a clean review round is parked instead of
// certifying the head: the reviewable files its reviewed_paths did not cover
// (or the whole set when the field was omitted), and any path it claimed that
// is not a reviewable changed file.
func uncoveredReviewMessage(reviewedPaths, reviewablePaths []string) string {
	if reviewedPaths == nil {
		return fmt.Sprintf("review reported no reviewed_paths; parking for approval with %d reviewable file(s) unverified: %s", len(reviewablePaths), strings.Join(reviewablePaths, ", "))
	}
	allowed := make(map[string]bool, len(reviewablePaths))
	for _, candidate := range reviewablePaths {
		allowed[normalizeReviewedPath(candidate)] = true
	}
	covered := make(map[string]bool, len(reviewedPaths))
	var outOfScope []string
	for _, reviewed := range reviewedPaths {
		normalized := normalizeReviewedPath(reviewed)
		if normalized == "" {
			outOfScope = append(outOfScope, `""`)
			continue
		}
		if !allowed[normalized] {
			outOfScope = append(outOfScope, reviewed)
			continue
		}
		covered[normalized] = true
	}
	var missing []string
	for _, candidate := range reviewablePaths {
		if !covered[normalizeReviewedPath(candidate)] {
			missing = append(missing, candidate)
		}
	}
	msg := "review coverage is incomplete; parking for approval"
	if len(missing) > 0 {
		msg += fmt.Sprintf(" with %d reviewable file(s) unverified: %s", len(missing), strings.Join(missing, ", "))
	}
	if len(outOfScope) > 0 {
		msg += fmt.Sprintf("; %d reviewed_paths entry(ies) outside the reviewable set: %s", len(outOfScope), strings.Join(outOfScope, ", "))
	}
	return msg
}

func normalizeReviewedPath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	cleaned := path.Clean(value)
	if cleaned == "." {
		return ""
	}
	return cleaned
}

// assertPipelineHeadContinuity fails closed when the worktree HEAD is no longer
// equal to or a descendant of the head the pipeline itself last recorded
// (sctx.Run.HeadSHA). Every repository gate and every post-review core step
// calls this guard at entry, and commitAgentFixes calls it around commits that
// advance the recorded head.
//
// The pipeline advances HEAD only through its own commits, each of which updates
// sctx.Run.HeadSHA in lockstep. If HEAD has diverged from that recorded head -
// e.g. a concurrent process reset the shared worktree to a different commit -
// then the pipeline's recorded history is no longer in HEAD, and continuing
// could validate or ship a substituted tree. The whole job of this tool is to
// not lose people's code, so we refuse rather than proceed.
//
// Anchor integrity: sctx.Run.HeadSHA is the correct, un-clobberable anchor. It
// is the *recorded* head the pipeline itself produced at its last commit - held
// in the single daemon process's in-memory Run struct (one shared pointer per
// run, never re-read from the DB mid-pipeline) and written only by no-mistakes
// commit code (commit_fix / rebase / ci_fix / push). An out-of-band `git reset`
// mutates the worktree HEAD on disk but cannot touch this field, so at the check
// point the anchor still holds the reviewed head even after a clobber. The guard
// deliberately compares the *recorded* head against the *live* worktree HEAD
// (git.HeadSHA); it never derives the anchor from the mutable worktree, which
// would be circular and defeatable. Because the guard runs at every repository
// gate and post-review core-step entry, and at the very top of commitAgentFixes
// before any commit that would advance sctx.Run.HeadSHA, the next pipeline
// boundary after a clobber is caught while the anchor is still the pre-clobber
// pipeline head. The anchor can never advance into a clobbered lineage without
// first passing this check.
//
// This is what happened in run 01KXC3SD5NZYMERGDS68Z1C8ER: the review step
// committed a correct fix, a sibling worktree sharing the bare repo reset HEAD
// to a divergent commit that lacked it, and the document step committed on the
// clobber and shipped it. A forward-only agent commit (git rebase --continue,
// etc.) keeps the recorded head as an ancestor and is allowed; a divergent
// (sibling) reset or a backward reset both trip this guard. On any failure the
// step and the whole run abort (executor.failRun) before doing more work -
// nothing is committed or shipped.
func assertPipelineHeadContinuity(sctx *pipeline.StepContext, stepName types.StepName) error {
	recorded := strings.TrimSpace(sctx.Run.HeadSHA)
	if recorded == "" {
		return nil
	}
	currentHead, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return fmt.Errorf("resolve head before %s step: %w", stepName, err)
	}
	if currentHead == recorded {
		return nil
	}
	// Fail closed: refuse unless the recorded head is genuinely an ancestor of the
	// live HEAD (a legitimate forward move). A non-ancestor result OR any git error
	// (e.g. an unknown recorded object) aborts rather than proceeds.
	if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "merge-base", "--is-ancestor", recorded, currentHead); err != nil {
		return fmt.Errorf("refusing to run %s step: worktree HEAD %s is not a descendant of the pipeline's recorded head %s; "+
			"the reviewed change was rewritten out-of-band and would be lost - aborting to protect it",
			stepName, currentHead, recorded)
	}
	return nil
}

// commitPipelineCorrection creates a pipeline-authored correction commit with
// hook verification bypassed, and is the single owner of that bypass.
//
// A correction commit is machine-authored: the pipeline records the change its
// own agents or its own formatter produced, inside the throwaway run
// worktree. That worktree is freshly carved from the bare gate repo, so tracked
// hooks that depend on generated untracked runtime files cannot run there - the
// canonical case is a repository whose shared config sets core.hooksPath=.husky
// while a tracked .husky hook sources the generated .husky/_/husky.sh that no
// install step ever created in this worktree. The hook exits nonzero, the
// correction commit fails, and the whole run dies on setup state that says
// nothing about the change under review.
//
// --no-verify alone is not enough, because Git gates only pre-commit and
// commit-msg on it and always runs prepare-commit-msg (builtin/commit.c
// prepare_to_commit), so a repository carrying a legacy .husky
// prepare-commit-msg hook - commitizen and ticket-prefix setups are the common
// ones - still fails the exact commit this helper exists to complete. Pointing
// core.hooksPath at a freshly created empty directory for this one invocation
// covers the whole commit hook family; --no-verify is kept so the intent stays
// explicit at the call. The override lives only in this process argument list
// and the directory is removed afterwards, so nothing persists in the
// repository, the user's configuration, or the daemon's environment.
//
// Reach is deliberately narrow. Only commitAgentFixes (Review, Test, Document,
// Lint, and an operator-authorized repository gate repair) and the Push step's
// leftover-worktree commit route here. These are the two routes that commit the
// pipeline's own agent and formatter output.
// CI repair commits, the generic git runner, and every user-authored commit keep
// hook verification; the core pipeline and repository gates remain the
// authoritative quality checks for what these commits contain.
// pipelineIdentityEnv forces the author and committer for one git
// invocation. Environment entries win over ambient values where -c flags do
// not: git reads GIT_AUTHOR_* and GIT_COMMITTER_* ahead of the config file
// the -c flags override, so flags alone cannot pin the identity.
func pipelineIdentityEnv(name, email string) []string {
	return []string{
		"GIT_AUTHOR_NAME=" + name,
		"GIT_AUTHOR_EMAIL=" + email,
		"GIT_COMMITTER_NAME=" + name,
		"GIT_COMMITTER_EMAIL=" + email,
	}
}

// pipelineCommitIdentity is the author and committer the pipeline stamps on
// its own commits: the worktree's configured identity, which the daemon copied
// from the user's checkout at setup. A missing value fails the commit closed
// instead of falling back, because any invented author trips the same identity
// gate the foreign commit tripped. The recorded head's committer is the only
// fallback, for a worktree that configures no identity and could not commit
// otherwise.
func pipelineCommitIdentity(sctx *pipeline.StepContext, recordedHead string) (string, string, error) {
	name, err := stepGitRun(sctx, "config", "--get", "--default", "", "user.name")
	if err != nil {
		return "", "", fmt.Errorf("read pipeline commit identity: %w", err)
	}
	email, err := stepGitRun(sctx, "config", "--get", "--default", "", "user.email")
	if err != nil {
		return "", "", fmt.Errorf("read pipeline commit identity: %w", err)
	}
	if strings.TrimSpace(name) != "" && strings.TrimSpace(email) != "" {
		return strings.TrimSpace(name), strings.TrimSpace(email), nil
	}
	if strings.TrimSpace(recordedHead) != "" {
		ident, err := stepGitRun(sctx, "log", "-1", "--format=%cn%x00%ce", recordedHead)
		if err == nil {
			if parts := strings.Split(ident, "\x00"); len(parts) == 2 && strings.TrimSpace(parts[0]) != "" && strings.TrimSpace(parts[1]) != "" {
				return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), nil
			}
		}
	}
	return "", "", fmt.Errorf("pipeline commit identity is not configured for %s and its recorded head names none", sctx.WorkDir)
}

// normalizeRewrittenHead re-authors the narrow replacement shape: a head that
// rewrites the recorded head with a single new commit on an older parent,
// such as an amended tip. The amend runs in place, so the rewrite keeps its
// structure and the strategy checks still judge it as a rewrite. Anything
// wider, a rebase replay or a merge, belongs to those checks and passes
// through untouched.
func normalizeRewrittenHead(sctx *pipeline.StepContext, recorded, head string) error {
	count, err := stepGitRun(sctx, "rev-list", "--count", recorded+".."+head)
	if err != nil {
		return fmt.Errorf("measure rewritten head %s: %w", shortObjectID(head), err)
	}
	if strings.TrimSpace(count) != "1" {
		return nil
	}
	parents, err := stepGitRun(sctx, "rev-list", "--parents", "-n", "1", head)
	if err != nil {
		return fmt.Errorf("inspect rewritten head %s: %w", shortObjectID(head), err)
	}
	if len(strings.Fields(parents)) != 2 {
		return nil
	}
	name, email, err := pipelineCommitIdentity(sctx, recorded)
	if err != nil {
		return err
	}
	ident, err := stepGitRun(sctx, "log", "-1", "--format=%an%x00%ae%x00%cn%x00%ce", head)
	if err != nil {
		return fmt.Errorf("inspect rewritten head %s: %w", shortObjectID(head), err)
	}
	fields := strings.Split(ident, "\x00")
	if len(fields) == 4 && strings.TrimSpace(fields[0]) == name && strings.TrimSpace(fields[1]) == email &&
		strings.TrimSpace(fields[2]) == name && strings.TrimSpace(fields[3]) == email {
		return nil
	}
	if _, err := stepGitRunWithEnv(sctx, pipelineIdentityEnv(name, email), "-c", "user.name="+name, "-c", "user.email="+email, "commit", "--amend", "--no-edit", "--reset-author"); err != nil {
		return fmt.Errorf("re-author rewritten head %s: %w", shortObjectID(head), err)
	}
	sctx.Log(fmt.Sprintf("re-authored rewritten head %s with the pipeline identity", shortObjectID(head)))
	return nil
}

// normalizeAgentHead restamps an agent-advanced head that carries a foreign
// identity. A linear range collapses onto the recorded head with identical
// content, so the caller sees a dirty tree and commits it with the pipeline
// identity instead of adopting the foreign one. A merge in the range fails
// closed, and a rewritten head goes to normalizeRewrittenHead: relinking
// either would launder dropped or foreign-merged commits into a publishable
// head.
func normalizeAgentHead(sctx *pipeline.StepContext) error {
	recorded := strings.TrimSpace(sctx.Run.HeadSHA)
	if recorded == "" {
		return nil
	}
	head, err := stepGitHeadSHA(sctx)
	if err != nil {
		return fmt.Errorf("resolve head for identity gate: %w", err)
	}
	if head == recorded {
		return nil
	}
	if _, err := stepGitRun(sctx, "merge-base", "--is-ancestor", recorded, head); err != nil {
		return normalizeRewrittenHead(sctx, recorded, head)
	}
	revList, err := stepGitRun(sctx, "rev-list", "--reverse", recorded+".."+head)
	if err != nil {
		return fmt.Errorf("list agent commits for identity gate: %w", err)
	}
	commits := strings.Fields(revList)
	if len(commits) == 0 {
		return nil
	}
	name, email, err := pipelineCommitIdentity(sctx, recorded)
	if err != nil {
		return err
	}
	hasMerge := false
	foreign := false
	for _, c := range commits {
		parents, err := stepGitRun(sctx, "rev-list", "--parents", "-n", "1", c)
		if err != nil {
			return fmt.Errorf("inspect agent commit %s: %w", shortObjectID(c), err)
		}
		if len(strings.Fields(parents)) > 2 {
			hasMerge = true
		}
		ident, err := stepGitRun(sctx, "log", "-1", "--format=%an%x00%ae%x00%cn%x00%ce", c)
		if err != nil {
			return fmt.Errorf("inspect agent commit %s: %w", shortObjectID(c), err)
		}
		fields := strings.Split(ident, "\x00")
		if len(fields) != 4 || strings.TrimSpace(fields[0]) != name || strings.TrimSpace(fields[1]) != email ||
			strings.TrimSpace(fields[2]) != name || strings.TrimSpace(fields[3]) != email {
			foreign = true
		}
	}
	if !foreign {
		return nil
	}
	if hasMerge {
		return fmt.Errorf("refusing to adopt %s: it concludes a merge with a foreign identity, which needs an operator re-author", shortObjectID(head))
	}
	if _, err := stepGitRun(sctx, "reset", "--soft", recorded); err != nil {
		return fmt.Errorf("recollect agent commits onto %s: %w", shortObjectID(recorded), err)
	}
	sctx.Log(fmt.Sprintf("re-authored %d agent commit(s) with the pipeline identity; recommitting their content", len(commits)))
	return nil
}

// A proven merge carries base history nothing in this run wrote, so the gate
// judges only the merge line and leaves the merged-in parents out.
func normalizeProvenMergeHead(sctx *pipeline.StepContext, provenHead string) error {
	recorded := strings.TrimSpace(sctx.Run.HeadSHA)
	if recorded == "" {
		return nil
	}
	head, err := stepGitHeadSHA(sctx)
	if err != nil {
		return fmt.Errorf("resolve head for identity gate: %w", err)
	}
	if head != provenHead || head == recorded {
		return normalizeAgentHead(sctx)
	}
	if _, err := stepGitRun(sctx, "merge-base", "--is-ancestor", recorded, head); err != nil {
		return normalizeAgentHead(sctx)
	}
	parents, err := stepGitRun(sctx, "rev-list", "--parents", "-n", "1", head)
	if err != nil {
		return fmt.Errorf("inspect proven merge %s: %w", shortObjectID(head), err)
	}
	fields := strings.Fields(parents)
	if len(fields) <= 2 {
		return normalizeAgentHead(sctx)
	}
	args := append([]string{"rev-list", "--reverse", recorded + ".." + head, "--not"}, fields[2:]...)
	revList, err := stepGitRun(sctx, args...)
	if err != nil {
		return fmt.Errorf("list proven merge commits for identity gate: %w", err)
	}
	commits := strings.Fields(revList)
	if len(commits) == 0 {
		return nil
	}
	name, email, err := pipelineCommitIdentity(sctx, recorded)
	if err != nil {
		return err
	}
	hasMerge := false
	foreign := false
	for _, c := range commits {
		parents, err := stepGitRun(sctx, "rev-list", "--parents", "-n", "1", c)
		if err != nil {
			return fmt.Errorf("inspect agent commit %s: %w", shortObjectID(c), err)
		}
		if len(strings.Fields(parents)) > 2 {
			hasMerge = true
		}
		ident, err := stepGitRun(sctx, "log", "-1", "--format=%an%x00%ae%x00%cn%x00%ce", c)
		if err != nil {
			return fmt.Errorf("inspect agent commit %s: %w", shortObjectID(c), err)
		}
		idFields := strings.Split(ident, "\x00")
		if len(idFields) != 4 || strings.TrimSpace(idFields[0]) != name || strings.TrimSpace(idFields[1]) != email ||
			strings.TrimSpace(idFields[2]) != name || strings.TrimSpace(idFields[3]) != email {
			foreign = true
		}
	}
	if !foreign {
		return nil
	}
	if hasMerge {
		return fmt.Errorf("refusing to adopt %s: it concludes a merge with a foreign identity, which needs an operator re-author", shortObjectID(head))
	}
	if _, err := stepGitRun(sctx, "reset", "--soft", recorded); err != nil {
		return fmt.Errorf("recollect agent commits onto %s: %w", shortObjectID(recorded), err)
	}
	sctx.Log(fmt.Sprintf("re-authored %d agent commit(s) with the pipeline identity; recommitting their content", len(commits)))
	return nil
}

func commitPipelineCorrection(sctx *pipeline.StepContext, workDir, message string, logf func(string)) error {
	return commitPipelineCorrectionWithCleanup(sctx, workDir, message, logf, os.RemoveAll)
}

func commitPipelineCorrectionWithCleanup(
	sctx *pipeline.StepContext,
	workDir, message string,
	logf func(string),
	cleanup func(string) error,
) error {
	gitRun := func(args ...string) (string, error) { return stepGitRun(sctx, args...) }
	staged, err := stagedChangesPresent(gitRun)
	if err != nil {
		return fmt.Errorf("inspect staged correction: %w", err)
	}
	if !staged {
		return nil
	}

	name, email, err := pipelineCommitIdentity(sctx, "")
	if err != nil {
		return err
	}
	emptyHooksDir, err := os.MkdirTemp("", "no-mistakes-correction-hooks-")
	if err != nil {
		return fmt.Errorf("prepare hook-free commit environment: %w", err)
	}
	_, commitErr := stepGitRunWithEnv(sctx, pipelineIdentityEnv(name, email), "-c", "core.hooksPath="+emptyHooksDir, "-c", "user.name="+name, "-c", "user.email="+email, "commit", "--no-verify", "-m", message)
	if cleanupErr := cleanup(emptyHooksDir); cleanupErr != nil {
		if logf != nil {
			logf(fmt.Sprintf("warning: failed to remove temporary hook-free commit directory %s: %v", emptyHooksDir, cleanupErr))
		} else {
			slog.Warn("failed to remove temporary hook-free commit directory", "path", emptyHooksDir, "error", cleanupErr)
		}
	}
	return commitErr
}

// withCommitTrailers appends commit.trailers rendered for producer. Changes not
// attributable to a single invocation get none rather than a guessed agent.
func withCommitTrailers(sctx *pipeline.StepContext, message string, producer *agent.Result) (string, error) {
	if producer == nil || len(sctx.Config.Commit.Trailers) == 0 {
		return message, nil
	}
	// A fallback chain reports the agent that actually ran in Provider; a bare
	// adapter leaves it empty, and then the step agent is the one that ran.
	agentName := producer.Provider
	if agentName == "" && sctx.Agent != nil {
		agentName = sctx.Agent.Name()
	}
	trailers, err := sctx.Config.Commit.RenderTrailers(config.TrailerData{Agent: agentName, Model: producer.Model})
	if err != nil {
		return "", err
	}
	return message + "\n\n" + strings.Join(trailers, "\n"), nil
}

// stagedChangesPresent is the handoff between catch-all staging and commit.
// Worktree status can become stale when an agent completes a rebase itself, or
// can report dirt that `git add -A` cannot put in the superproject index. Only
// the staged index answers whether a correction commit is actually required.
func stagedChangesPresent(gitRun gitRunner) (bool, error) {
	staged, err := gitRun("diff", "--cached", "--name-only", "-z")
	if err != nil {
		return false, err
	}
	return staged != "", nil
}

func commitAgentFixes(sctx *pipeline.StepContext, stepName types.StepName, summary, fallbackSummary string) error {
	_, err := commitAgentFixesWithResult(sctx, stepName, summary, fallbackSummary, nil)
	return err
}

// commitAgentFixesWithResult commits the worktree changes left by producer, the
// agent invocation that made them. A nil producer commits without trailers.
func commitAgentFixesWithResult(sctx *pipeline.StepContext, stepName types.StepName, summary, fallbackSummary string, producer *agent.Result) (bool, error) {
	ctx := sctx.Ctx
	if err := assertPipelineHeadContinuity(sctx, stepName); err != nil {
		return false, err
	}
	render := func() (string, error) {
		return renderFixCommitMessage(sctx, stepName, summary, fallbackSummary, producer)
	}
	if err := conformAgentCommits(sctx, stepName, render); err != nil {
		return false, err
	}
	if err := normalizeAgentHead(sctx); err != nil {
		return false, err
	}
	status, err := git.Run(ctx, sctx.WorkDir, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("check %s changes: %w", stepName, err)
	}
	if strings.TrimSpace(status) == "" {
		sctx.Log("no agent changes to commit")
		headSHA, err := git.HeadSHA(ctx, sctx.WorkDir)
		if err != nil {
			return false, fmt.Errorf("resolve agent head: %w", err)
		}
		return false, recordAgentFixHead(sctx, stepName, headSHA)
	}
	commitMessage, err := render()
	if err != nil {
		return false, err
	}
	if err := stagePipelineChanges(sctx); err != nil {
		return false, fmt.Errorf("stage %s changes: %w", stepName, err)
	}
	headBeforeCommit, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return false, fmt.Errorf("resolve head before %s commit: %w", stepName, err)
	}
	if err := commitPipelineCorrection(sctx, sctx.WorkDir, commitMessage, sctx.Log); err != nil {
		return false, fmt.Errorf("commit %s changes: %w", stepName, err)
	}
	headSHA, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return false, fmt.Errorf("resolve head after %s commit: %w", stepName, err)
	}
	// An empty staged index is a successful no-op, not a commit. Reporting it
	// as one would claim a head advance that never happened.
	if headSHA == headBeforeCommit {
		sctx.Log("no staged agent changes to commit")
	} else {
		sctx.Log(fmt.Sprintf("committed agent fixes: %s", commitMessage))
	}
	if err := recordAgentFixHead(sctx, stepName, headSHA); err != nil {
		return false, err
	}
	return headSHA != headBeforeCommit, nil
}

// renderFixCommitMessage renders the commit.fix_message subject and the
// commit.trailers for producer, the single message every pipeline-published fix
// commit carries whether the pipeline or the agent created it.
func renderFixCommitMessage(sctx *pipeline.StepContext, stepName types.StepName, summary, fallbackSummary string, producer *agent.Result) (string, error) {
	if summary == "" {
		summary = fallbackSummary
	}
	if summary == "" {
		summary = "apply fixes"
	}
	message, err := sctx.Config.Commit.RenderFixMessageForBranch(stepName, summary, sctx.Run.Branch)
	if err != nil {
		return "", fmt.Errorf("render %s fix commit message: %w", stepName, err)
	}
	message, err = withCommitTrailers(sctx, message, producer)
	if err != nil {
		return "", fmt.Errorf("render %s fix commit trailers: %w", stepName, err)
	}
	return message, nil
}

func recordAgentFixHead(sctx *pipeline.StepContext, stepName types.StepName, headSHA string) error {
	if headSHA == sctx.Run.HeadSHA {
		return nil
	}
	if err := assertPipelineHeadContinuity(sctx, stepName); err != nil {
		return err
	}
	if err := updateNonSharedBranchRef(sctx, headSHA); err != nil {
		return err
	}
	startingHead := strings.TrimSpace(sctx.ReviewStartingHeadSHA)
	if startingHead == "" {
		startingHead = sctx.Run.HeadSHA
	}
	if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, headSHA); err != nil {
		return err
	}
	sctx.Run.HeadSHA = headSHA
	if stepName == types.StepReview {
		pipeline.PersistUncertifiedPipelineRange(sctx, startingHead, headSHA)
	}
	return nil
}

func fixResultSummary(committed bool) string {
	if committed {
		return changesAppliedSummary
	}
	return NoChangesAppliedSummary
}

func extractCommitSummary(result *agent.Result) (string, error) {
	var summary commitSummary
	if result.Output == nil {
		return "", fmt.Errorf("agent returned no structured summary")
	}
	if !utf8.Valid(result.Output) {
		return "", fmt.Errorf("%w: agent output must contain valid UTF-8", errRejectedCommitSummary)
	}
	if err := json.Unmarshal(result.Output, &summary); err != nil {
		return "", fmt.Errorf("parse commit summary: %w", err)
	}
	if len(summary.Summary) > config.MaxFixMessageSummaryBytes {
		return "", fmt.Errorf("%w: commit summary must not exceed %d bytes", errRejectedCommitSummary, config.MaxFixMessageSummaryBytes)
	}
	cleaned := strings.Trim(config.NormalizeFixMessageSummary(summary.Summary), " \t\r\n\"'.;:,-")
	return cleaned, nil
}

func executeFixMode(sctx *pipeline.StepContext, stepName types.StepName, opts fixExecutionOptions) (string, error) {
	if !sctx.Fixing {
		return "", nil
	}
	if opts.RequirePreviousFindings && sctx.PreviousFindings == "" {
		return "", errors.New(opts.MissingFindingsError)
	}
	if opts.LogMessage != "" {
		sctx.Log(opts.LogMessage)
	}
	purpose := opts.Purpose
	if purpose == "" {
		purpose = string(stepName) + "-fix"
	}
	schema := commitSummarySchema
	if opts.ReportBlockedVerification {
		schema = fixSummarySchema
	}
	runOpts := agent.RunOpts{
		Prompt:     fixerPrompt(opts.Prompt) + fixRoundHistoryRule,
		CWD:        sctx.WorkDir,
		JSONSchema: schema,
		OnChunk:    sctx.LogChunk,
		Purpose:    purpose,
		Workload:   opts.Workload,
	}
	// The fixer may commit its own work and leave a clean worktree, which
	// commitAgentFixesWithResult reports as "not committed". HEAD movement is
	// what tells that round apart from one that made no edits.
	var err error
	headBeforeAgent := ""
	if opts.ReportBlockedVerification {
		headBeforeAgent, err = git.HeadSHA(sctx.Ctx, sctx.WorkDir)
		if err != nil {
			return "", fmt.Errorf("resolve head before %s fix: %w", stepName, err)
		}
	}
	var result *agent.Result
	if opts.RunAgent != nil {
		result, err = opts.RunAgent(runOpts)
	} else {
		result, err = sctx.RunAgentSessionContext(sctx.Ctx, opts.SessionRole, runOpts)
	}
	if err != nil {
		if opts.ErrorPrefix == "" {
			return "", err
		}
		return "", fmt.Errorf("%s: %w", opts.ErrorPrefix, err)
	}
	if err := refuseHistoryRewrite(sctx, stepName); err != nil {
		return "", err
	}
	if opts.AfterAgentRun != nil {
		if err := opts.AfterAgentRun(result); err != nil {
			return "", err
		}
	}
	summary, err := extractCommitSummary(result)
	if err != nil {
		if errors.Is(err, errRejectedCommitSummary) {
			return "", fmt.Errorf("validate %s fix summary: %w", stepName, err)
		}
		sctx.Log(fmt.Sprintf("warning: could not parse fix summary: %v", err))
	}
	committed, err := commitAgentFixesWithResult(sctx, stepName, summary, opts.FallbackSummary, result)
	if err != nil {
		return "", err
	}
	if opts.ReportBlockedVerification {
		headAfterAgent, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
		if err != nil {
			return "", fmt.Errorf("resolve head after %s fix: %w", stepName, err)
		}
		logBlockedVerification(sctx, blockedVerification(result), committed || headAfterAgent != headBeforeAgent)
	}
	return fixResultSummary(committed), nil
}

func updateNonSharedBranchRef(sctx *pipeline.StepContext, headSHA string) error {
	shared, err := worktreeSharesGateRefs(sctx)
	if err != nil || shared {
		return err
	}
	if _, err := stepGitRun(sctx, "update-ref", normalizedBranchRef(sctx.Run.Branch), headSHA); err != nil {
		return fmt.Errorf("update local branch ref: %w", err)
	}
	return nil
}

func worktreeSharesGateRefs(sctx *pipeline.StepContext) (bool, error) {
	if strings.TrimSpace(sctx.GateDir) == "" {
		return false, nil
	}
	gateInfo, err := os.Stat(sctx.GateDir)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect gate ref storage: %w", err)
	}
	commonDir, err := stepGitRun(sctx, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return false, fmt.Errorf("resolve worktree ref storage: %w", err)
	}
	commonInfo, err := os.Stat(commonDir)
	if err != nil {
		return false, fmt.Errorf("inspect worktree ref storage: %w", err)
	}
	return os.SameFile(gateInfo, commonInfo), nil
}
