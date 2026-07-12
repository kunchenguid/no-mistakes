package steps

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
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
	// SessionRole, when set, runs the fix turn in that durable review-loop
	// session (the review step's fixer role). Steps outside the review loop
	// leave it empty and stay session-isolated.
	SessionRole pipeline.SessionRole
	// Purpose labels the invocation for local performance telemetry.
	Purpose string
	// Workload records the bounded size of the change under fix for local
	// telemetry. Optional; nil leaves the invocation's workload unknown.
	Workload *agent.InvocationWorkload
}

type commitSummary struct {
	Summary string `json:"summary"`
}

var commitSummarySchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"summary": {"type": "string"}
	},
	"required": ["summary"]
}`)

// hasBlockingFindings returns true if any finding has error or warning severity.
func hasBlockingFindings(items []Finding) bool {
	for _, f := range items {
		if f.Severity == "error" || f.Severity == "warning" {
			return true
		}
	}
	return false
}

// assertPipelineHeadContinuity fails closed when the worktree HEAD is no longer
// a descendant of the head the pipeline itself last recorded (sctx.Run.HeadSHA).
//
// The pipeline advances HEAD only through its own commits, each of which updates
// sctx.Run.HeadSHA in lockstep. If HEAD has diverged from that recorded head -
// e.g. a concurrent process reset the shared worktree to a different commit -
// then the reviewed change the pipeline approved is no longer in HEAD's history,
// and committing on top of it would ship an unreviewed tree. The whole job of
// this tool is to not lose people's code, so we refuse rather than proceed.
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
// would be circular and defeatable. Because the guard sits at the very top of
// commitAgentFixes - before any commit that would advance sctx.Run.HeadSHA - the
// first pipeline commit after a clobber is caught while the anchor is still the
// pre-clobber reviewed head; the anchor can never be advanced into a clobbered
// lineage without first passing this check.
//
// This is what happened in run 01KXC3SD5NZYMERGDS68Z1C8ER: the review step
// committed a correct fix, a sibling worktree sharing the bare repo reset HEAD
// to a divergent commit that lacked it, and the document step committed on the
// clobber and shipped it. A forward-only agent commit (git rebase --continue,
// etc.) keeps the recorded head as an ancestor and is allowed; a divergent
// (sibling) reset or a backward reset both trip this guard. On any failure the
// error propagates out of commitAgentFixes, so the step and the whole run abort
// (executor.failRun) before push - nothing is committed or shipped.
func assertPipelineHeadContinuity(sctx *pipeline.StepContext, stepName types.StepName) error {
	recorded := strings.TrimSpace(sctx.Run.HeadSHA)
	if recorded == "" {
		return nil
	}
	currentHead, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return fmt.Errorf("resolve head before %s commit: %w", stepName, err)
	}
	if currentHead == recorded {
		return nil
	}
	// Fail closed: refuse unless the recorded head is genuinely an ancestor of the
	// live HEAD (a legitimate forward move). A non-ancestor result OR any git error
	// (e.g. an unknown recorded object) aborts rather than proceeds.
	if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "merge-base", "--is-ancestor", recorded, currentHead); err != nil {
		return fmt.Errorf("refusing to commit %s changes: worktree HEAD %s is not a descendant of the pipeline's recorded head %s; "+
			"the reviewed change was rewritten out-of-band and would be lost - aborting to protect it",
			stepName, currentHead, recorded)
	}
	return nil
}

func commitAgentFixes(sctx *pipeline.StepContext, stepName types.StepName, summary, fallbackSummary string) error {
	ctx := sctx.Ctx
	if err := assertPipelineHeadContinuity(sctx, stepName); err != nil {
		return err
	}
	status, _ := git.Run(ctx, sctx.WorkDir, "status", "--porcelain")
	if strings.TrimSpace(status) == "" {
		sctx.Log("no agent changes to commit")
		return nil
	}
	if _, err := git.Run(ctx, sctx.WorkDir, "add", "-A"); err != nil {
		return fmt.Errorf("stage %s changes: %w", stepName, err)
	}
	if summary == "" {
		summary = fallbackSummary
	}
	commitMessage := deterministicFixCommitMessage(stepName, summary)
	if _, err := git.Run(ctx, sctx.WorkDir, "commit", "-m", commitMessage); err != nil {
		return fmt.Errorf("commit %s changes: %w", stepName, err)
	}
	headSHA, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return fmt.Errorf("resolve head after %s commit: %w", stepName, err)
	}
	if err := assertPipelineHeadContinuity(sctx, stepName); err != nil {
		return err
	}
	ref := normalizedBranchRef(sctx.Run.Branch)
	if _, err := git.Run(ctx, sctx.WorkDir, "update-ref", ref, headSHA); err != nil {
		return fmt.Errorf("update local branch ref: %w", err)
	}
	sctx.Run.HeadSHA = headSHA
	if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, headSHA); err != nil {
		return err
	}
	sctx.Log(fmt.Sprintf("committed agent fixes: %s", commitMessage))
	return nil
}

func extractCommitSummary(result *agent.Result) (string, error) {
	var summary commitSummary
	if result.Output == nil {
		return "", fmt.Errorf("agent returned no structured summary")
	}
	if err := json.Unmarshal(result.Output, &summary); err != nil {
		return "", fmt.Errorf("parse commit summary: %w", err)
	}
	cleaned := strings.Join(strings.Fields(summary.Summary), " ")
	cleaned = strings.Trim(cleaned, " \t\r\n\"'.;:,-")
	return cleaned, nil
}

func deterministicFixCommitMessage(stepName types.StepName, summary string) string {
	if summary == "" {
		summary = "apply fixes"
	}
	return fmt.Sprintf("no-mistakes(%s): %s", stepName, summary)
}

// executeFixMode runs the fix agent and commits any resulting changes. It
// returns the agent's one-line fix summary (empty when the agent returned
// nothing parseable), which the caller should place on StepOutcome.FixSummary
// so the executor can persist it on the round record.
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
	runOpts := agent.RunOpts{
		Prompt:     opts.Prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: commitSummarySchema,
		OnChunk:    sctx.LogChunk,
		Purpose:    purpose,
		Workload:   opts.Workload,
	}
	var result *agent.Result
	var err error
	if opts.SessionRole != "" {
		result, err = sctx.RunAgentSession(opts.SessionRole, runOpts)
	} else {
		result, err = sctx.Agent.Run(sctx.Ctx, runOpts)
	}
	if err != nil {
		return "", fmt.Errorf("%s: %w", opts.ErrorPrefix, err)
	}
	if opts.AfterAgentRun != nil {
		if err := opts.AfterAgentRun(result); err != nil {
			return "", err
		}
	}
	summary, err := extractCommitSummary(result)
	if err != nil {
		sctx.Log(fmt.Sprintf("warning: could not parse fix summary: %v", err))
	}
	if err := commitAgentFixes(sctx, stepName, summary, opts.FallbackSummary); err != nil {
		return "", err
	}
	return summary, nil
}
