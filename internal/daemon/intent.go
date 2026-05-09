package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os/user"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/intent"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
)

// intentExtractTimeout caps total wall-clock time spent on intent extraction.
// Discover + load + summarize must all fit inside this budget; on timeout we
// silently skip and the run continues without intent attached.
const intentExtractTimeout = 30 * time.Second

// extractIntent runs the discover -> match -> summarize pipeline against the
// user's local agent transcripts and persists the result onto the run.
//
// Failures are deliberately swallowed: a missing transcript, a buggy reader,
// a slow summarizer, or a DB hiccup must NOT fail the pipeline. Telemetry
// records the outcome so we can spot widespread regressions.
func (m *RunManager) extractIntent(
	ctx context.Context,
	cfg *config.Config,
	ag agent.Agent,
	repo *db.Repo,
	run *db.Run,
	baseSHA, headSHA string,
) {
	if !cfg.Intent.Enabled {
		return
	}
	if ag == nil {
		return
	}

	extractCtx, cancel := context.WithTimeout(ctx, intentExtractTimeout)
	defer cancel()

	defer func() {
		if r := recover(); r != nil {
			slog.Warn("panic during intent extraction", "run_id", run.ID, "panic", r)
		}
	}()

	startedAt := time.Now()
	outcome := "no_match"
	var matchedAgent string
	var score float64

	defer func() {
		fields := telemetry.Fields{
			"action":      "intent",
			"outcome":     outcome,
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

	resolvedBaseSHA := resolveIntentBaseSHA(extractCtx, repo.WorkingPath, baseSHA, repo.DefaultBranch)
	diffFiles, err := git.DiffNameOnly(extractCtx, repo.WorkingPath, resolvedBaseSHA, headSHA)
	if err != nil {
		slog.Debug("intent: diff failed", "run_id", run.ID, "error", err)
		outcome = "error"
		return
	}
	if len(diffFiles) == 0 {
		outcome = "empty_diff"
		return
	}

	// Use the original base SHA for time bounds when it's a real commit,
	// otherwise approximate with a generous lookback so first-push branches
	// can still match transcripts written in the past few days.
	baseTime, err := git.CommitTime(extractCtx, repo.WorkingPath, resolvedBaseSHA)
	if err != nil || git.IsZeroSHA(baseSHA) {
		baseTime = time.Now().Add(-7 * 24 * time.Hour)
	}
	headTime, err := git.CommitTime(extractCtx, repo.WorkingPath, headSHA)
	if err != nil {
		headTime = time.Now()
	}

	authorEmail, err := git.CommitAuthorEmail(extractCtx, repo.WorkingPath, headSHA)
	if err == nil && authorEmail != "" {
		if u, uerr := user.Current(); uerr == nil && u != nil {
			localUser := strings.ToLower(u.Username)
			emailUser := strings.ToLower(strings.SplitN(authorEmail, "@", 2)[0])
			if localUser != "" && emailUser != "" && !strings.Contains(emailUser, localUser) && !strings.Contains(localUser, emailUser) {
				slog.Warn("intent: head commit author looks different from local user; intent may not reflect this commit",
					"run_id", run.ID, "commit_email", authorEmail, "local_user", u.Username)
			}
		}
	}

	result, err := intent.Extract(extractCtx, intent.ExtractParams{
		OriginCWD:  repo.WorkingPath,
		DiffFiles:  diffFiles,
		BaseTime:   baseTime,
		HeadTime:   headTime,
		SlackDays:  cfg.Intent.SlackDays,
		Threshold:  cfg.Intent.Threshold,
		Readers:    intent.AllReaders(cfg.Intent.DisabledReaders),
		Cache:      intent.NewDBCache(m.db),
		Summarizer: intent.NewAgentSummarizer(ag),
	})
	if err != nil {
		if errors.Is(err, intent.ErrNoMatch) {
			return
		}
		slog.Debug("intent: extract failed", "run_id", run.ID, "error", err)
		outcome = "error"
		return
	}

	matchedAgent = result.AgentName
	score = result.Score
	outcome = "matched"

	if dbErr := m.db.UpdateRunIntent(run.ID, db.RunIntent{
		Summary:   result.Summary,
		Source:    result.AgentName,
		SessionID: result.SessionID,
		Score:     result.Score,
	}); dbErr != nil {
		slog.Warn("intent: persist failed", "run_id", run.ID, "error", dbErr)
		return
	}

	intentCopy := result.Summary
	run.Intent = &intentCopy
	source := result.AgentName
	run.IntentSource = &source
	sessionID := result.SessionID
	run.IntentSessionID = &sessionID
	run.IntentScore = &result.Score

	slog.Info("intent: attached", "run_id", run.ID, "agent", matchedAgent, "score", score)
}

// resolveIntentBaseSHA returns a usable base SHA for diff'ing against the
// head. New-branch pushes arrive with the all-zeros SHA from git's hook,
// in which case we fall back to merge-base against the default branch and
// then to git's empty-tree SHA, so the diff always succeeds.
func resolveIntentBaseSHA(ctx context.Context, workDir, baseSHA, defaultBranch string) string {
	if !git.IsZeroSHA(baseSHA) {
		return baseSHA
	}
	if strings.TrimSpace(defaultBranch) != "" {
		for _, ref := range []string{"origin/" + defaultBranch, defaultBranch} {
			mb, err := git.Run(ctx, workDir, "merge-base", "HEAD", ref)
			if err == nil && strings.TrimSpace(mb) != "" {
				return strings.TrimSpace(mb)
			}
		}
	}
	return git.EmptyTreeSHA
}
