package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const defaultChecksGracePeriod = 60 * time.Second

// CIStep monitors CI checks after PR creation, auto-fixing failures.
type CIStep struct {
	lastFixedChecks      string        // sorted check names from last fix attempt, to avoid re-fixing
	ciFixAttempts        int           // number of CI auto-fix attempts made
	checksGracePeriod    time.Duration // minimum wait before trusting empty CI checks (0 = default 60s)
	pollIntervalOverride time.Duration // if set, overrides computed poll interval (for testing)
	waitForNextPoll      func(context.Context, time.Duration) error
	now                  func() time.Time
}

func (s *CIStep) Name() types.StepName { return types.StepCI }

func (s *CIStep) gracePeriod() time.Duration {
	if s.checksGracePeriod > 0 {
		return s.checksGracePeriod
	}
	return defaultChecksGracePeriod
}

// ciCheck represents a CI check result from gh pr checks --json.
type ciCheck struct {
	Name       string `json:"name"`
	Status     string `json:"status"`     // legacy fake-test field
	Conclusion string `json:"conclusion"` // legacy fake-test field
	State      string `json:"state"`      // gh CLI field
	Bucket     string `json:"bucket"`     // gh CLI field: pass|fail|pending|skipping|cancel
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
	if _, err := strconv.Atoi(num); err != nil {
		return "", fmt.Errorf("invalid PR number %q in URL: %s", num, prURL)
	}
	return num, nil
}

// pollInterval returns the polling interval based on elapsed time since CI monitoring started.
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
		if c.Bucket != "" {
			continue
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

func ciFailureOutcome(failing []string, summary string) *pipeline.StepOutcome {
	findings := Findings{Summary: summary}
	for _, name := range failing {
		findings.Items = append(findings.Items, Finding{
			Severity:    "warning",
			Description: fmt.Sprintf("CI check failing: %s", name),
		})
	}
	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		Findings:      string(findingsJSON),
	}
}

func (s *CIStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	ctx := sctx.Ctx
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	provider := scm.DetectProvider(sctx.Repo.UpstreamURL)
	if provider == scm.ProviderUnknown && sctx.Run.PRURL != nil {
		provider = scm.DetectProvider(*sctx.Run.PRURL)
	}
	if provider != scm.ProviderGitHub {
		sctx.Log(fmt.Sprintf("skipping CI: provider %s is not supported yet", provider))
		return &pipeline.StepOutcome{}, nil
	}
	if !scm.CLIAvailable(provider) {
		sctx.Log("skipping CI: gh CLI is not installed")
		return &pipeline.StepOutcome{}, nil
	}
	if !scm.AuthConfigured(ctx, provider, sctx.WorkDir) {
		sctx.Log("skipping CI: gh CLI is not authenticated")
		return &pipeline.StepOutcome{}, nil
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
		sctx.Log("no PR URL found, skipping CI")
		return &pipeline.StepOutcome{}, nil
	}

	prNumber, err := extractPRNumber(prURL)
	if err != nil {
		return nil, fmt.Errorf("extract PR number: %w", err)
	}

	timeout := sctx.Config.CITimeout
	if timeout == 0 {
		timeout = 4 * time.Hour
	}

	sctx.Log(fmt.Sprintf("monitoring CI for PR #%s (timeout: %s)...", prNumber, timeout))
	now := s.now
	if now == nil {
		now = time.Now
	}
	started := now()
	manualFixAttempted := false

	for {
		checksReadyToExit := false
		checksSummary := ""

		if err := ctx.Err(); err != nil {
			return nil, err
		}

		elapsed := now().Sub(started)
		if elapsed >= timeout {
			sctx.Log("CI timeout reached")
			return &pipeline.StepOutcome{}, nil
		}

		// Check PR state (merged/closed -> exit)
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

		// Check CI status - auto-fix failures when configured
		ciFixLimit := sctx.Config.AutoFix.CI
		checks, err := s.getCIChecks(ctx, sctx.WorkDir, prNumber)
		if err != nil {
			sctx.Log(fmt.Sprintf("warning: could not check CI: %v", err))
		} else if hasFailingChecks(checks) {
			failing := failingCheckNames(checks)
			sort.Strings(failing)
			fixKey := strings.Join(failing, ",")
			if sctx.Fixing && !manualFixAttempted {
				manualFixAttempted = true
				sctx.Log(fmt.Sprintf("CI failures detected: %s - manual fix requested...", strings.Join(failing, ", ")))
				pushed, err := s.autoFixCI(sctx, prNumber, failing)
				if err != nil {
					sctx.Log(fmt.Sprintf("warning: CI manual fix failed: %v", err))
				} else if pushed {
					s.lastFixedChecks = fixKey
				} else {
					sctx.Log("CI fix produced no changes, returning for manual intervention...")
					return ciFailureOutcome(failing, "CI fix produced no changes - failures require manual intervention"), nil
				}
			} else if sctx.Fixing && fixKey == s.lastFixedChecks {
				sctx.Log("fix already attempted for these failures, waiting for CI re-run...")
			} else if ciFixLimit <= 0 {
				sctx.Log(fmt.Sprintf("CI failures detected: %s - auto-fix disabled, waiting for manual intervention...", strings.Join(failing, ", ")))
				return ciFailureOutcome(failing, "CI failures require manual intervention"), nil
			} else if s.ciFixAttempts >= ciFixLimit {
				sctx.Log(fmt.Sprintf("CI failures detected: %s - max auto-fix attempts (%d) reached, waiting for manual intervention...", strings.Join(failing, ", "), ciFixLimit))
				return ciFailureOutcome(failing, "CI failures still present after auto-fix attempts"), nil
			} else if fixKey == s.lastFixedChecks {
				sctx.Log("fix already attempted for these failures, waiting for CI re-run...")
			} else {
				s.ciFixAttempts++
				sctx.Log(fmt.Sprintf("CI failures detected: %s - auto-fixing (attempt %d/%d)...", strings.Join(failing, ", "), s.ciFixAttempts, ciFixLimit))
				pushed, err := s.autoFixCI(sctx, prNumber, failing)
				if err != nil {
					sctx.Log(fmt.Sprintf("warning: CI auto-fix failed: %v", err))
				} else if pushed {
					s.lastFixedChecks = fixKey
				} else {
					// No changes produced - don't set lastFixedChecks so next
					// poll treats this as a new failure and retries if attempts remain.
					sctx.Log("CI fix produced no changes, will retry if attempts remain...")
				}
			}
		} else {
			s.lastFixedChecks = ""
			if !hasPendingChecks(checks) {
				if len(checks) == 0 && elapsed < s.gracePeriod() {
					// CI checks may not be registered yet, keep polling
					sctx.Log("no CI checks reported yet, waiting for checks to register...")
				} else {
					checksReadyToExit = true
					if len(checks) == 0 {
						checksSummary = "no CI checks reported, CI monitoring complete"
					} else {
						checksSummary = "all CI checks passed"
					}
				}
			}
		}

		if checksReadyToExit {
			sctx.Log(checksSummary)
			return &pipeline.StepOutcome{}, nil
		}

		// Sleep for poll interval
		interval := s.pollIntervalOverride
		if interval == 0 {
			interval = pollInterval(now().Sub(started))
		}
		remaining := timeout - now().Sub(started)
		if remaining < interval {
			interval = remaining
		}
		waitForNextPoll := s.waitForNextPoll
		if waitForNextPoll == nil {
			waitForNextPoll = func(ctx context.Context, interval time.Duration) error {
				select {
				case <-time.After(interval):
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
		if err := waitForNextPoll(ctx, interval); err != nil {
			return nil, err
		}
	}
}

// getPRState returns the PR state (OPEN, MERGED, CLOSED).
func (s *CIStep) getPRState(ctx context.Context, workDir, prNumber string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "view", prNumber, "--json", "state", "--jq", ".state")
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// getCIChecks fetches CI check results for a PR.
func (s *CIStep) getCIChecks(ctx context.Context, workDir, prNumber string) ([]ciCheck, error) {
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

// autoFixCI runs the agent to fix CI failures, then commits and pushes.
// Returns (true, nil) when changes were committed and pushed, (false, nil)
// when the agent produced no changes, or (false, err) on failure.
func (s *CIStep) autoFixCI(sctx *pipeline.StepContext, prNumber string, failingNames []string) (bool, error) {
	ctx := sctx.Ctx
	baseSHA := resolveBranchBaseSHA(ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)

	// Find the most recent failing run for this branch so we fetch logs from the right run.
	var runID string
	listCmd := exec.CommandContext(ctx, "gh", "run", "list",
		"--branch", sctx.Run.Branch,
		"--status", "failure",
		"--limit", "1",
		"--json", "databaseId",
		"--jq", ".[0].databaseId")
	listCmd.Dir = sctx.WorkDir
	if listOut, err := listCmd.Output(); err == nil {
		runID = strings.TrimSpace(string(listOut))
	}

	// Attempt to fetch CI failure logs for context
	const maxLogBytes = 32 * 1024
	var logOutput string
	if runID != "" {
		cmd := exec.CommandContext(ctx, "gh", "run", "view", runID, "--log-failed")
		cmd.Dir = sctx.WorkDir
		out, _ := cmd.Output()
		if len(out) > 0 {
			logOutput = strings.TrimSpace(string(out))
			if len(logOutput) > maxLogBytes {
				logOutput = logOutput[len(logOutput)-maxLogBytes:]
				for i := 0; i < len(logOutput) && i < 4; i++ {
					if logOutput[i]&0xC0 != 0x80 {
						logOutput = logOutput[i:]
						break
					}
				}
			}
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
		- You MUST produce file changes that fix the failing checks. Do not conclude that nothing needs to change.
		- If a test fails only on a specific OS (e.g. Windows CRLF, path separators), fix the test to be cross-platform.
		- If a test is flaky, make it deterministic.
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
		return false, fmt.Errorf("agent CI fix: %w", err)
	}

	return s.commitAndPush(sctx)
}

// commitAndPush commits any uncommitted changes and force-pushes to upstream.
// Returns (true, nil) when changes were pushed, (false, nil) when there was
// nothing to commit, or (false, err) on failure.
func (s *CIStep) commitAndPush(sctx *pipeline.StepContext) (bool, error) {
	ctx := sctx.Ctx
	newHeadSHA := ""

	status, err := git.Run(ctx, sctx.WorkDir, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("check CI changes: %w", err)
	}
	if strings.TrimSpace(status) == "" {
		sctx.Log("no changes to commit")
		headSHA, err := git.HeadSHA(ctx, sctx.WorkDir)
		if err == nil && headSHA != sctx.Run.HeadSHA {
			sctx.Run.HeadSHA = headSHA
			if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, headSHA); err != nil {
				return false, err
			}
		}
		return false, nil
	}

	if _, err := git.Run(ctx, sctx.WorkDir, "add", "-A"); err != nil {
		return false, fmt.Errorf("stage CI changes: %w", err)
	}
	if _, err := git.Run(ctx, sctx.WorkDir, "commit", "-m", "no-mistakes: apply CI fixes"); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	headSHA, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return false, fmt.Errorf("resolve head after commit: %w", err)
	}
	newHeadSHA = headSHA

	ref := normalizedBranchRef(sctx.Run.Branch)

	upstreamSHA, lsErr := git.LsRemote(ctx, sctx.WorkDir, sctx.Repo.UpstreamURL, ref)
	if lsErr != nil {
		slog.Warn("ls-remote failed, pushing without force-with-lease", "ref", ref, "error", lsErr)
	}
	if err := git.Push(ctx, sctx.WorkDir, sctx.Repo.UpstreamURL, ref, upstreamSHA, upstreamSHA != ""); err != nil {
		if lsErr != nil {
			return false, fmt.Errorf("push (ls-remote failed: %v): %w", lsErr, err)
		}
		return false, fmt.Errorf("push: %w", err)
	}

	if _, err := git.Run(ctx, sctx.WorkDir, "update-ref", ref, newHeadSHA); err != nil {
		return false, fmt.Errorf("update local branch ref: %w", err)
	}
	sctx.Run.HeadSHA = newHeadSHA
	if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, newHeadSHA); err != nil {
		return false, err
	}

	sctx.Log("committed and pushed fixes")
	return true, nil
}
