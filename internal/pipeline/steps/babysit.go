package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// BabysitStep monitors CI and PR comments after PR creation,
// auto-fixing CI failures and presenting PR comments for human selection.
type BabysitStep struct {
	seenComments map[string]bool
}

func (s *BabysitStep) Name() types.StepName { return types.StepBabysit }

// ciCheck represents a CI check result from gh pr checks --json.
type ciCheck struct {
	Name       string `json:"name"`
	Status     string `json:"status"`     // legacy fake-test field
	Conclusion string `json:"conclusion"` // legacy fake-test field
	State      string `json:"state"`      // gh CLI field
	Bucket     string `json:"bucket"`     // gh CLI field: pass|fail|pending|skipping|cancel
}

// prComment represents a comment on a PR from gh pr view --json comments.
type prComment struct {
	ID     string `json:"id"`
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
	URL       string `json:"url"`
}

// extractPRNumber extracts the PR number from a GitHub PR URL.
// Handles URLs like "https://github.com/owner/repo/pull/42".
func extractPRNumber(prURL string) (string, error) {
	trimmed := strings.TrimRight(prURL, "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) == 0 {
		return "", fmt.Errorf("invalid PR URL: %s", prURL)
	}
	num := parts[len(parts)-1]
	if num == "" {
		return "", fmt.Errorf("invalid PR URL: %s", prURL)
	}
	return num, nil
}

// pollInterval returns the polling interval based on elapsed time since babysit started.
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

// hasFailingChecks returns true if any CI check has a failure conclusion.
func hasFailingChecks(checks []ciCheck) bool {
	for _, c := range checks {
		if c.Bucket == "fail" || c.Conclusion == "failure" || c.Conclusion == "action_required" {
			return true
		}
	}
	return false
}

// hasPendingChecks returns true if any CI check is still running or queued.
func hasPendingChecks(checks []ciCheck) bool {
	for _, c := range checks {
		if c.Bucket == "pending" {
			return true
		}
		if c.Conclusion == "" && c.Status != "COMPLETED" {
			return true
		}
	}
	return false
}

// failingCheckNames returns the names of failing checks.
func failingCheckNames(checks []ciCheck) []string {
	var names []string
	for _, c := range checks {
		if c.Bucket == "fail" || c.Conclusion == "failure" || c.Conclusion == "action_required" {
			names = append(names, c.Name)
		}
	}
	return names
}

// commentsToFindings converts PR comments to the findings format for TUI display.
func commentsToFindings(comments []prComment) Findings {
	var items []Finding
	for _, c := range comments {
		items = append(items, Finding{
			ID:          c.ID,
			Severity:    "info",
			Description: fmt.Sprintf("@%s: %s", c.Author.Login, truncate(c.Body, 200)),
		})
	}
	return Findings{
		Items:   items,
		Summary: fmt.Sprintf("%d PR comment(s) to review", len(comments)),
	}
}

// truncate shortens a string to maxLen, adding "..." if truncated.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

func (s *BabysitStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	ctx := sctx.Ctx

	// Initialize seen comments tracking
	if s.seenComments == nil {
		s.seenComments = make(map[string]bool)
	}

	// Get PR URL from run record
	prURL := ""
	if sctx.Run.PRURL != nil {
		prURL = *sctx.Run.PRURL
	}
	if prURL == "" {
		// Try to refresh from DB in case PR step set it
		run, _ := sctx.DB.GetRun(sctx.Run.ID)
		if run != nil && run.PRURL != nil {
			prURL = *run.PRURL
			sctx.Run.PRURL = run.PRURL
		}
	}
	if prURL == "" {
		sctx.Log("no PR URL found, skipping babysit")
		return &pipeline.StepOutcome{}, nil
	}

	prNumber, err := extractPRNumber(prURL)
	if err != nil {
		return nil, fmt.Errorf("extract PR number: %w", err)
	}

	// If in fix mode, address comments then resume polling
	if sctx.Fixing {
		if err := s.addressComments(sctx, prNumber); err != nil {
			sctx.Log(fmt.Sprintf("warning: could not address comments: %v", err))
		}
	}

	timeout := sctx.Config.BabysitTimeout
	if timeout == 0 {
		timeout = 4 * time.Hour
	}

	sctx.Log(fmt.Sprintf("babysitting PR #%s (timeout: %s)...", prNumber, timeout))
	started := time.Now()

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		elapsed := time.Since(started)
		if elapsed >= timeout {
			sctx.Log("babysit timeout reached")
			return &pipeline.StepOutcome{}, nil
		}

		// Check PR state (merged/closed → exit)
		state, err := s.getPRState(ctx, sctx.WorkDir, prNumber)
		if err != nil {
			sctx.Log(fmt.Sprintf("warning: could not check PR state: %v", err))
		} else if state == "MERGED" {
			sctx.Log("PR has been merged!")
			return &pipeline.StepOutcome{}, nil
		} else if state == "CLOSED" {
			sctx.Log("PR has been closed")
			return &pipeline.StepOutcome{}, nil
		}

		// Check CI status — auto-fix failures (no approval needed)
		checks, err := s.getCIChecks(ctx, sctx.WorkDir, prNumber)
		if err != nil {
			sctx.Log(fmt.Sprintf("warning: could not check CI: %v", err))
		} else if hasFailingChecks(checks) {
			failing := failingCheckNames(checks)
			sctx.Log(fmt.Sprintf("CI failures detected: %s — auto-fixing...", strings.Join(failing, ", ")))
			if err := s.autoFixCI(sctx, prNumber, failing); err != nil {
				sctx.Log(fmt.Sprintf("warning: CI auto-fix failed: %v", err))
			}
		}

		// Check for new PR comments — pause for human selection
		comments, err := s.getNewComments(ctx, sctx.WorkDir, prNumber)
		if err != nil {
			sctx.Log(fmt.Sprintf("warning: could not check comments: %v", err))
		} else if len(comments) > 0 {
			sctx.Log(fmt.Sprintf("found %d new PR comment(s)", len(comments)))
			// Mark all as seen
			for _, c := range comments {
				s.seenComments[c.ID] = true
			}
			// Return with findings for TUI display
			findings := commentsToFindings(comments)
			findingsJSON, _ := json.Marshal(findings)
			return &pipeline.StepOutcome{
				NeedsApproval: true,
				Findings:      string(findingsJSON),
			}, nil
		}

		// Sleep for poll interval
		interval := pollInterval(time.Since(started))
		remaining := timeout - time.Since(started)
		if remaining < interval {
			interval = remaining
		}
		select {
		case <-time.After(interval):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// getPRState returns the PR state (OPEN, MERGED, CLOSED).
func (s *BabysitStep) getPRState(ctx context.Context, workDir, prNumber string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "view", prNumber, "--json", "state", "--jq", ".state")
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// getCIChecks fetches CI check results for a PR.
func (s *BabysitStep) getCIChecks(ctx context.Context, workDir, prNumber string) ([]ciCheck, error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "checks", prNumber, "--json", "name,state,bucket")
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "no checks reported") {
			return nil, nil
		}
		return nil, fmt.Errorf("gh pr checks: %w", err)
	}
	var checks []ciCheck
	if err := json.Unmarshal(out, &checks); err != nil {
		return nil, fmt.Errorf("parse CI checks: %w", err)
	}
	return checks, nil
}

// getNewComments fetches PR comments and returns only unseen ones.
func (s *BabysitStep) getNewComments(ctx context.Context, workDir, prNumber string) ([]prComment, error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "view", prNumber, "--json", "comments", "--jq", ".comments")
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh pr comments: %w", err)
	}

	var comments []prComment
	if err := json.Unmarshal(out, &comments); err != nil {
		return nil, fmt.Errorf("parse PR comments: %w", err)
	}

	// Filter to only new (unseen) comments
	var newComments []prComment
	for _, c := range comments {
		if !s.seenComments[c.ID] {
			newComments = append(newComments, c)
		}
	}
	return newComments, nil
}

// autoFixCI runs the agent to fix CI failures, then commits and pushes.
func (s *BabysitStep) autoFixCI(sctx *pipeline.StepContext, prNumber string, failingNames []string) error {
	ctx := sctx.Ctx
	baseSHA := resolveBranchBaseSHA(ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)

	// Attempt to fetch CI failure logs for context
	var logOutput string
	for _, name := range failingNames {
		cmd := exec.CommandContext(ctx, "gh", "run", "view", "--json", "jobs",
			"--jq", fmt.Sprintf(`.jobs[] | select(.name == "%s") | .steps[] | select(.conclusion == "failure") | .name + ": " + (.log // "")`, name))
		cmd.Dir = sctx.WorkDir
		out, _ := cmd.Output()
		if len(out) > 0 {
			logOutput += fmt.Sprintf("=== %s ===\n%s\n\n", name, strings.TrimSpace(string(out)))
		}
	}

	prompt := fmt.Sprintf(
		`The following CI checks have failed on this PR. Diagnose and fix the issues.

Context:
- branch: %s
- base commit: %s
- target commit: %s
- PR number: %s
- failing checks: %s

		Rules:
		- Make the minimal change needed.
		- Do not refactor beyond what is needed.
		- Verify the fix by running the most relevant commands locally before finishing.`,
		sctx.Run.Branch,
		baseSHA,
		sctx.Run.HeadSHA,
		prNumber,
		strings.Join(failingNames, ", "),
	)
	if logOutput != "" {
		prompt += fmt.Sprintf(`

CI logs:
%s`, logOutput)
	}

	sctx.Log("running agent to fix CI failures...")
	_, err := sctx.Agent.Run(ctx, agent.RunOpts{
		Prompt:  prompt,
		CWD:     sctx.WorkDir,
		OnChunk: sctx.Log,
	})
	if err != nil {
		return fmt.Errorf("agent CI fix: %w", err)
	}

	return s.commitAndPush(sctx)
}

// addressComments runs the agent to address PR comments, then commits, pushes, and replies.
func (s *BabysitStep) addressComments(sctx *pipeline.StepContext, prNumber string) error {
	ctx := sctx.Ctx
	baseSHA := resolveBranchBaseSHA(ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)
	selected := selectedFindingIDs(sctx.PreviousFindings)

	// Build prompt from the selected comments that triggered the approval pause.
	cmd := exec.CommandContext(ctx, "gh", "pr", "view", prNumber, "--json", "comments", "--jq", ".comments")
	cmd.Dir = sctx.WorkDir
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("fetch comments: %w", err)
	}

	var comments []prComment
	json.Unmarshal(out, &comments)

	var commentText string
	for _, c := range comments {
		if !s.seenComments[c.ID] {
			continue
		}
		if len(selected) > 0 && !selected[c.ID] {
			continue
		}
		commentText += fmt.Sprintf("@%s:\n%s\n\n", c.Author.Login, c.Body)
	}

	if commentText == "" {
		sctx.Log("no comments to address")
		return nil
	}

	prompt := fmt.Sprintf(
		`Address the following PR review comments by making the requested changes.

Context:
- branch: %s
- base commit: %s
- target commit: %s
- PR number: %s

		Rules:
		- Make the minimal change needed.
		- Do not refactor beyond what is needed.
		- Do not add comments explaining your fixes.
		- Verify any directly relevant commands before finishing.

		Comments to address:
		%s`,
		sctx.Run.Branch,
		baseSHA,
		sctx.Run.HeadSHA,
		prNumber,
		commentText,
	)

	sctx.Log("running agent to address PR comments...")
	_, err = sctx.Agent.Run(ctx, agent.RunOpts{
		Prompt:  prompt,
		CWD:     sctx.WorkDir,
		OnChunk: sctx.Log,
	})
	if err != nil {
		return fmt.Errorf("agent address comments: %w", err)
	}

	if err := s.commitAndPush(sctx); err != nil {
		return err
	}

	// Reply to addressed comments (best-effort)
	headSHA, _ := git.HeadSHA(ctx, sctx.WorkDir)
	if headSHA != "" {
		replyCmd := exec.CommandContext(ctx, "gh", "pr", "comment", prNumber,
			"--body", fmt.Sprintf("Addressed in %s", headSHA))
		replyCmd.Dir = sctx.WorkDir
		replyCmd.Run() // best-effort, don't fail on reply error
	}

	return nil
}

func selectedFindingIDs(raw string) map[string]bool {
	if raw == "" {
		return nil
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return nil
	}
	selected := make(map[string]bool, len(findings.Items))
	for _, item := range findings.Items {
		if item.ID != "" {
			selected[item.ID] = true
		}
	}
	return selected
}

// commitAndPush commits any uncommitted changes and force-pushes to upstream.
func (s *BabysitStep) commitAndPush(sctx *pipeline.StepContext) error {
	ctx := sctx.Ctx

	status, _ := git.Run(ctx, sctx.WorkDir, "status", "--porcelain")
	if strings.TrimSpace(status) == "" {
		sctx.Log("no changes to commit")
		return nil
	}

	git.Run(ctx, sctx.WorkDir, "add", "-A")
	if _, err := git.Run(ctx, sctx.WorkDir, "commit", "-m", "no-mistakes: apply babysit fixes"); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	ref := sctx.Run.Branch
	if !strings.HasPrefix(ref, "refs/") {
		ref = "refs/heads/" + ref
	}

	if err := git.Push(ctx, sctx.WorkDir, sctx.Repo.UpstreamURL, ref, "", false); err != nil {
		return fmt.Errorf("push: %w", err)
	}

	sctx.Log("committed and pushed fixes")
	return nil
}
