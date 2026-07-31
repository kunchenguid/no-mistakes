package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
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

var errRejectedCommitSummary = errors.New("rejected commit summary")

var commitSummarySchema = json.RawMessage(fmt.Sprintf(`{
	"type": "object",
	"properties": {
		"summary": {"type": "string", "maxLength": %d}
	},
	"required": ["summary"]
}`, config.MaxFixMessageSummaryBytes))

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
// equal to or a descendant of the head the pipeline itself last recorded
// (sctx.Run.HeadSHA). Every post-review step calls this guard at entry, and
// commitAgentFixes calls it around commits that advance the recorded head.
//
// The pipeline advances HEAD only through its own commits, each of which updates
// sctx.Run.HeadSHA in lockstep. If HEAD has diverged from that recorded head -
// e.g. a concurrent process reset the shared worktree to a different commit -
// then the reviewed change the pipeline approved is no longer in HEAD's history,
// and continuing would ship an unreviewed tree. The whole job of this tool is
// to not lose people's code, so we refuse rather than proceed.
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
// would be circular and defeatable. Because the guard runs at every post-review
// step entry and at the very top of commitAgentFixes - before any commit that
// would advance sctx.Run.HeadSHA - the next pipeline boundary after a clobber is
// caught while the anchor is still the pre-clobber reviewed head; the anchor can
// never be advanced into a clobbered lineage without first passing this check.
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

func commitAgentFixes(sctx *pipeline.StepContext, stepName types.StepName, summary, fallbackSummary string) error {
	return commitAgentFixesWithHooks(sctx, stepName, summary, fallbackSummary, headAdoptionHooks{})
}

// headAdoptionHooks expose only durable crash/race boundaries to focused
// tests. Production passes zero hooks. A hook error simulates process loss at
// that boundary; retry must resume monotonically from the anchor/ref/journal.
type headAdoptionHooks struct {
	AfterAnchor   func() error
	BeforeGateCAS func() error
	AfterGateCAS  func() error
	AfterDBCAS    func() error
}

func commitAgentFixesWithHooks(sctx *pipeline.StepContext, stepName types.StepName, summary, fallbackSummary string, hooks headAdoptionHooks) error {
	if sctx.BranchLock != nil {
		unlock := sctx.BranchLock()
		defer unlock()
	}
	ctx := sctx.Ctx
	oldHead := strings.TrimSpace(sctx.Run.HeadSHA)
	if oldHead == "" {
		return fmt.Errorf("adopt %s fixes: pipeline run has no recorded head", stepName)
	}
	if err := assertPipelineHeadContinuity(sctx, stepName); err != nil {
		return err
	}
	status, err := git.Run(ctx, sctx.WorkDir, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("read worktree status before %s adoption: %w", stepName, err)
	}
	commitMessage := ""
	commitParent := ""
	expectedCommitTree := ""
	if strings.TrimSpace(status) != "" {
		if summary == "" {
			summary = fallbackSummary
		}
		if summary == "" {
			summary = "apply fixes"
		}
		commitMessage, err = sctx.Config.Commit.RenderFixMessage(stepName, summary)
		if err != nil {
			return fmt.Errorf("render %s fix commit message: %w", stepName, err)
		}
		if _, err := git.Run(ctx, sctx.WorkDir, "add", "-A"); err != nil {
			return fmt.Errorf("stage %s changes: %w", stepName, err)
		}
		commitParent, err = git.HeadSHA(ctx, sctx.WorkDir)
		if err != nil {
			return fmt.Errorf("resolve head before %s fix commit: %w", stepName, err)
		}
		expectedCommitTree, err = git.Run(ctx, sctx.WorkDir, "write-tree")
		if err != nil {
			return fmt.Errorf("resolve staged tree before %s fix commit: %w", stepName, err)
		}
		if _, err := git.Run(ctx, sctx.WorkDir, "commit", "-m", commitMessage); err != nil {
			return fmt.Errorf("commit %s changes: %w", stepName, err)
		}
	}
	candidate, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return fmt.Errorf("resolve head after %s fixes: %w", stepName, err)
	}
	finalStatus, err := git.Run(ctx, sctx.WorkDir, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("read final worktree status after %s fixes: %w", stepName, err)
	}
	if strings.TrimSpace(finalStatus) != "" {
		return fmt.Errorf("refusing to adopt %s head %s: worktree is not clean after commit", stepName, candidate)
	}
	if commitMessage != "" {
		candidateTree, treeErr := git.Run(ctx, sctx.WorkDir, "rev-parse", "--verify", candidate+"^{tree}")
		if treeErr != nil {
			return fmt.Errorf("resolve %s candidate tree: %w", stepName, treeErr)
		}
		if candidate == commitParent || candidateTree != expectedCommitTree {
			return fmt.Errorf("refusing to adopt %s head %s: it is not a descendant commit preserving the exact staged tree on top of head %s", stepName, candidate, commitParent)
		}
	}
	if candidate == oldHead {
		sctx.Log("no agent changes to commit")
		return nil
	}
	if _, err := git.Run(ctx, sctx.WorkDir, "merge-base", "--is-ancestor", oldHead, candidate); err != nil {
		return fmt.Errorf("refusing to adopt %s head %s: it is not a descendant of recorded head %s (adoption requires a strict forward move)", stepName, candidate, oldHead)
	}

	anchorRef := liveHeadCandidateAnchorRef(sctx.Run.ID, candidate)
	if _, err := git.Run(ctx, sctx.WorkDir, "check-ref-format", anchorRef); err != nil {
		return fmt.Errorf("validate %s candidate anchor: %w", stepName, err)
	}
	if err := createOrVerifyImmutableRef(ctx, sctx.WorkDir, anchorRef, candidate); err != nil {
		return fmt.Errorf("anchor %s candidate %s: %w", stepName, candidate, err)
	}
	if hooks.AfterAnchor != nil {
		if err := hooks.AfterAnchor(); err != nil {
			return fmt.Errorf("after %s candidate anchor: %w", stepName, err)
		}
	}
	advance := db.ActiveRunHeadAdvance{
		RunID: sctx.Run.ID, RepoID: sctx.Run.RepoID, Branch: sctx.Run.Branch,
		StepName: string(stepName), ExpectedHead: oldHead, Candidate: candidate, AnchorRef: anchorRef,
	}
	// Git refs and SQLite cannot move atomically. Persist the complete exact
	// transition tuple before the gate CAS so a new daemon can distinguish the
	// one prepared old→candidate move it may reconcile from an arbitrary
	// descendant or an unanchored gate change.
	if err := sctx.DB.PrepareActiveRunHeadAdvanceCAS(advance); err != nil {
		return err
	}

	ref := normalizedBranchRef(sctx.Run.Branch)
	gateHead, err := git.Run(ctx, sctx.WorkDir, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return fmt.Errorf("resolve gate branch %s before %s adoption: %w", ref, stepName, err)
	}
	switch gateHead {
	case oldHead:
		if hooks.BeforeGateCAS != nil {
			if err := hooks.BeforeGateCAS(); err != nil {
				return fmt.Errorf("before %s gate CAS: %w", stepName, err)
			}
		}
		if err := verifyExactCleanHead(ctx, sctx.WorkDir, candidate); err != nil {
			return fmt.Errorf("refusing %s gate CAS: %w", stepName, err)
		}
		if _, err := git.Run(ctx, sctx.WorkDir, "update-ref", ref, candidate, oldHead); err != nil {
			return fmt.Errorf("advance local branch ref with expected-old CAS: %w", err)
		}
	case candidate:
		// Monotonic retry after a crash between the Git and SQLite CAS.
	default:
		return fmt.Errorf("refusing to adopt %s head %s: gate branch %s is at %s, expected %s", stepName, candidate, ref, gateHead, oldHead)
	}
	if hooks.AfterGateCAS != nil {
		if err := hooks.AfterGateCAS(); err != nil {
			return fmt.Errorf("after %s gate CAS: %w", stepName, err)
		}
	}
	if err := verifyExactCleanHead(ctx, sctx.WorkDir, candidate); err != nil {
		return fmt.Errorf("refusing %s database CAS: %w", stepName, err)
	}
	if exactGate, err := git.Run(ctx, sctx.WorkDir, "rev-parse", "--verify", ref+"^{commit}"); err != nil || exactGate != candidate {
		return fmt.Errorf("refusing %s database CAS: gate branch no longer equals candidate %s", stepName, candidate)
	}
	if err := sctx.DB.AdvanceActiveRunHeadCAS(advance); err != nil {
		return err
	}
	if hooks.AfterDBCAS != nil {
		if err := hooks.AfterDBCAS(); err != nil {
			return fmt.Errorf("after %s database CAS: %w", stepName, err)
		}
	}
	if err := verifyExactCleanHead(ctx, sctx.WorkDir, candidate); err != nil {
		return fmt.Errorf("%s head was durably adopted but final worktree verification failed: %w", stepName, err)
	}
	if exactGate, err := git.Run(ctx, sctx.WorkDir, "rev-parse", "--verify", ref+"^{commit}"); err != nil || exactGate != candidate {
		return fmt.Errorf("%s head was durably adopted but final gate verification failed", stepName)
	}
	// In-memory authority advances only after the durable journal/head CAS and
	// final Git equality checks have succeeded.
	sctx.Run.HeadSHA = candidate
	if commitMessage != "" {
		sctx.Log(fmt.Sprintf("committed agent fixes: %s", commitMessage))
	} else {
		sctx.Log(fmt.Sprintf("adopted agent self-commit: %s", candidate))
	}
	return nil
}

func liveHeadCandidateAnchorRef(runID, candidate string) string {
	return "refs/no-mistakes/run-head-candidates/" + runID + "/" + candidate
}

func createOrVerifyImmutableRef(ctx context.Context, dir, ref, candidate string) error {
	if existing, err := git.Run(ctx, dir, "rev-parse", "--verify", ref+"^{commit}"); err == nil {
		if existing != candidate {
			return fmt.Errorf("immutable ref %s already names conflicting commit %s", ref, existing)
		}
		return nil
	}
	zero := strings.Repeat("0", len(candidate))
	if _, err := git.Run(ctx, dir, "update-ref", ref, candidate, zero); err != nil {
		if existing, readErr := git.Run(ctx, dir, "rev-parse", "--verify", ref+"^{commit}"); readErr == nil && existing == candidate {
			return nil
		}
		return err
	}
	existing, err := git.Run(ctx, dir, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil || existing != candidate {
		return fmt.Errorf("immutable ref %s did not retain candidate %s", ref, candidate)
	}
	return nil
}

func verifyExactCleanHead(ctx context.Context, dir, candidate string) error {
	head, err := git.HeadSHA(ctx, dir)
	if err != nil {
		return fmt.Errorf("resolve HEAD: %w", err)
	}
	if head != candidate {
		return fmt.Errorf("HEAD changed to %s, expected exact candidate %s", head, candidate)
	}
	status, err := git.Run(ctx, dir, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("read worktree status: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		return fmt.Errorf("worktree changed after candidate selection")
	}
	return nil
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
	cleaned := strings.Join(strings.Fields(summary.Summary), " ")
	cleaned = strings.Trim(cleaned, " \t\r\n\"'.;:,-")
	return cleaned, nil
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
		if errors.Is(err, errRejectedCommitSummary) {
			return "", fmt.Errorf("validate %s fix summary: %w", stepName, err)
		}
		sctx.Log(fmt.Sprintf("warning: could not parse fix summary: %v", err))
	}
	if err := commitAgentFixes(sctx, stepName, summary, opts.FallbackSummary); err != nil {
		return "", err
	}
	return summary, nil
}
