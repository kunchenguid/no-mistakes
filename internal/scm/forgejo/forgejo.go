// Package forgejo implements scm.Host through forgejo-axi's stable JSON CLI.
package forgejo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os/exec"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

const (
	defaultExecutable          = "forgejo-axi"
	maxForgejoOutputBytes      = 1 << 20
	maxForgejoErrorOutputBytes = 64 << 10
)

var errForgejoOutputLimit = errors.New("forgejo-axi output exceeded limit")

// CmdFactory creates one non-shell forgejo-axi invocation.
type CmdFactory func(ctx context.Context, name string, args ...string) *exec.Cmd

// Options configures a Forgejo host adapter.
type Options struct {
	CommandFactory CmdFactory
	CLIAvailable   func(executable string) bool
	Executable     string
	BaseURL        string
	Repository     string
	TokenEnv       string
	Secrets        []string
}

// Host maps no-mistakes SCM operations to forgejo-axi --json commands.
type Host struct {
	cmdFactory   CmdFactory
	available    func(string) bool
	executable   string
	baseURL      string
	repository   string
	tokenEnv     string
	secrets      []string
	capabilities scm.Capabilities
}

// New constructs a Forgejo host. Callers should resolve the remote with
// ResolveRemote first so host and repository identity are pinned once.
func New(opts Options) *Host {
	executable := strings.TrimSpace(opts.Executable)
	if executable == "" {
		executable = defaultExecutable
	}
	available := opts.CLIAvailable
	if available == nil {
		available = func(name string) bool {
			_, err := exec.LookPath(name)
			return err == nil
		}
	}
	secrets := append([]string(nil), opts.Secrets...)
	sort.Slice(secrets, func(i, j int) bool {
		return len(secrets[i]) > len(secrets[j])
	})
	return &Host{
		cmdFactory: opts.CommandFactory,
		available:  available,
		executable: executable,
		baseURL:    strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/"),
		repository: strings.Trim(strings.TrimSpace(opts.Repository), "/"),
		tokenEnv:   strings.TrimSpace(opts.TokenEnv),
		secrets:    secrets,
	}
}

func (h *Host) Provider() scm.Provider { return scm.ProviderForgejo }

func (h *Host) Capabilities() scm.Capabilities { return h.capabilities }

func (h *Host) Available(ctx context.Context) error {
	if !h.available(h.executable) {
		return fmt.Errorf("Forgejo CLI %q not found; install forgejo-axi or configure forgejo_axi_path", h.executable)
	}
	if h.cmdFactory == nil {
		return errors.New("Forgejo command runner is not configured")
	}
	if _, _, err := ResolveRemote(h.baseURL+"/"+h.repository+".git", h.baseURL, ""); err != nil {
		return fmt.Errorf("invalid Forgejo host configuration: %w", err)
	}

	var response statusResponse
	if err := h.runJSON(ctx, "status", nil, &response); err != nil {
		return fmt.Errorf("Forgejo authentication check: %w", err)
	}
	if response.Host.URL == "" || strings.TrimRight(response.Host.URL, "/") != h.baseURL {
		return fmt.Errorf("Forgejo status returned host identity %q, expected %q", response.Host.URL, h.baseURL)
	}
	expectedAPIURL := h.baseURL + "/api/v1"
	if strings.TrimRight(response.Host.APIURL, "/") != expectedAPIURL {
		return fmt.Errorf("Forgejo status returned API identity %q, expected %q", response.Host.APIURL, expectedAPIURL)
	}
	if !response.Auth.Configured || !response.Auth.Authenticated {
		return errors.New("forgejo-axi is not authenticated for the configured Forgejo host; configure a host-scoped token")
	}
	if response.Auth.Source == nil || *response.Auth.Source == "" {
		return errors.New("Forgejo authentication source is incomplete")
	}
	if h.tokenEnv != "" && *response.Auth.Source != h.tokenEnv {
		return fmt.Errorf("Forgejo authentication source %q does not match token environment %q", *response.Auth.Source, h.tokenEnv)
	}
	if response.Capabilities.Probe.Source == "" || !response.Capabilities.Probe.Complete {
		return errors.New("Forgejo capability probe was incomplete; refusing to guess from the server version")
	}
	if !response.Capabilities.PullRequests {
		return errors.New("Forgejo host does not report pull-request capability")
	}
	h.capabilities = scm.Capabilities{
		MergeableState:    response.Capabilities.CommitStatuses && response.Capabilities.BranchProtection,
		MergedProof:       true,
		PullRequests:      response.Capabilities.PullRequests,
		CommitStatuses:    response.Capabilities.CommitStatuses,
		BranchProtection:  response.Capabilities.BranchProtection,
		ExpectedHeadMerge: response.Capabilities.ExpectedHeadMerge,
		ActionsJobLogs:    response.Capabilities.ActionsJobLogs,
		ActionsRuns:       response.Capabilities.Runs,
		ActionsRunJobs:    response.Capabilities.RunJobs,
		// Check gating depends only on commit statuses. Failed logs are optional
		// and require every released run-view route independently.
		FailedCheckLogs: response.Capabilities.CommitStatuses &&
			response.Capabilities.Runs &&
			response.Capabilities.RunJobs &&
			response.Capabilities.ActionsJobLogs,
	}
	return nil
}

func (h *Host) FindPR(ctx context.Context, branch, base string) (*scm.PR, error) {
	var response struct {
		Found       bool         `json:"found"`
		PullRequest *pullRequest `json:"pull_request"`
		SearchInfo  struct {
			Complete bool `json:"complete"`
			Pages    int  `json:"pages"`
			Fetched  int  `json:"fetched"`
			Total    *int `json:"total"`
		} `json:"search_info"`
	}
	args := []string{"--repo", h.repository, "--head", branch, "--base", base, "--state", "open"}
	if err := h.runJSON(ctx, "pr find", args, &response); err != nil {
		return nil, err
	}
	if !response.SearchInfo.Complete {
		return nil, errors.New("forgejo-axi PR search was incomplete; refusing to create a possible duplicate")
	}
	if response.SearchInfo.Pages <= 0 || response.SearchInfo.Fetched < 0 ||
		(response.SearchInfo.Total != nil && (*response.SearchInfo.Total < 0 || *response.SearchInfo.Total < response.SearchInfo.Fetched)) {
		return nil, errors.New("forgejo-axi returned inconsistent PR search metadata")
	}
	if !response.Found {
		if response.PullRequest != nil {
			return nil, errors.New("forgejo-axi returned an inconsistent not-found PR result")
		}
		return nil, nil
	}
	if response.PullRequest == nil || response.SearchInfo.Fetched == 0 {
		return nil, errors.New("forgejo-axi reported a found PR without consistent pull_request data")
	}
	pr, err := h.normalizePull(*response.PullRequest)
	if err != nil {
		return nil, err
	}
	if response.PullRequest.Head != branch || response.PullRequest.Base != base {
		return nil, fmt.Errorf("Forgejo PR branch identity mismatch: got %q -> %q, expected %q -> %q", response.PullRequest.Head, response.PullRequest.Base, branch, base)
	}
	return pr, nil
}

func (h *Host) CreatePR(ctx context.Context, branch, base string, content scm.PRContent) (*scm.PR, error) {
	var response struct {
		Created     bool        `json:"created"`
		PullRequest pullRequest `json:"pull_request"`
	}
	args := []string{"--repo", h.repository, "--head", branch, "--base", base, "--title", content.Title, "--body", content.Body}
	if err := h.runJSON(ctx, "pr create", args, &response); err != nil {
		return nil, err
	}
	pr, err := h.normalizePull(response.PullRequest)
	if err != nil {
		return nil, err
	}
	if response.PullRequest.Head != branch || response.PullRequest.Base != base {
		return nil, fmt.Errorf("created Forgejo PR branch identity mismatch: got %q -> %q, expected %q -> %q", response.PullRequest.Head, response.PullRequest.Base, branch, base)
	}
	return pr, nil
}

func (h *Host) UpdatePR(ctx context.Context, pr *scm.PR, content scm.PRContent) (*scm.PR, error) {
	number, err := h.validateInputPR(pr)
	if err != nil {
		return nil, err
	}
	var response struct {
		Updated     bool        `json:"updated"`
		PullRequest pullRequest `json:"pull_request"`
	}
	args := []string{"--repo", h.repository, number, "--title", content.Title, "--body", content.Body}
	if err := h.runJSON(ctx, "pr update", args, &response); err != nil {
		return nil, err
	}
	updated, err := h.normalizePull(response.PullRequest)
	if err != nil {
		return nil, err
	}
	if err := h.validateOutputPRNumber(number, response.PullRequest.Number); err != nil {
		return nil, err
	}
	return updated, nil
}

func (h *Host) GetPRState(ctx context.Context, pr *scm.PR) (scm.PRState, error) {
	number, err := h.validateInputPR(pr)
	if err != nil {
		return "", err
	}
	var response struct {
		PullRequest pullRequest `json:"pull_request"`
	}
	if err := h.runJSON(ctx, "pr view", []string{"--repo", h.repository, number}, &response); err != nil {
		return "", err
	}
	if _, err := h.normalizePull(response.PullRequest); err != nil {
		return "", err
	}
	if err := h.validateOutputPRNumber(number, response.PullRequest.Number); err != nil {
		return "", err
	}
	switch {
	case response.PullRequest.Merged:
		return scm.PRStateMerged, nil
	case response.PullRequest.State == "open":
		return scm.PRStateOpen, nil
	case response.PullRequest.State == "closed":
		return scm.PRStateClosed, nil
	default:
		return "", fmt.Errorf("forgejo-axi returned unknown PR state %q", response.PullRequest.State)
	}
}

func (h *Host) GetChecks(ctx context.Context, pr *scm.PR) ([]scm.Check, error) {
	result, err := h.readChecks(ctx, pr)
	if err != nil {
		return nil, err
	}
	return h.normalizeChecks(result)
}

func (h *Host) readChecks(ctx context.Context, pr *scm.PR) (checksResult, error) {
	number, err := h.validateInputPR(pr)
	if err != nil {
		return checksResult{}, err
	}
	if h.capabilities.PullRequests && !h.capabilities.CommitStatuses {
		return checksResult{}, fmt.Errorf("Forgejo commit-status capability unavailable: %w", scm.ErrUnsupported)
	}
	var response struct {
		Checks checksResult `json:"checks"`
	}
	if err := h.runJSON(ctx, "pr checks", []string{"--repo", h.repository, number}, &response); err != nil {
		return checksResult{}, err
	}
	return response.Checks, nil
}

func (h *Host) GetMergeableState(ctx context.Context, pr *scm.PR) (scm.MergeableState, error) {
	if !h.capabilities.MergeableState {
		return scm.MergeableUnknown, scm.ErrUnsupported
	}
	number, err := h.validateInputPR(pr)
	if err != nil {
		return scm.MergeableUnknown, err
	}
	var response struct {
		Mergeability struct {
			Number           int      `json:"number"`
			URL              string   `json:"url"`
			ForgejoMergeable *bool    `json:"forgejo_mergeable"`
			Reasons          []string `json:"reasons"`
		} `json:"mergeability"`
	}
	if err := h.runJSON(ctx, "pr mergeability", []string{"--repo", h.repository, number}, &response); err != nil {
		return scm.MergeableUnknown, err
	}
	if err := h.validateOutputIdentity(response.Mergeability.Number, response.Mergeability.URL); err != nil {
		return scm.MergeableUnknown, err
	}
	if err := h.validateOutputPRNumber(number, response.Mergeability.Number); err != nil {
		return scm.MergeableUnknown, err
	}
	for _, reason := range response.Mergeability.Reasons {
		if reason == "already_merged" {
			return scm.MergeableOK, nil
		}
	}
	if response.Mergeability.ForgejoMergeable == nil {
		return scm.MergeablePending, nil
	}
	if !*response.Mergeability.ForgejoMergeable {
		return scm.MergeableConflict, nil
	}
	return scm.MergeableOK, nil
}

func (h *Host) GetMergedProof(ctx context.Context, pr *scm.PR, expectedHead string) (scm.MergedProof, error) {
	number, err := h.validateInputPR(pr)
	if err != nil {
		return scm.MergedProof{}, err
	}
	expectedHead = strings.TrimSpace(expectedHead)
	if expectedHead == "" {
		return scm.MergedProof{}, errors.New("Forgejo merged proof requires an expected head SHA")
	}
	var response struct {
		Proof mergedProof `json:"proof"`
	}
	if err := h.runJSON(ctx, "pr merged", []string{"--repo", h.repository, number}, &response); err != nil {
		return scm.MergedProof{}, err
	}
	proof := response.Proof
	if err := h.validateOutputIdentity(proof.Number, proof.URL); err != nil {
		return scm.MergedProof{}, err
	}
	if err := h.validateOutputPRNumber(number, proof.Number); err != nil {
		return scm.MergedProof{}, err
	}
	if err := h.validateExpectedHead(proof.HeadSHA, expectedHead); err != nil {
		return scm.MergedProof{}, err
	}
	result := scm.MergedProof{
		Merged:         proof.Merged,
		Number:         strconv.Itoa(proof.Number),
		URL:            proof.URL,
		HeadSHA:        proof.HeadSHA,
		MergeCommitSHA: stringValue(proof.MergeCommitSHA),
		MergedBy:       stringValue(proof.MergedBy),
	}
	if proof.MergedAt != nil {
		parsed, err := time.Parse(time.RFC3339, *proof.MergedAt)
		if err != nil {
			return scm.MergedProof{}, fmt.Errorf("invalid Forgejo merged_at timestamp: %w", err)
		}
		result.MergedAt = parsed
	}
	if result.Merged && (result.MergeCommitSHA == "" || result.MergedAt.IsZero() || result.MergedBy == "") {
		return scm.MergedProof{}, errors.New("forgejo-axi returned incomplete evidence for a merged PR")
	}
	return result, nil
}

func (h *Host) FetchFailedCheckLogs(ctx context.Context, pr *scm.PR, _ string, _ string, failingNames []string) (string, error) {
	if !h.capabilities.FailedCheckLogs {
		return "", scm.ErrUnsupported
	}
	if len(failingNames) == 0 {
		return "", nil
	}
	targets := make(map[string]struct{}, len(failingNames))
	for _, name := range failingNames {
		if name = strings.TrimSpace(name); name != "" {
			targets[name] = struct{}{}
		}
	}
	if len(targets) == 0 {
		return "", nil
	}

	result, err := h.readChecks(ctx, pr)
	if err != nil {
		return "", err
	}
	checks, err := h.normalizeChecks(result)
	if err != nil {
		return "", err
	}

	jobIndicesByRunNumber := make(map[int]map[int]struct{})
	for _, check := range checks {
		if check.State != "failure" {
			continue
		}
		if _, ok := targets[check.Name]; !ok {
			continue
		}
		runNumber, jobIndex, ok := h.actionsRunJobFromTarget(check.Link)
		if ok {
			if jobIndicesByRunNumber[runNumber] == nil {
				jobIndicesByRunNumber[runNumber] = make(map[int]struct{})
			}
			jobIndicesByRunNumber[runNumber][jobIndex] = struct{}{}
		}
	}
	if len(jobIndicesByRunNumber) == 0 {
		return "", nil
	}
	runNumbers := make([]int, 0, len(jobIndicesByRunNumber))
	for runNumber := range jobIndicesByRunNumber {
		runNumbers = append(runNumbers, runNumber)
	}
	sort.Ints(runNumbers)

	var listed runListResponse
	if err := h.runJSONWithLimit(ctx, "run list", []string{"--repo", h.repository, "--fields", "all"}, &listed, maxForgejoOutputBytes); err != nil {
		return "", err
	}
	if !listed.PageInfo.Complete {
		return "", errors.New("forgejo-axi run list did not return a complete run identity set")
	}
	runIDsByNumber := make(map[int]int, len(runNumbers))
	seenRunIDs := make(map[int]struct{}, len(runNumbers))
	for _, runNumber := range runNumbers {
		matched := -1
		for i, run := range listed.Runs {
			if run.RunNumber != runNumber || run.HeadSHA != result.SHA {
				continue
			}
			if matched >= 0 {
				return "", fmt.Errorf("forgejo-axi run list returned multiple runs numbered %d at pull request head %s", runNumber, result.SHA)
			}
			matched = i
		}
		if matched < 0 {
			return "", fmt.Errorf("forgejo-axi run list did not return run number %d at pull request head %s", runNumber, result.SHA)
		}
		run := listed.Runs[matched]
		if err := h.validateRunIdentity(run, run.ID, runNumber, result.SHA); err != nil {
			return "", err
		}
		if _, duplicate := seenRunIDs[run.ID]; duplicate {
			return "", fmt.Errorf("forgejo-axi run list mapped multiple run numbers to run %d", run.ID)
		}
		seenRunIDs[run.ID] = struct{}{}
		runIDsByNumber[runNumber] = run.ID
	}

	var logs strings.Builder
	remaining := maxForgejoOutputBytes
	for _, runNumber := range runNumbers {
		runID := runIDsByNumber[runNumber]
		var response runViewResponse
		args := []string{"--repo", h.repository, strconv.Itoa(runID), "--log-failed"}
		if err := h.runJSONWithLimit(ctx, "run view", args, &response, maxForgejoOutputBytes); err != nil {
			return "", err
		}
		if err := h.validateRunView(response, runID, runNumber, result.SHA); err != nil {
			return "", err
		}
		jobIndices := make([]int, 0, len(jobIndicesByRunNumber[runNumber]))
		for jobIndex := range jobIndicesByRunNumber[runNumber] {
			jobIndices = append(jobIndices, jobIndex)
		}
		sort.Ints(jobIndices)
		selectedJobIDs := make(map[int]struct{}, len(jobIndices))
		for _, jobIndex := range jobIndices {
			if jobIndex >= len(response.Jobs) {
				return "", fmt.Errorf("Forgejo Actions run %d target job index %d is absent from %d returned jobs", runID, jobIndex, len(response.Jobs))
			}
			job := response.Jobs[jobIndex]
			if job.Status != "failure" {
				return "", fmt.Errorf("Forgejo Actions run %d target job %d has status %q, expected failure", runID, job.ID, job.Status)
			}
			selectedJobIDs[job.ID] = struct{}{}
		}
		sort.Slice(response.Jobs, func(i, j int) bool {
			return response.Jobs[i].ID < response.Jobs[j].ID
		})
		for _, job := range response.Jobs {
			if _, ok := selectedJobIDs[job.ID]; !ok {
				continue
			}
			if job.Log == nil {
				return "", fmt.Errorf("forgejo-axi run view returned failed job %d without a log", job.ID)
			}
			logText := strings.TrimSpace(*job.Log)
			if logText == "" {
				continue
			}
			block := fmt.Sprintf("Forgejo Actions run %d, job %s:\n%s", runNumber, job.Name, logText)
			separator := ""
			if logs.Len() > 0 {
				separator = "\n\n"
			}
			if len(separator)+len(block) > remaining {
				return "", fmt.Errorf("Forgejo failed check logs exceeded %d bytes: %w", maxForgejoOutputBytes, errForgejoOutputLimit)
			}
			logs.WriteString(separator)
			logs.WriteString(block)
			remaining -= len(separator) + len(block)
		}
	}
	return logs.String(), nil
}

func (h *Host) actionsRunJobFromTarget(raw string) (int, int, bool) {
	target, err := url.Parse(raw)
	if err != nil || target.Scheme != "" || target.Host != "" || target.User != nil || target.ForceQuery || target.RawQuery != "" || target.Fragment != "" || target.RawPath != "" {
		return 0, 0, false
	}
	prefix := "/" + h.repository + "/actions/runs/"
	if !strings.HasPrefix(target.Path, prefix) {
		return 0, 0, false
	}
	parts := strings.Split(strings.TrimPrefix(target.Path, prefix), "/")
	if len(parts) != 3 || parts[1] != "jobs" {
		return 0, 0, false
	}
	runNumber, err := strconv.Atoi(parts[0])
	if err != nil || runNumber <= 0 || parts[0] != strconv.Itoa(runNumber) {
		return 0, 0, false
	}
	jobIndex, err := strconv.Atoi(parts[2])
	if err != nil || jobIndex < 0 || parts[2] != strconv.Itoa(jobIndex) {
		return 0, 0, false
	}
	canonical := prefix + strconv.Itoa(runNumber) + "/jobs/" + strconv.Itoa(jobIndex)
	if raw != canonical {
		return 0, 0, false
	}
	return runNumber, jobIndex, true
}

func (h *Host) validateRunIdentity(run actionRun, expectedRunID, expectedRunNumber int, expectedHead string) error {
	if run.ID <= 0 || run.ID != expectedRunID {
		return fmt.Errorf("forgejo-axi returned run %d, expected %d", run.ID, expectedRunID)
	}
	if run.RunNumber != expectedRunNumber {
		return fmt.Errorf("forgejo-axi returned run %d with number %d, expected %d", run.ID, run.RunNumber, expectedRunNumber)
	}
	if err := h.validateExpectedHead(run.HeadSHA, expectedHead); err != nil {
		return err
	}
	wantURL := h.baseURL + "/" + h.repository + "/actions/runs/" + strconv.Itoa(expectedRunID)
	wantAPIURL := h.baseURL + "/api/v1/repos/" + h.repository + "/actions/runs/" + strconv.Itoa(expectedRunID)
	if run.URL != wantURL || run.APIURL != wantAPIURL {
		return fmt.Errorf("forgejo-axi run identity mismatch: got %q and %q", run.URL, run.APIURL)
	}
	return nil
}

func (h *Host) validateRunView(response runViewResponse, expectedRunID, expectedRunNumber int, expectedHead string) error {
	if err := h.validateRunIdentity(response.Run, expectedRunID, expectedRunNumber, expectedHead); err != nil {
		return err
	}
	if len(response.Next) != 0 {
		return fmt.Errorf("forgejo-axi run view did not provide requested failed logs: %s", strings.Join(response.Next, "; "))
	}
	jobIDs := make(map[int]struct{}, len(response.Jobs))
	for _, job := range response.Jobs {
		if job.ID <= 0 || job.RunID != expectedRunID {
			return fmt.Errorf("forgejo-axi run view returned job %d for run %d, expected run %d", job.ID, job.RunID, expectedRunID)
		}
		if strings.TrimSpace(job.Name) == "" {
			return fmt.Errorf("forgejo-axi run view returned job %d without a name", job.ID)
		}
		if _, duplicate := jobIDs[job.ID]; duplicate {
			return fmt.Errorf("forgejo-axi run view returned duplicate job %d", job.ID)
		}
		jobIDs[job.ID] = struct{}{}
	}
	return nil
}

func (h *Host) normalizePull(pull pullRequest) (*scm.PR, error) {
	if err := h.validateOutputIdentity(pull.Number, pull.URL); err != nil {
		return nil, err
	}
	wantAPI := h.baseURL + "/api/v1/repos/" + h.repository + "/pulls/" + strconv.Itoa(pull.Number)
	if pull.APIURL != wantAPI {
		return nil, fmt.Errorf("Forgejo PR API identity mismatch: got %q, expected %q", pull.APIURL, wantAPI)
	}
	if pull.Head == "" || pull.Base == "" || pull.HeadSHA == "" || pull.Title == "" {
		return nil, errors.New("forgejo-axi returned incomplete PR identity data")
	}
	if pull.State != "open" && pull.State != "closed" {
		return nil, fmt.Errorf("forgejo-axi returned unknown PR state %q", pull.State)
	}
	return &scm.PR{Number: strconv.Itoa(pull.Number), URL: pull.URL, HeadSHA: pull.HeadSHA}, nil
}

func (h *Host) normalizeChecks(result checksResult) ([]scm.Check, error) {
	if result.SHA == "" {
		return nil, errors.New("forgejo-axi returned checks without a head SHA")
	}
	if result.Reported != len(result.Statuses) {
		return nil, fmt.Errorf("forgejo-axi reported %d statuses but returned %d", result.Reported, len(result.Statuses))
	}
	if result.Protection == nil {
		return nil, errors.New("forgejo-axi returned checks without branch protection data")
	}
	if !result.Protection.StatusChecksEnabled && len(result.Required) != 0 {
		return nil, errors.New("forgejo-axi returned required contexts while status checks are disabled")
	}
	if err := validateCheckSummary(result); err != nil {
		return nil, err
	}
	checks := make([]scm.Check, 0, len(result.Statuses)+len(result.Required)+1)
	for _, status := range result.Statuses {
		if status.Context == "" {
			return nil, errors.New("forgejo-axi returned a status without context")
		}
		bucket, err := statusBucket(status.State)
		if err != nil {
			return nil, err
		}
		check := scm.Check{
			Name:   status.Context,
			Bucket: bucket,
			State:  status.State,
			Link:   stringValue(status.TargetURL),
		}
		if status.UpdatedAt != nil {
			check.CompletedAt, err = time.Parse(time.RFC3339, *status.UpdatedAt)
			if err != nil {
				return nil, fmt.Errorf("invalid Forgejo status updated_at timestamp: %w", err)
			}
		}
		checks = append(checks, check)
	}
	for _, required := range result.Required {
		if required.Context == "" {
			return nil, errors.New("forgejo-axi returned a required status without context")
		}
		if required.State == "missing" {
			if len(required.Matched) != 0 {
				return nil, fmt.Errorf("required context %q is missing but has matches", required.Context)
			}
			checks = append(checks, scm.Check{Name: "required status: " + required.Context, Bucket: scm.CheckBucketFail})
		}
	}
	return checks, nil
}

func validateCheckSummary(result checksResult) error {
	statusStates := make(map[string]string, len(result.Statuses))
	expectedState := "none"
	if len(result.Statuses) != 0 {
		expectedState = "success"
	}
	for _, status := range result.Statuses {
		if status.Context == "" {
			return errors.New("forgejo-axi returned a status without context")
		}
		if _, exists := statusStates[status.Context]; exists {
			return fmt.Errorf("forgejo-axi returned duplicate status context %q", status.Context)
		}
		switch status.State {
		case "pending", "failure", "success":
		default:
			return fmt.Errorf("forgejo-axi returned unknown commit status state %q", status.State)
		}
		statusStates[status.Context] = status.State
		expectedState = worseCheckState(expectedState, status.State)
	}
	if result.State != expectedState {
		return fmt.Errorf("forgejo-axi returned inconsistent checks state %q, expected %q", result.State, expectedState)
	}

	expectedRequiredState := "not_required"
	if len(result.Required) != 0 {
		expectedRequiredState = "success"
		for _, required := range result.Required {
			if required.Context == "" {
				return errors.New("forgejo-axi returned a required status without context")
			}
			switch required.State {
			case "missing":
				if len(required.Matched) != 0 {
					return fmt.Errorf("required context %q is missing but has matches", required.Context)
				}
			case "pending", "failure", "success":
				if len(required.Matched) == 0 {
					return fmt.Errorf("required context %q has state %q but has no matches", required.Context, required.State)
				}
				matchedState := "success"
				seenMatched := make(map[string]struct{}, len(required.Matched))
				for _, matched := range required.Matched {
					state, exists := statusStates[matched]
					if !exists {
						return fmt.Errorf("required context %q matched unknown status %q", required.Context, matched)
					}
					if _, duplicate := seenMatched[matched]; duplicate {
						return fmt.Errorf("required context %q repeated matched status %q", required.Context, matched)
					}
					seenMatched[matched] = struct{}{}
					matchedState = worseCheckState(matchedState, state)
				}
				if required.State != matchedState {
					return fmt.Errorf("required context %q has inconsistent state %q, expected %q", required.Context, required.State, matchedState)
				}
			default:
				return fmt.Errorf("forgejo-axi returned unknown required status state %q", required.State)
			}
			expectedRequiredState = worseRequiredState(expectedRequiredState, required.State)
		}
	}
	if result.RequiredState != expectedRequiredState {
		return fmt.Errorf("forgejo-axi returned inconsistent required status summary %q, expected %q", result.RequiredState, expectedRequiredState)
	}
	expectedPasses := result.RequiredState == "success" || (result.RequiredState == "not_required" && result.State == "success")
	if result.Passes != expectedPasses {
		return errors.New("forgejo-axi returned inconsistent passing status summary")
	}
	return nil
}

func worseCheckState(current, candidate string) string {
	if current == "failure" || candidate == "failure" {
		return "failure"
	}
	if current == "pending" || candidate == "pending" {
		return "pending"
	}
	return candidate
}

func worseRequiredState(current, candidate string) string {
	rank := map[string]int{"not_required": 0, "success": 1, "pending": 2, "missing": 3, "failure": 4}
	if rank[candidate] > rank[current] {
		return candidate
	}
	return current
}

func statusBucket(state string) (scm.CheckBucket, error) {
	switch state {
	case "success":
		return scm.CheckBucketPass, nil
	case "pending":
		return scm.CheckBucketPending, nil
	case "failure":
		return scm.CheckBucketFail, nil
	default:
		return "", fmt.Errorf("forgejo-axi returned unknown commit status state %q", state)
	}
}

func (h *Host) validateInputPR(pr *scm.PR) (string, error) {
	if pr == nil {
		return "", errors.New("Forgejo PR is nil")
	}
	number, err := strconv.Atoi(pr.Number)
	if err != nil || number <= 0 {
		return "", fmt.Errorf("invalid Forgejo PR number %q", pr.Number)
	}
	if err := h.validateOutputIdentity(number, pr.URL); err != nil {
		return "", fmt.Errorf("Forgejo PR input identity: %w", err)
	}
	return strconv.Itoa(number), nil
}

func (h *Host) validateOutputIdentity(number int, gotURL string) error {
	if number <= 0 {
		return fmt.Errorf("forgejo-axi returned invalid PR number %d", number)
	}
	want := h.canonicalPRURL(number)
	if gotURL != want {
		return fmt.Errorf("Forgejo PR identity mismatch: got %q, expected %q", gotURL, want)
	}
	return nil
}

func (h *Host) validateOutputPRNumber(expected string, got int) error {
	if strconv.Itoa(got) != expected {
		return fmt.Errorf("Forgejo PR number mismatch: got %d, expected %s", got, expected)
	}
	return nil
}

func (h *Host) validateExpectedHead(got, expected string) error {
	if got == "" {
		return errors.New("forgejo-axi returned an empty PR head SHA")
	}
	if expected != "" && got != expected {
		return fmt.Errorf("%w: Forgejo reported %s, expected %s", scm.ErrHeadChanged, got, expected)
	}
	return nil
}

func (h *Host) canonicalPRURL(number int) string {
	return h.baseURL + "/" + h.repository + "/pulls/" + strconv.Itoa(number)
}

func (h *Host) runJSON(ctx context.Context, operation string, operationArgs []string, dst any) error {
	return h.runJSONWithLimit(ctx, operation, operationArgs, dst, 0)
}

func (h *Host) runJSONWithLimit(ctx context.Context, operation string, operationArgs []string, dst any, maxStdoutBytes int) error {
	if maxStdoutBytes <= 0 {
		maxStdoutBytes = maxForgejoOutputBytes
	}
	args := append(strings.Fields(operation), operationArgs...)
	args = append(args, "--base-url", h.baseURL)
	if h.tokenEnv != "" {
		args = append(args, "--token-env", h.tokenEnv)
	}
	args = append(args, "--json")
	cmd := h.cmdFactory(ctx, h.executable, args...)
	if cmd == nil {
		return errors.New("Forgejo command runner returned a nil command")
	}
	shellenv.ConfigureShellCommand(cmd)
	var stdout cappedBuffer
	stdout.limit = maxStdoutBytes
	stderr := prefixBuffer{limit: maxForgejoErrorOutputBytes}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := shellenv.RunShellCommand(cmd)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if stdout.exceeded {
		return fmt.Errorf("forgejo-axi %s output exceeded %d bytes: %w", operation, maxStdoutBytes, errForgejoOutputLimit)
	}
	if err != nil {
		return h.commandError(operation, err, stdout.String(), stderr.String(h.secrets))
	}
	if err := decodeSingleJSON(stdout.Bytes(), dst); err != nil {
		return fmt.Errorf("forgejo-axi %s returned invalid JSON: %w", operation, err)
	}
	return nil
}

type cappedBuffer struct {
	buf      bytes.Buffer
	limit    int
	exceeded bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		return b.buf.Write(p)
	}
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		b.exceeded = true
		return 0, errForgejoOutputLimit
	}
	if len(p) <= remaining {
		return b.buf.Write(p)
	}
	n, _ := b.buf.Write(p[:remaining])
	b.exceeded = true
	return n, errForgejoOutputLimit
}

func (b *cappedBuffer) Bytes() []byte  { return b.buf.Bytes() }
func (b *cappedBuffer) String() string { return b.buf.String() }

type prefixBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *prefixBuffer) Write(p []byte) (int, error) {
	written := len(p)
	if remaining := b.limit - b.buf.Len(); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
			b.truncated = true
		}
		_, _ = b.buf.Write(p)
	} else if len(p) > 0 {
		b.truncated = true
	}
	return written, nil
}

func (b *prefixBuffer) String(secrets []string) string {
	value := b.buf.String()
	if !b.truncated {
		return value
	}
	trim := 0
	for _, secret := range secrets {
		maxPrefix := len(secret) - 1
		if maxPrefix > len(value) {
			maxPrefix = len(value)
		}
		for length := maxPrefix; length > trim; length-- {
			if strings.HasSuffix(value, secret[:length]) {
				trim = length
				break
			}
		}
	}
	return value[:len(value)-trim]
}

func (h *Host) commandError(operation string, commandErr error, stdout, stderr string) error {
	message := strings.TrimSpace(stdout)
	var cliErr struct {
		Error   string          `json:"error"`
		Code    string          `json:"code"`
		Details json.RawMessage `json:"details"`
		Help    []string        `json:"help"`
	}
	if decodeSingleJSON([]byte(message), &cliErr) == nil && cliErr.Error != "" {
		parts := []string{cliErr.Error}
		if len(cliErr.Details) > 0 && string(cliErr.Details) != "null" {
			parts = append(parts, string(cliErr.Details))
		}
		if len(cliErr.Help) > 0 {
			parts = append(parts, "help: "+strings.Join(cliErr.Help, "; "))
		}
		message = strings.Join(parts, "; ")
		if cliErr.Code != "" {
			message = cliErr.Code + ": " + message
		}
	} else if strings.TrimSpace(stderr) != "" {
		message = strings.TrimSpace(stderr)
	}
	if message == "" {
		message = commandErr.Error()
	}
	return fmt.Errorf("forgejo-axi %s failed: %s", operation, h.redact(message))
}

func (h *Host) redact(value string) string {
	value = safeurl.RedactText(value)
	for _, secret := range h.secrets {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	return value
}

func decodeSingleJSON(data []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return errors.New("multiple JSON values")
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("trailing data: %w", err)
	}
	return nil
}

type statusResponse struct {
	Host struct {
		URL    string `json:"url"`
		APIURL string `json:"api_url"`
	} `json:"host"`
	Auth struct {
		Configured    bool    `json:"configured"`
		Authenticated bool    `json:"authenticated"`
		Source        *string `json:"source"`
	} `json:"auth"`
	Capabilities struct {
		PullRequests      bool `json:"pull_requests"`
		CommitStatuses    bool `json:"commit_statuses"`
		BranchProtection  bool `json:"branch_protection"`
		ExpectedHeadMerge bool `json:"expected_head_merge"`
		ActionsJobLogs    bool `json:"actions_job_logs"`
		Runs              bool `json:"runs"`
		RunJobs           bool `json:"run_jobs"`
		RunCancel         bool `json:"run_cancel"`
		RunArtifacts      bool `json:"run_artifacts"`
		Probe             struct {
			Source   string `json:"source"`
			Complete bool   `json:"complete"`
		} `json:"probe"`
	} `json:"capabilities"`
}

type pullRequest struct {
	Number         int     `json:"number"`
	URL            string  `json:"url"`
	APIURL         string  `json:"api_url"`
	State          string  `json:"state"`
	Draft          bool    `json:"draft"`
	Title          string  `json:"title"`
	Head           string  `json:"head"`
	Base           string  `json:"base"`
	HeadSHA        string  `json:"head_sha"`
	Mergeable      *bool   `json:"mergeable"`
	Merged         bool    `json:"merged"`
	MergeCommitSHA *string `json:"merge_commit_sha"`
	MergedAt       *string `json:"merged_at"`
	MergedBy       *string `json:"merged_by"`
}

type checksResult struct {
	SHA           string           `json:"sha"`
	Reported      int              `json:"reported"`
	State         string           `json:"state"`
	Statuses      []commitStatus   `json:"statuses"`
	Required      []requiredStatus `json:"required"`
	RequiredState string           `json:"required_state"`
	Passes        bool             `json:"passes"`
	Protection    *checkProtection `json:"protection"`
}

type checkProtection struct {
	Protected           bool    `json:"protected"`
	Rule                *string `json:"rule"`
	StatusChecksEnabled bool    `json:"status_checks_enabled"`
}

type commitStatus struct {
	Context   string  `json:"context"`
	State     string  `json:"state"`
	TargetURL *string `json:"target_url"`
	UpdatedAt *string `json:"updated_at"`
}

type runViewResponse struct {
	Run  actionRun   `json:"run"`
	Jobs []actionJob `json:"jobs"`
	Next []string    `json:"next"`
}

type runListResponse struct {
	Runs     []actionRun `json:"runs"`
	PageInfo struct {
		Complete bool `json:"complete"`
	} `json:"page_info"`
}

type actionRun struct {
	ID        int    `json:"id"`
	URL       string `json:"url"`
	APIURL    string `json:"api_url"`
	HeadSHA   string `json:"head_sha"`
	RunNumber int    `json:"run_number"`
}

type actionJob struct {
	ID     int     `json:"id"`
	RunID  int     `json:"run_id"`
	Name   string  `json:"name"`
	Status string  `json:"status"`
	Log    *string `json:"log"`
}

type requiredStatus struct {
	Context string   `json:"context"`
	Matched []string `json:"matched"`
	State   string   `json:"state"`
}

type mergedProof struct {
	Merged         bool    `json:"merged"`
	Number         int     `json:"number"`
	URL            string  `json:"url"`
	HeadSHA        string  `json:"head_sha"`
	MergeCommitSHA *string `json:"merge_commit_sha"`
	MergedAt       *string `json:"merged_at"`
	MergedBy       *string `json:"merged_by"`
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// ResolveRemote pins a Forgejo web base URL and OWNER/REPO identity from a
// remote. configuredBase is required for arbitrary self-hosted names and SSH
// origins; recognizable HTTPS Forgejo origins can infer a path prefix from all
// path segments before OWNER/REPO.
func ResolveRemote(remote, configuredBase, resolvedSSHHost string) (string, string, error) {
	remoteHost, remotePath, remoteScheme, err := parseRemote(remote)
	if err != nil {
		return "", "", err
	}
	remotePath = stripPullURLSuffix(remotePath)
	if configuredBase != "" {
		base, baseURL, err := normalizeBaseURL(configuredBase)
		if err != nil {
			return "", "", err
		}
		if remoteScheme == "http" || remoteScheme == "https" {
			remoteURL, parseErr := url.Parse(strings.TrimSpace(remote))
			if parseErr == nil {
				_, remoteURL, parseErr = normalizeBaseURL((&url.URL{Scheme: remoteURL.Scheme, Host: remoteURL.Host}).String())
			}
			if parseErr != nil || remoteURL.Host != baseURL.Host {
				return "", "", fmt.Errorf("remote host %q does not match configured Forgejo host %q", remoteHost, baseURL.Host)
			}
		} else {
			resolvedSSHHost = strings.TrimSpace(resolvedSSHHost)
			if resolvedSSHHost == "" {
				resolvedSSHHost = remoteHost
			}
			if !strings.EqualFold(resolvedSSHHost, baseURL.Hostname()) {
				return "", "", fmt.Errorf("remote host %q does not match configured Forgejo host %q", resolvedSSHHost, baseURL.Hostname())
			}
		}
		prefix := strings.Trim(baseURL.Path, "/")
		repo, err := repositoryAfterPrefix(remotePath, prefix)
		if err != nil {
			return "", "", err
		}
		return base, repo, nil
	}
	if remoteScheme != "http" && remoteScheme != "https" {
		return "", "", errors.New("FORGEJO_BASE_URL is required for an SSH Forgejo origin")
	}
	parts := splitPath(remotePath)
	if len(parts) < 2 {
		return "", "", fmt.Errorf("Forgejo remote path %q does not contain OWNER/REPO", remotePath)
	}
	repo, err := validRepository(parts[len(parts)-2], parts[len(parts)-1])
	if err != nil {
		return "", "", err
	}
	remoteURL, err := url.Parse(strings.TrimSpace(remote))
	if err != nil {
		return "", "", fmt.Errorf("invalid Forgejo remote URL %q: %w", remote, err)
	}
	remoteURL.User = nil
	remoteURL.RawQuery = ""
	remoteURL.Fragment = ""
	remoteURL.Path = "/" + strings.Join(parts[:len(parts)-2], "/")
	remoteURL.RawPath = ""
	base, _, err := normalizeBaseURL(remoteURL.String())
	if err != nil {
		return "", "", err
	}
	return base, repo, nil
}

func normalizeBaseURL(raw string) (string, *url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", nil, fmt.Errorf("invalid Forgejo base URL %q", raw)
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	hostname := strings.ToLower(parsed.Hostname())
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || hostname == "" {
		return "", nil, fmt.Errorf("invalid Forgejo base URL %q", raw)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", nil, errors.New("Forgejo base URL must not contain credentials, a query, or a fragment")
	}
	port := parsed.Port()
	if (parsed.Scheme == "http" && port == "80") || (parsed.Scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		parsed.Host = net.JoinHostPort(hostname, port)
	} else if strings.Contains(hostname, ":") {
		parsed.Host = "[" + hostname + "]"
	} else {
		parsed.Host = hostname
	}
	parsed.Path = strings.TrimRight(path.Clean("/"+strings.Trim(parsed.Path, "/")), "/")
	if parsed.Path == "." || parsed.Path == "/" {
		parsed.Path = ""
	}
	parsed.RawPath = ""
	return strings.TrimRight(parsed.String(), "/"), parsed, nil
}

func parseRemote(remote string) (host, remotePath, scheme string, err error) {
	raw := strings.TrimSpace(remote)
	if raw == "" {
		return "", "", "", errors.New("empty Forgejo remote URL")
	}
	if strings.Contains(raw, "://") {
		parsed, parseErr := url.Parse(raw)
		if parseErr != nil || parsed.Hostname() == "" {
			return "", "", "", fmt.Errorf("invalid Forgejo remote URL %q", remote)
		}
		if parsed.RawQuery != "" || parsed.Fragment != "" {
			return "", "", "", errors.New("Forgejo remote URL must not contain a query or fragment")
		}
		scheme := strings.ToLower(parsed.Scheme)
		if scheme != "http" && scheme != "https" && scheme != "ssh" {
			return "", "", "", fmt.Errorf("unsupported Forgejo remote URL scheme %q", parsed.Scheme)
		}
		return strings.ToLower(parsed.Hostname()), parsed.Path, scheme, nil
	}
	colon := strings.Index(raw, ":")
	if colon <= 0 {
		return "", "", "", fmt.Errorf("invalid Forgejo remote URL %q", remote)
	}
	hostPart := raw[:colon]
	if at := strings.LastIndex(hostPart, "@"); at >= 0 {
		hostPart = hostPart[at+1:]
	}
	if hostPart == "" {
		return "", "", "", fmt.Errorf("invalid Forgejo remote URL %q", remote)
	}
	return strings.ToLower(hostPart), raw[colon+1:], "ssh", nil
}

func repositoryAfterPrefix(remotePath, prefix string) (string, error) {
	parts := splitPath(remotePath)
	prefixParts := splitPath(prefix)
	if len(parts) != len(prefixParts)+2 {
		return "", fmt.Errorf("Forgejo remote path %q does not match configured path prefix %q and OWNER/REPO", remotePath, prefix)
	}
	for i := range prefixParts {
		if parts[i] != prefixParts[i] {
			return "", fmt.Errorf("Forgejo remote path %q does not match configured path prefix %q", remotePath, prefix)
		}
	}
	return validRepository(parts[len(parts)-2], parts[len(parts)-1])
}

func stripPullURLSuffix(value string) string {
	parts := splitPath(value)
	if len(parts) >= 4 && parts[len(parts)-2] == "pulls" {
		if number, err := strconv.Atoi(parts[len(parts)-1]); err == nil && number > 0 {
			return "/" + strings.Join(parts[:len(parts)-2], "/")
		}
	}
	return value
}

func splitPath(value string) []string {
	value = strings.Trim(strings.TrimSpace(value), "/")
	value = strings.TrimSuffix(value, ".git")
	if value == "" {
		return nil
	}
	return strings.Split(value, "/")
}

func validRepository(owner, repo string) (string, error) {
	if owner == "" || repo == "" || owner == "." || owner == ".." || repo == "." || repo == ".." {
		return "", errors.New("invalid Forgejo OWNER/REPO identity")
	}
	if strings.ContainsAny(owner+repo, "\\\x00\r\n") {
		return "", errors.New("invalid Forgejo OWNER/REPO identity")
	}
	return owner + "/" + repo, nil
}
