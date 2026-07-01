package steps

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// aiReviewGate returns true when it handled the "checks passed" decision
// (either by suppressing the ChecksPassedMsg emission until the AI reviewer
// finishes, or by escalating to a human). Returns false when the pipeline
// should proceed with the normal ChecksPassedMsg emission (no reviewer
// configured, reviewer disabled, or a clean pass).
func (s *CIStep) aiReviewGate(
	sctx *pipeline.StepContext,
	host scm.Host,
	pr *scm.PR,
	now time.Time,
	started time.Time,
	lastMonitorLog *string,
) bool {
	cfg := sctx.Config.AIReview
	if !cfg.Enabled || !host.Capabilities().ReviewComments {
		return false
	}

	reviewerIdentity := cfg.PolicyName
	if reviewerIdentity == "" {
		reviewerIdentity = cfg.Identity
	}

	pass, err := host.GetReviewPass(sctx.Ctx, pr, reviewerIdentity)
	if err != nil {
		if err == scm.ErrUnsupported {
			return false
		}
		sctx.Log(fmt.Sprintf("warning: could not read AI review pass: %v", err))
		return false
	}

	if s.lastPushedSHA == "" {
		s.lastPushedSHA = sctx.Run.HeadSHA
	}

	pushElapsed := now.Sub(started)
	if s.lastPushedSHA != sctx.Run.HeadSHA {
		pushElapsed = now.Sub(started)
	}

	passTimeout := cfg.PassTimeout
	if passTimeout <= 0 {
		passTimeout = 30 * time.Minute
	}

	switch {
	case !pass.Ran:
		// Reviewer not configured or hasn't queued yet.
		if pushElapsed > passTimeout {
			sctx.Log(fmt.Sprintf("AI reviewer did not run within %s, proceeding without it", passTimeout))
			return false
		}
		sctx.Log("waiting for AI reviewer to start...")
		*lastMonitorLog = ""
		return true

	case !pass.Complete || pass.ForSHA != sctx.Run.HeadSHA:
		// A pass is running, or the completed pass is stale (covers an older SHA).
		if pushElapsed > passTimeout {
			sctx.Log(fmt.Sprintf("AI review pass did not complete within %s, proceeding", passTimeout))
			return false
		}
		sctx.Log(fmt.Sprintf("AI review pass in progress for %s...", shortSHA(pass.ForSHA)))
		*lastMonitorLog = ""
		return true

	default:
		// A complete pass covering the current head.
		threads, err := host.GetReviewThreads(sctx.Ctx, pr)
		if err != nil {
			sctx.Log(fmt.Sprintf("warning: could not read AI review threads: %v", err))
			return false
		}
		var unresolved []scm.ReviewThread
		for _, t := range threads {
			if t.IsBot && !t.Resolved {
				unresolved = append(unresolved, t)
			}
		}
		if len(unresolved) == 0 {
			return false
		}
		if s.reviewFixAttempts >= cfg.MaxIterations {
			sctx.Log(fmt.Sprintf("AI review still has %d comments after %d fix rounds, escalating to human",
				len(unresolved), s.reviewFixAttempts))
			return true
		}
		if s.lastReviewedSHA == sctx.Run.HeadSHA {
			sctx.Log("fix already pushed for this head, waiting for next AI review pass...")
			*lastMonitorLog = ""
			return true
		}
		s.reviewFixAttempts++
		sctx.Log(fmt.Sprintf("AI review left %d comment(s), fixing (round %d/%d)...",
			len(unresolved), s.reviewFixAttempts, cfg.MaxIterations))
		pushed, err := s.autoFixReview(sctx, host, pr, unresolved)
		if err != nil {
			sctx.Log(fmt.Sprintf("warning: AI review fix failed: %v", err))
		} else if pushed || sctx.Run.HeadSHA != s.lastPushedSHA {
			s.lastReviewedSHA = sctx.Run.HeadSHA
			s.lastPushedSHA = sctx.Run.HeadSHA
		} else {
			sctx.Log("AI review fix produced no changes, escalating")
			return true
		}
		*lastMonitorLog = ""
		return true
	}
}

// autoFixReview runs the agent to fix AI-reviewer comments and pushes.
// Returns (true, nil) when changes were committed and pushed, (false, nil)
// when the agent produced no changes, or (false, err) on failure.
func (s *CIStep) autoFixReview(
	sctx *pipeline.StepContext,
	host scm.Host,
	pr *scm.PR,
	threads []scm.ReviewThread,
) (bool, error) {
	ctx := sctx.Ctx

	var commentSummaries []string
	for _, t := range threads {
		loc := t.File
		if t.Line > 0 {
			loc = fmt.Sprintf("%s:%d", t.File, t.Line)
		}
		commentSummaries = append(commentSummaries, fmt.Sprintf("- %s: %s", loc, t.Body))
	}

	prompt := fmt.Sprintf(`The CI/CD AI reviewer left the following comments on this PR. Address each comment.

Context:
- branch: %s
- target commit: %s
- PR number: %s

AI reviewer comments:
%s

Rules:
- You MUST produce file changes that address the reviewer's comments. Do not conclude that nothing needs to change.
- Make the smallest correct root-cause fix for each comment.
- Do not refactor beyond what is needed to address the comments.
- Verify the fix by running the most relevant commands locally before finishing.
%s`,
		sctx.Run.Branch,
		sctx.Run.HeadSHA,
		pr.Number,
		strings.Join(commentSummaries, "\n"),
		userIntentPromptSection(sctx),
	)

	sctx.Log("running agent to fix AI review comments...")
	_, err := sctx.Agent.Run(ctx, agent.RunOpts{
		Prompt:  prompt,
		CWD:     sctx.WorkDir,
		OnChunk: sctx.LogChunk,
	})
	if err != nil {
		return false, fmt.Errorf("agent AI review fix: %w", err)
	}

	pushed, err := s.commitAndPush(sctx)
	if err != nil {
		slog.Warn("AI review fix push failed", "err", err)
	}
	return pushed, err
}
