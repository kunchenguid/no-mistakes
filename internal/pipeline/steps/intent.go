package steps

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/user"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/intent"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// intentExtractTimeout caps total wall-clock time spent on intent extraction.
const intentExtractTimeout = 30 * time.Second

// IntentStep is a best-effort pipeline step that infers the user's intent
// from local agent transcripts and attaches it to the run so downstream
// steps can surface it in their prompts. Failures are intentionally
// swallowed and surface as a "skipped" outcome rather than a run failure:
// missing transcripts, slow summarizers, or DB hiccups must not block
// the pipeline.
type IntentStep struct {
	// runIntent computes the intent for a step context. It is overridden
	// in tests; the zero value falls back to defaultRunIntent which wires
	// up readers, summarizer, and the real intent.Extract pipeline.
	runIntent func(ctx context.Context, sctx *pipeline.StepContext) (*intent.Result, error)
}

func (s *IntentStep) Name() types.StepName { return types.StepIntent }

func (s *IntentStep) Execute(sctx *pipeline.StepContext) (outcome *pipeline.StepOutcome, err error) {
	defer func() {
		if r := recover(); r != nil {
			runID := ""
			if sctx != nil && sctx.Run != nil {
				runID = sctx.Run.ID
			}
			slog.Warn("panic during intent extraction", "run_id", runID, "panic", r)
			outcome = &pipeline.StepOutcome{Skipped: true}
			err = nil
		}
	}()

	if sctx == nil || sctx.Config == nil || !sctx.Config.Intent.Enabled {
		if sctx != nil && sctx.Log != nil {
			sctx.Log("intent extraction disabled in config")
		}
		return &pipeline.StepOutcome{Skipped: true}, nil
	}
	if sctx.DB == nil || sctx.Run == nil || sctx.Repo == nil {
		return &pipeline.StepOutcome{Skipped: true}, nil
	}

	sctx.Log("scanning recent agent transcripts...")

	ctx, cancel := context.WithTimeout(sctx.Ctx, intentExtractTimeout)
	defer cancel()

	startedAt := time.Now()
	outcomeLabel := "no_match"
	matchedAgent := ""
	score := 0.0

	defer func() {
		fields := telemetry.Fields{
			"action":      "intent",
			"outcome":     outcomeLabel,
			"duration_ms": time.Since(startedAt).Milliseconds(),
		}
		if matchedAgent != "" {
			fields["matched_agent"] = matchedAgent
		}
		if score > 0 {
			fields["score"] = score
		}
		telemetry.Track("run", fields)
	}()

	runFn := s.runIntent
	if runFn == nil {
		runFn = defaultRunIntent
	}

	result, runErr := runFn(ctx, sctx)
	if runErr != nil {
		if errors.Is(runErr, intent.ErrNoMatch) {
			outcomeLabel = "no_match"
			sctx.Log("no matching agent transcript found")
			return &pipeline.StepOutcome{Skipped: true}, nil
		}
		if errors.Is(runErr, errIntentEmptyDiff) {
			outcomeLabel = "empty_diff"
			sctx.Log("no diff between base and head, skipping intent extraction")
			return &pipeline.StepOutcome{Skipped: true}, nil
		}
		slog.Debug("intent: extract failed", "run_id", sctx.Run.ID, "error", runErr)
		outcomeLabel = "error"
		sctx.Log(fmt.Sprintf("intent extraction failed: %v", runErr))
		return &pipeline.StepOutcome{Skipped: true}, nil
	}
	if result == nil {
		sctx.Log("no intent attached")
		return &pipeline.StepOutcome{Skipped: true}, nil
	}

	matchedAgent = result.AgentName
	score = result.Score
	outcomeLabel = "matched"

	if dbErr := sctx.DB.UpdateRunIntent(sctx.Run.ID, db.RunIntent{
		Summary:   result.Summary,
		Source:    result.AgentName,
		SessionID: result.SessionID,
		Score:     result.Score,
	}); dbErr != nil {
		slog.Warn("intent: persist failed", "run_id", sctx.Run.ID, "error", dbErr)
		sctx.Log(fmt.Sprintf("intent matched but failed to persist: %v", dbErr))
		return &pipeline.StepOutcome{Skipped: true}, nil
	}

	summaryCopy := result.Summary
	sctx.Run.Intent = &summaryCopy
	sourceCopy := result.AgentName
	sctx.Run.IntentSource = &sourceCopy
	sessionCopy := result.SessionID
	sctx.Run.IntentSessionID = &sessionCopy
	scoreCopy := result.Score
	sctx.Run.IntentScore = &scoreCopy

	sctx.Log(fmt.Sprintf("matched %s session (score %.2f)", result.AgentName, result.Score))
	sctx.Log("inferred intent:")
	sctx.Log(intent.RedactSecrets(intent.StripAdversarial(sanitizePromptMultilineText(result.Summary))))

	slog.Info("intent: attached", "run_id", sctx.Run.ID, "agent", matchedAgent, "score", score)
	return &pipeline.StepOutcome{}, nil
}

// errIntentEmptyDiff is returned by defaultRunIntent when the diff between
// base and head produces no files. It is reported as the "empty_diff"
// telemetry outcome.
var errIntentEmptyDiff = errors.New("intent: empty diff")

// defaultRunIntent is the production implementation: it derives intent
// extraction inputs from the run's git state and calls intent.Extract.
func defaultRunIntent(ctx context.Context, sctx *pipeline.StepContext) (*intent.Result, error) {
	if sctx.Agent == nil {
		return nil, intent.ErrNoMatch
	}

	repo := sctx.Repo
	run := sctx.Run
	cfg := sctx.Config
	gitWorkDir := strings.TrimSpace(sctx.WorkDir)
	if gitWorkDir == "" {
		gitWorkDir = repo.WorkingPath
	}

	resolvedBaseSHA := resolveIntentBaseSHA(ctx, gitWorkDir, run.BaseSHA, repo.DefaultBranch)
	diffFiles, err := git.DiffNameOnly(ctx, gitWorkDir, resolvedBaseSHA, run.HeadSHA)
	if err != nil {
		return nil, err
	}
	if len(diffFiles) == 0 {
		return nil, errIntentEmptyDiff
	}

	baseTime, err := git.CommitTime(ctx, gitWorkDir, resolvedBaseSHA)
	if err != nil || git.IsZeroSHA(run.BaseSHA) {
		baseTime = time.Now().Add(-7 * 24 * time.Hour)
	}
	headTime, err := git.CommitTime(ctx, gitWorkDir, run.HeadSHA)
	if err != nil {
		headTime = time.Now()
	}

	if authorEmail, err := git.CommitAuthorEmail(ctx, gitWorkDir, run.HeadSHA); err == nil && authorEmail != "" {
		if u, uerr := user.Current(); uerr == nil && u != nil {
			localUser := strings.ToLower(u.Username)
			emailUser := strings.ToLower(strings.SplitN(authorEmail, "@", 2)[0])
			if localUser != "" && emailUser != "" && !strings.Contains(emailUser, localUser) && !strings.Contains(localUser, emailUser) {
				slog.Warn("intent: head commit author looks different from local user; intent may not reflect this commit",
					"run_id", run.ID, "commit_email", authorEmail, "local_user", u.Username)
			}
		}
	}

	return intent.Extract(ctx, intent.ExtractParams{
		OriginCWD:  repo.WorkingPath,
		DiffFiles:  diffFiles,
		BaseTime:   baseTime,
		HeadTime:   headTime,
		SlackDays:  cfg.Intent.SlackDays,
		Threshold:  cfg.Intent.Threshold,
		Readers:    intent.AllReaders(cfg.Intent.DisabledReaders),
		Cache:      intent.NewDBCache(sctx.DB),
		Summarizer: intent.NewAgentSummarizer(sctx.Agent, sctx.WorkDir),
		Logf: func(format string, args ...any) {
			sctx.Log(fmt.Sprintf("intent "+format, args...))
		},
	})
}

// resolveIntentBaseSHA returns a usable base SHA for diff'ing against head.
// Prefers an explicit run.BaseSHA when reachable in the worktree, but falls
// back to merge-base against the default branch when the SHA is the zero ref
// (new branch push) or has been orphaned by a force push that rewrote the
// prior remote tip away. Final fallback is git's empty-tree SHA so the diff
// always succeeds.
func resolveIntentBaseSHA(ctx context.Context, workDir, baseSHA, defaultBranch string) string {
	if !git.IsZeroSHA(baseSHA) && commitReachable(ctx, workDir, baseSHA) {
		return baseSHA
	}
	if mb := mergeBaseWithDefaultBranch(ctx, workDir, defaultBranch); mb != "" {
		return mb
	}
	return git.EmptyTreeSHA
}

func commitReachable(ctx context.Context, workDir, sha string) bool {
	if strings.TrimSpace(sha) == "" {
		return false
	}
	_, err := git.Run(ctx, workDir, "cat-file", "-e", sha+"^{commit}")
	return err == nil
}
