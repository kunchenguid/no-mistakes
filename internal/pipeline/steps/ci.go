package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/bitbucket"
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

func ciFailureOutcome(failing []string, mergeConflict bool, summary string) *pipeline.StepOutcome {
	findings := Findings{Summary: summary}
	for _, name := range failing {
		findings.Items = append(findings.Items, Finding{
			Severity:    "warning",
			Description: fmt.Sprintf("CI check failing: %s", name),
		})
	}
	if mergeConflict {
		findings.Items = append(findings.Items, Finding{
			Severity:    "warning",
			Description: "PR has merge conflicts with the base branch",
		})
	}
	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		Findings:      string(findingsJSON),
	}
}

func ciMergeabilityOutcome(summary, description string) *pipeline.StepOutcome {
	findings := Findings{
		Summary: summary,
		Items: []Finding{{
			Severity:    "warning",
			Description: description,
			Action:      types.ActionAskUser,
		}},
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
	if provider != scm.ProviderGitHub && provider != scm.ProviderBitbucket {
		sctx.Log(fmt.Sprintf("skipping CI: provider %s is not supported yet", provider))
		return &pipeline.StepOutcome{}, nil
	}
	var bitbucketClient *bitbucket.Client
	var bitbucketRepo bitbucket.RepoRef
	if provider == scm.ProviderGitHub {
		if !stepCLIAvailable(sctx, provider) {
			sctx.Log("skipping CI: gh CLI is not installed")
			return &pipeline.StepOutcome{}, nil
		}
		if !stepAuthConfigured(sctx, provider) {
			sctx.Log("skipping CI: gh CLI is not authenticated")
			return &pipeline.StepOutcome{}, nil
		}
	} else {
		var err error
		bitbucketClient, err = bitbucket.NewClientFromEnv(sctx.Env)
		if err != nil {
			sctx.Log(fmt.Sprintf("skipping CI: %v", err))
			return &pipeline.StepOutcome{}, nil
		}
		bitbucketRepo, err = resolveBitbucketRepoRef(sctx.Repo.UpstreamURL, sctx.Run.PRURL)
		if err != nil {
			return nil, err
		}
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
	mergeabilityBlockedReason := ""
	timeoutFailingChecks := []string{}
	timeoutMergeConflict := false

	for {
		checksReadyToExit := false
		checksSummary := ""

		if err := ctx.Err(); err != nil {
			return nil, err
		}

		elapsed := now().Sub(started)
		if elapsed >= timeout {
			sctx.Log("CI timeout reached")
			if len(timeoutFailingChecks) > 0 || timeoutMergeConflict {
				return ciFailureOutcome(timeoutFailingChecks, timeoutMergeConflict, "CI timed out with known failures still present"), nil
			}
			if mergeabilityBlockedReason != "" {
				return ciMergeabilityOutcome("mergeability check timed out", mergeabilityBlockedReason), nil
			}
			return &pipeline.StepOutcome{}, nil
		}

		// Check PR state (merged/closed -> exit)
		state, err := s.getPRState(sctx, provider, bitbucketClient, bitbucketRepo, prNumber)
		if err != nil {
			sctx.Log(fmt.Sprintf("warning: could not check PR state: %v", err))
		} else if state == "MERGED" {
			sctx.Log("PR has been merged!")
			return &pipeline.StepOutcome{}, nil
		} else if state == "CLOSED" || state == "DECLINED" {
			sctx.Log("PR has been closed")
			return &pipeline.StepOutcome{}, nil
		}

		// Check mergeable state
		mergeConflict := false
		mergeabilityKnown := true
		if provider == scm.ProviderGitHub {
			mergeState, mergeErr := s.getMergeableState(sctx, prNumber)
			if mergeErr != nil {
				sctx.Log(fmt.Sprintf("warning: could not check mergeable state: %v", mergeErr))
				mergeabilityBlockedReason = ""
			} else {
				mergeConflict = isMergeConflict(mergeState)
				mergeabilityKnown = isResolvedMergeableState(mergeState)
				if !mergeabilityKnown {
					sctx.Log(fmt.Sprintf("mergeable state still pending: %s", mergeState))
					mergeabilityBlockedReason = fmt.Sprintf("PR mergeability remained unresolved before timeout: %s", mergeState)
				} else {
					mergeabilityBlockedReason = ""
					timeoutMergeConflict = mergeConflict
				}
			}
		}

		// Check CI status - wait for all checks to complete before fixing
		ciFixLimit := sctx.Config.AutoFix.CI
		checks, err := s.getCIChecks(sctx, provider, bitbucketClient, bitbucketRepo, prNumber)
		if err != nil {
			sctx.Log(fmt.Sprintf("warning: could not check CI: %v", err))
		} else {
			pending := hasPendingChecks(checks)
			failing := failingCheckNames(checks)
			sort.Strings(failing)
			hasFailures := len(failing) > 0
			hasIssues := hasFailures || mergeConflict
			timeoutFailingChecks = append(timeoutFailingChecks[:0], failing...)

			if hasIssues && pending {
				// Some checks still running - wait for all to complete before fixing
				sctx.Log("issues detected but checks still pending, waiting for all checks to complete...")
			} else if hasIssues {
				// All checks done, issues present - fix or report
				fixKey := strings.Join(failing, ",")
				if mergeConflict {
					fixKey += "+conflict"
				}
				issueDesc := strings.Join(failing, ", ")
				if mergeConflict {
					if issueDesc != "" {
						issueDesc += " + merge conflict"
					} else {
						issueDesc = "merge conflict"
					}
				}
				if sctx.Fixing && !manualFixAttempted {
					manualFixAttempted = true
					sctx.Log(fmt.Sprintf("issues detected: %s - manual fix requested...", issueDesc))
					previousHeadSHA := sctx.Run.HeadSHA
					pushed, err := s.autoFixCI(sctx, prNumber, failing, mergeConflict)
					if err != nil {
						sctx.Log(fmt.Sprintf("warning: CI manual fix failed: %v", err))
					} else if pushed || sctx.Run.HeadSHA != previousHeadSHA {
						s.lastFixedChecks = fixKey
					} else {
						sctx.Log("CI fix produced no changes, returning for manual intervention...")
						return ciFailureOutcome(failing, mergeConflict, "CI fix produced no changes - failures require manual intervention"), nil
					}
				} else if sctx.Fixing && fixKey == s.lastFixedChecks {
					sctx.Log("fix already attempted for these issues, waiting for CI re-run...")
				} else if ciFixLimit <= 0 {
					sctx.Log(fmt.Sprintf("issues detected: %s - auto-fix disabled, waiting for manual intervention...", issueDesc))
					return ciFailureOutcome(failing, mergeConflict, "CI failures require manual intervention"), nil
				} else if s.ciFixAttempts >= ciFixLimit {
					sctx.Log(fmt.Sprintf("issues detected: %s - max auto-fix attempts (%d) reached, waiting for manual intervention...", issueDesc, ciFixLimit))
					return ciFailureOutcome(failing, mergeConflict, "CI failures still present after auto-fix attempts"), nil
				} else if fixKey == s.lastFixedChecks {
					sctx.Log("fix already attempted for these issues, waiting for CI re-run...")
				} else {
					s.ciFixAttempts++
					sctx.Log(fmt.Sprintf("issues detected: %s - auto-fixing (attempt %d/%d)...", issueDesc, s.ciFixAttempts, ciFixLimit))
					previousHeadSHA := sctx.Run.HeadSHA
					pushed, err := s.autoFixCI(sctx, prNumber, failing, mergeConflict)
					if err != nil {
						sctx.Log(fmt.Sprintf("warning: CI auto-fix failed: %v", err))
					} else if pushed || sctx.Run.HeadSHA != previousHeadSHA {
						s.lastFixedChecks = fixKey
					} else {
						// No changes produced - don't set lastFixedChecks so next
						// poll treats this as a new failure and retries if attempts remain.
						sctx.Log("CI fix produced no changes, will retry if attempts remain...")
					}
				}
			} else {
				s.lastFixedChecks = ""
				if !pending && mergeabilityKnown {
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

func (s *CIStep) getPRState(sctx *pipeline.StepContext, provider scm.Provider, bitbucketClient *bitbucket.Client, bitbucketRepo bitbucket.RepoRef, prNumber string) (string, error) {
	if provider == scm.ProviderBitbucket {
		prID, err := strconv.Atoi(prNumber)
		if err != nil {
			return "", err
		}
		pr, err := bitbucketClient.GetPR(sctx.Ctx, bitbucketRepo, prID)
		if err != nil {
			return "", err
		}
		return pr.State, nil
	}
	cmd := stepCmd(sctx, "gh", "pr", "view", prNumber, "--json", "state", "--jq", ".state")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// getMergeableState returns the PR mergeable state (MERGEABLE, CONFLICTING, UNKNOWN).
func (s *CIStep) getMergeableState(sctx *pipeline.StepContext, prNumber string) (string, error) {
	cmd := stepCmd(sctx, "gh", "pr", "view", prNumber, "--json", "mergeable", "--jq", ".mergeable")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view mergeable: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// isMergeConflict returns true if the mergeable state indicates conflicts.
func isMergeConflict(state string) bool {
	return state == "CONFLICTING"
}

func isResolvedMergeableState(state string) bool {
	return state == "MERGEABLE" || state == "CONFLICTING"
}

// getCIChecks fetches CI check results for a PR.

func (s *CIStep) getCIChecks(sctx *pipeline.StepContext, provider scm.Provider, bitbucketClient *bitbucket.Client, bitbucketRepo bitbucket.RepoRef, prNumber string) ([]ciCheck, error) {
	if provider == scm.ProviderBitbucket {
		prID, err := strconv.Atoi(prNumber)
		if err != nil {
			return nil, err
		}
		statuses, err := bitbucketClient.ListPRStatuses(sctx.Ctx, bitbucketRepo, prID)
		if err != nil {
			return nil, err
		}
		statuses = latestBitbucketStatuses(statuses)
		checks := make([]ciCheck, 0, len(statuses))
		for _, status := range statuses {
			checks = append(checks, ciCheck{
				Name:   status.Name,
				State:  status.State,
				Bucket: bitbucketStatusBucket(status.State),
			})
		}
		return checks, nil
	}
	cmd := stepCmd(sctx, "gh", "pr", "checks", prNumber, "--json", "name,state,bucket")
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

func latestBitbucketStatuses(statuses []bitbucket.CommitStatus) []bitbucket.CommitStatus {
	latest := make([]bitbucket.CommitStatus, 0, len(statuses))
	seen := make(map[string]struct{}, len(statuses))
	for _, status := range statuses {
		id := strings.TrimSpace(status.Key)
		if id == "" {
			id = strings.TrimSpace(status.Name)
		}
		if id == "" {
			latest = append(latest, status)
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		latest = append(latest, status)
	}
	return latest
}

func bitbucketStatusBucket(state string) string {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "SUCCESSFUL", "SUCCESS":
		return "pass"
	case "FAILED", "FAILURE", "ERROR":
		return "fail"
	case "STOPPED":
		return "cancel"
	case "INPROGRESS", "IN_PROGRESS", "PENDING":
		return "pending"
	default:
		return ""
	}
}

func resolveBitbucketRepoRef(upstreamURL string, prURL *string) (bitbucket.RepoRef, error) {
	if repo, err := bitbucket.ParseRepoRef(upstreamURL); err == nil {
		return repo, nil
	}
	if prURL != nil && strings.TrimSpace(*prURL) != "" {
		return bitbucket.ParseRepoRef(*prURL)
	}
	return bitbucket.RepoRef{}, fmt.Errorf("resolve Bitbucket repository from upstream %q", upstreamURL)
}

func trimLogOutput(logOutput string, maxBytes int) string {
	if len(logOutput) <= maxBytes {
		return logOutput
	}
	logOutput = logOutput[len(logOutput)-maxBytes:]
	for i := 0; i < len(logOutput) && i < 4; i++ {
		if logOutput[i]&0xC0 != 0x80 {
			return logOutput[i:]
		}
	}
	return logOutput
}

func (s *CIStep) fetchBitbucketFailedStepLogs(sctx *pipeline.StepContext, client *bitbucket.Client, repo bitbucket.RepoRef, commitSHA string) string {
	if client == nil || strings.TrimSpace(commitSHA) == "" {
		return ""
	}
	pipelines, err := client.ListPipelinesByCommit(sctx.Ctx, repo, commitSHA)
	if err != nil {
		return ""
	}
	for _, pipelineRun := range pipelines {
		steps, err := client.ListPipelineSteps(sctx.Ctx, repo, pipelineRun.UUID)
		if err != nil {
			continue
		}
		for _, step := range steps {
			if strings.EqualFold(step.State.Result.Name, "FAILED") {
				logOutput, err := client.GetStepLog(sctx.Ctx, repo, pipelineRun.UUID, step.UUID)
				if err != nil || strings.TrimSpace(logOutput) == "" {
					continue
				}
				return trimLogOutput(strings.TrimSpace(logOutput), 32*1024)
			}
		}
	}
	return ""
}

// autoFixCI runs the agent to fix CI failures and/or merge conflicts, then commits and pushes.
// Returns (true, nil) when changes were committed and pushed, (false, nil)
// when the agent produced no changes, or (false, err) on failure.
func (s *CIStep) autoFixCI(sctx *pipeline.StepContext, prNumber string, failingNames []string, mergeConflict bool) (bool, error) {
	ctx := sctx.Ctx
	baseSHA := resolveBranchBaseSHA(ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)
	rebaseBaseSHA := resolveDefaultBranchTipSHA(ctx, sctx.WorkDir, sctx.Repo.UpstreamURL, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)
	promptBaseSHA := baseSHA
	if mergeConflict {
		promptBaseSHA = rebaseBaseSHA
	}

	// Find the most recent failing run for this branch so we fetch logs from the right run.
	provider := scm.DetectProvider(sctx.Repo.UpstreamURL)
	if provider == scm.ProviderUnknown && sctx.Run.PRURL != nil {
		provider = scm.DetectProvider(*sctx.Run.PRURL)
	}

	var runID string
	const maxLogBytes = 32 * 1024
	var logOutput string
	if provider == scm.ProviderBitbucket {
		client, err := bitbucket.NewClientFromEnv(sctx.Env)
		if err == nil {
			repo, repoErr := resolveBitbucketRepoRef(sctx.Repo.UpstreamURL, sctx.Run.PRURL)
			if repoErr == nil {
				prID, convErr := strconv.Atoi(prNumber)
				if convErr == nil {
					commitSHA := strings.TrimSpace(sctx.Run.HeadSHA)
					if pr, prErr := client.GetPR(sctx.Ctx, repo, prID); prErr == nil && pr != nil && strings.TrimSpace(pr.SourceCommitHash) != "" {
						commitSHA = strings.TrimSpace(pr.SourceCommitHash)
					}
					logOutput = s.fetchBitbucketFailedStepLogs(sctx, client, repo, commitSHA)
				}
			}
		}
	} else if len(failingNames) > 0 {
		listCmd := stepCmd(sctx, "gh", "run", "list",
			"--branch", sctx.Run.Branch,
			"--status", "failure",
			"--limit", "1",
			"--json", "databaseId",
			"--jq", ".[0].databaseId")
		if listOut, err := listCmd.Output(); err == nil {
			runID = strings.TrimSpace(string(listOut))
		}
	}

	// Attempt to fetch CI failure logs for context
	if runID != "" {
		cmd := stepCmd(sctx, "gh", "run", "view", runID, "--log-failed")
		out, _ := cmd.Output()
		if len(out) > 0 {
			logOutput = strings.TrimSpace(string(out))
			logOutput = trimLogOutput(logOutput, maxLogBytes)
		}
	}

	// Build prompt based on what issues are present
	var promptIntro string
	var promptRules string
	switch {
	case len(failingNames) > 0 && mergeConflict:
		promptIntro = "The following CI checks have failed and the PR has merge conflicts with the base branch. Diagnose and fix the CI issues, then rebase onto the base branch and resolve the merge conflicts."
		promptRules = `- You MUST produce file changes that fix the failing checks. Do not conclude that nothing needs to change.
		- If a test fails only on a specific OS (e.g. Windows CRLF, path separators), fix the test to be cross-platform.
		- If a test is flaky, make it deterministic.
		- Make the minimal change needed.
		- Do not refactor beyond what is needed.
		- Verify the fix by running the most relevant commands locally before finishing.`
	case mergeConflict:
		promptIntro = "The PR has merge conflicts with the base branch. Rebase onto the base branch and resolve the merge conflicts."
		promptRules = `- Resolve the merge conflicts by applying the minimal necessary changes.
		- Do not make unrelated file edits.
		- Verify the rebase completes cleanly before finishing.`
	default:
		promptIntro = "The following CI checks have failed on this PR. Diagnose and fix the issues."
		promptRules = `- You MUST produce file changes that fix the failing checks. Do not conclude that nothing needs to change.
		- If a test fails only on a specific OS (e.g. Windows CRLF, path separators), fix the test to be cross-platform.
		- If a test is flaky, make it deterministic.
		- Make the minimal change needed.
		- Do not refactor beyond what is needed.
		- Verify the fix by running the most relevant commands locally before finishing.`
	}

	prompt := fmt.Sprintf(
		`%s

Context:
- branch: %s
- base commit: %s
- target commit: %s
- PR number: %s
- failing checks: %s
- merge conflict: %v

		Rules:
		%s`,
		promptIntro,
		sctx.Run.Branch,
		promptBaseSHA,
		sctx.Run.HeadSHA,
		prNumber,
		strings.Join(failingNames, ", "),
		mergeConflict,
		promptRules,
	)
	if mergeConflict {
		prompt += fmt.Sprintf("\n- rebase target commit: %s", rebaseBaseSHA)
	}
	if logOutput != "" {
		prompt += fmt.Sprintf(`

CI logs:
%s`, logOutput)
	}

	sctx.Log("running agent to fix CI issues...")
	_, err := sctx.Agent.Run(ctx, agent.RunOpts{
		Prompt:  prompt,
		CWD:     sctx.WorkDir,
		OnChunk: sctx.LogChunk,
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
	status, err := stepGitRun(sctx, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("check CI changes: %w", err)
	}
	if strings.TrimSpace(status) == "" {
		sctx.Log("no changes to commit")
		headSHA, err := stepGitHeadSHA(sctx)
		if err == nil && headSHA != sctx.Run.HeadSHA {
			return s.pushUpdatedHeadSHA(sctx, headSHA)
		}
		return false, nil
	}

	if _, err := stepGitRun(sctx, "add", "-A"); err != nil {
		return false, fmt.Errorf("stage CI changes: %w", err)
	}
	if _, err := stepGitRun(sctx, "commit", "-m", "no-mistakes: apply CI fixes"); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	headSHA, err := stepGitHeadSHA(sctx)
	if err != nil {
		return false, fmt.Errorf("resolve head after commit: %w", err)
	}

	return s.pushUpdatedHeadSHA(sctx, headSHA)
}

func (s *CIStep) pushUpdatedHeadSHA(sctx *pipeline.StepContext, newHeadSHA string) (bool, error) {
	ref := normalizedBranchRef(sctx.Run.Branch)

	upstreamSHA, lsErr := stepGitLsRemote(sctx, sctx.Repo.UpstreamURL, ref)
	if lsErr != nil {
		slog.Warn("ls-remote failed, pushing without force-with-lease", "ref", ref, "error", lsErr)
	} else if upstreamSHA == newHeadSHA {
		if _, err := stepGitRun(sctx, "update-ref", ref, newHeadSHA); err != nil {
			return false, fmt.Errorf("update local branch ref: %w", err)
		}
		sctx.Run.HeadSHA = newHeadSHA
		if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, newHeadSHA); err != nil {
			return false, err
		}
		return false, nil
	}
	if err := stepGitPush(sctx, sctx.Repo.UpstreamURL, ref, upstreamSHA, upstreamSHA != ""); err != nil {
		if lsErr != nil {
			return false, fmt.Errorf("push (ls-remote failed: %v): %w", lsErr, err)
		}
		return false, fmt.Errorf("push: %w", err)
	}

	if _, err := stepGitRun(sctx, "update-ref", ref, newHeadSHA); err != nil {
		return false, fmt.Errorf("update local branch ref: %w", err)
	}
	sctx.Run.HeadSHA = newHeadSHA
	if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, newHeadSHA); err != nil {
		return false, err
	}

	sctx.Log("committed and pushed fixes")
	return true, nil
}
