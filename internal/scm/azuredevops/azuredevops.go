// Package azuredevops implements scm.Host backed by the az CLI.
package azuredevops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// CmdFactory builds an exec.Cmd in the caller's workdir with the caller's env.
type CmdFactory func(ctx context.Context, name string, args ...string) *exec.Cmd

// Host talks to Azure DevOps through the az CLI.
type Host struct {
	cmd          CmdFactory
	cliAvailable func() bool
	org          string // "https://dev.azure.com/talroo"
	project      string // "Product"
	repoName     string // "ai-knowledge-base" (name, not GUID)
	repoID       string // GUID, lazily resolved
	repoIDFn     func(ctx context.Context) (string, error)
	now          func() time.Time
}

// New builds a Host. org is the Azure DevOps organization URL
// (e.g. "https://dev.azure.com/talroo"). project is the project name
// (e.g. "Product"). repoName is the repository name (e.g. "ai-knowledge-base").
func New(cmd CmdFactory, cliAvailable func() bool, org, project, repoName string) *Host {
	h := &Host{
		cmd:          cmd,
		cliAvailable: cliAvailable,
		org:          strings.TrimSpace(org),
		project:      strings.TrimSpace(project),
		repoName:     strings.TrimSpace(repoName),
		now:          time.Now,
	}
	h.repoIDFn = func(ctx context.Context) (string, error) {
		return h.resolveRepoID(ctx)
	}
	return h
}

// OrgProjectRepo extracts the Azure DevOps organization URL, project, and
// repository name from a remote URL. It handles SSH scp-style
// (git@ssh.dev.azure.com:v3/org/project/repo), HTTPS
// (https://org@dev.azure.com/org/project/_git/repo), and legacy
// visualstudio.com URLs. Returns ("", "", "") when the input is not an
// Azure DevOps URL or cannot be parsed.
func OrgProjectRepo(remoteURL string) (org, project, repo string) {
	raw := strings.TrimSpace(remoteURL)
	if raw == "" {
		return "", "", ""
	}
	lower := strings.ToLower(raw)

	// SSH scp-style: git@ssh.dev.azure.com:v3/org/project/repo
	if strings.Contains(lower, "ssh.dev.azure.com") {
		colon := strings.Index(raw, ":")
		if colon < 0 {
			return "", "", ""
		}
		path := raw[colon+1:]
		path = strings.TrimPrefix(path, "v3/")
		path = strings.Trim(path, "/")
		parts := strings.Split(path, "/")
		if len(parts) >= 3 {
			return orgURL(parts[0]), parts[1], parts[2]
		}
		return "", "", ""
	}

	// HTTPS or SSH URL form
	if strings.Contains(lower, "dev.azure.com") {
		return parseAzureHTTPSURL(raw)
	}

	// Legacy visualstudio.com: https://org.visualstudio.com/project/_git/repo
	if strings.Contains(lower, ".visualstudio.com") {
		return parseVisualStudioURL(raw)
	}

	return "", "", ""
}

func parseAzureHTTPSURL(raw string) (org, project, repo string) {
	// Format: https://[user@]dev.azure.com/org/project/_git/repo
	// or ssh://git@ssh.dev.azure.com:v3/org/project/repo (already handled above)
	schemeEnd := strings.Index(raw, "://")
	if schemeEnd < 0 {
		return "", "", ""
	}
	rest := raw[schemeEnd+3:]
	// Strip userinfo
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	// rest is now "dev.azure.com/org/project/_git/repo"
	slash := strings.Index(rest, "/")
	if slash < 0 {
		return "", "", ""
	}
	rest = rest[slash+1:] // "org/project/_git/repo"
	parts := strings.Split(rest, "/")
	if len(parts) >= 4 && parts[2] == "_git" {
		return orgURL(parts[0]), parts[1], parts[3]
	}
	return "", "", ""
}

func parseVisualStudioURL(raw string) (org, project, repo string) {
	// Format: https://org.visualstudio.com/project/_git/repo
	schemeEnd := strings.Index(raw, "://")
	if schemeEnd < 0 {
		return "", "", ""
	}
	rest := raw[schemeEnd+3:]
	// Strip userinfo
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	// rest is "org.visualstudio.com/project/_git/repo"
	dot := strings.Index(rest, ".visualstudio.com")
	if dot < 0 {
		return "", "", ""
	}
	orgName := rest[:dot]
	rest = rest[strings.Index(rest, "/")+1:]
	parts := strings.Split(rest, "/")
	if len(parts) >= 3 && parts[1] == "_git" {
		return orgURL(orgName), parts[0], parts[2]
	}
	return "", "", ""
}

func orgURL(org string) string {
	return "https://dev.azure.com/" + strings.TrimPrefix(strings.TrimSpace(org), "/")
}

func (h *Host) Provider() scm.Provider { return scm.ProviderAzureDevOps }

func (h *Host) Capabilities() scm.Capabilities {
	return scm.Capabilities{
		MergeableState:  true,
		FailedCheckLogs: true,
		ReviewComments:  true,
	}
}

func (h *Host) Available(ctx context.Context) error {
	if h.cliAvailable != nil && !h.cliAvailable() {
		return errors.New("az CLI is not installed")
	}
	if err := h.cmd(ctx, "az", "account", "show").Run(); err != nil {
		return errors.New("az CLI is not authenticated")
	}
	return nil
}

func (h *Host) orgArgs() []string {
	var args []string
	if h.org != "" {
		args = append(args, "--org", h.org)
	}
	return args
}

func (h *Host) projectArgs() []string {
	var args []string
	if h.project != "" {
		args = append(args, "--project", h.project)
	}
	return args
}

func (h *Host) repoArgs() []string {
	var args []string
	if h.repoName != "" {
		args = append(args, "--repository", h.repoName)
	}
	return args
}

// resolveRepoID resolves the repository GUID from the name. Needed for
// az devops invoke route parameters that require the GUID.
func (h *Host) resolveRepoID(ctx context.Context) (string, error) {
	if h.repoID != "" {
		return h.repoID, nil
	}
	args := append([]string{"repos", "show"}, h.repoArgs()...)
	args = append(args, h.orgArgs()...)
	args = append(args, "-o", "json")
	cmd := h.cmd(ctx, "az", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("az repos show: %s: %w", strings.TrimSpace(string(out)), err)
	}
	var payload struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return "", fmt.Errorf("parse repo ID: %w", err)
	}
	h.repoID = payload.ID
	return h.repoID, nil
}

// Azure PR JSON shapes
type azPR struct {
	PullRequestID   int    `json:"pullRequestId"`
	Status          string `json:"status"`
	MergeStatus     string `json:"mergeStatus"`
	SourceRefName   string `json:"sourceRefName"`
	TargetRefName   string `json:"targetRefName"`
	WebURL          string `json:"url"`
	IsDraft         bool   `json:"isDraft"`
	LastMergeCommit struct {
		CommitID string `json:"commitId"`
	} `json:"lastMergeSourceCommit"`
}

func (p azPR) toPR() *scm.PR {
	pr := &scm.PR{URL: strings.TrimSpace(p.WebURL)}
	if p.PullRequestID > 0 {
		pr.Number = strconv.Itoa(p.PullRequestID)
	}
	return pr
}

func (h *Host) FindPR(ctx context.Context, branch, base string) (*scm.PR, error) {
	args := []string{"repos", "pr", "list", "--status", "active"}
	args = append(args, h.repoArgs()...)
	args = append(args, h.projectArgs()...)
	args = append(args, h.orgArgs()...)
	if strings.TrimSpace(branch) != "" {
		args = append(args, "--source-branch", branch)
	}
	if strings.TrimSpace(base) != "" {
		args = append(args, "--target-branch", base)
	}
	args = append(args, "-o", "json")
	cmd := h.cmd(ctx, "az", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("az repos pr list: %s: %w", strings.TrimSpace(string(out)), err)
	}
	var prs []azPR
	if err := json.Unmarshal(out, &prs); err != nil || len(prs) == 0 {
		return nil, nil
	}
	return prs[0].toPR(), nil
}

func (h *Host) CreatePR(ctx context.Context, branch, base string, content scm.PRContent) (*scm.PR, error) {
	args := []string{"repos", "pr", "create",
		"--source-branch", branch,
		"--target-branch", base,
		"--title", content.Title,
		"--description", content.Body,
	}
	args = append(args, h.repoArgs()...)
	args = append(args, h.projectArgs()...)
	args = append(args, h.orgArgs()...)
	args = append(args, "-o", "json")
	cmd := h.cmd(ctx, "az", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("az repos pr create: %s: %w", strings.TrimSpace(string(out)), err)
	}
	var pr azPR
	if err := json.Unmarshal(out, &pr); err != nil {
		return nil, fmt.Errorf("parse az repos pr create response: %w", err)
	}
	return pr.toPR(), nil
}

func (h *Host) UpdatePR(ctx context.Context, pr *scm.PR, content scm.PRContent) (*scm.PR, error) {
	args := []string{"repos", "pr", "update",
		"--id", pr.Number,
		"--title", content.Title,
		"--description", content.Body,
	}
	args = append(args, h.orgArgs()...)
	cmd := h.cmd(ctx, "az", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("az repos pr update: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return pr, nil
}

func (h *Host) GetPRState(ctx context.Context, pr *scm.PR) (scm.PRState, error) {
	az, err := h.showPR(ctx, pr.Number)
	if err != nil {
		return "", err
	}
	return normalizePRState(az.Status), nil
}

func (h *Host) GetMergeableState(ctx context.Context, pr *scm.PR) (scm.MergeableState, error) {
	az, err := h.showPR(ctx, pr.Number)
	if err != nil {
		return "", err
	}
	return normalizeMergeableState(az.MergeStatus), nil
}

func (h *Host) showPR(ctx context.Context, id string) (azPR, error) {
	args := []string{"repos", "pr", "show", "--id", id}
	args = append(args, h.orgArgs()...)
	args = append(args, "-o", "json")
	cmd := h.cmd(ctx, "az", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return azPR{}, fmt.Errorf("az repos pr show: %s: %w", strings.TrimSpace(string(out)), err)
	}
	var pr azPR
	if err := json.Unmarshal(out, &pr); err != nil {
		return azPR{}, fmt.Errorf("parse az repos pr show response: %w", err)
	}
	return pr, nil
}

// Azure policy JSON shape
type azPolicy struct {
	Status        string `json:"status"`
	Configuration struct {
		IsBlocking bool `json:"isBlocking"`
		IsEnabled  bool `json:"isEnabled"`
		Type       struct {
			DisplayName string `json:"displayName"`
		} `json:"type"`
		Settings struct {
			DisplayName       string `json:"displayName"`
			BuildDefinitionID int    `json:"buildDefinitionId"`
		} `json:"settings"`
	} `json:"configuration"`
	Context struct {
		BuildID                 int    `json:"buildId"`
		BuildIsNotCurrent       bool   `json:"buildIsNotCurrent"`
		LastMergeSourceCommitID string `json:"lastMergeSourceCommitId"`
		BuildStartedUTC         string `json:"buildStartedUtc"`
	} `json:"context"`
}

func (h *Host) GetChecks(ctx context.Context, pr *scm.PR) ([]scm.Check, error) {
	policies, err := h.listPolicies(ctx, pr.Number)
	if err != nil {
		return nil, err
	}
	if len(policies) == 0 {
		return nil, nil
	}
	now := h.now
	if now == nil {
		now = time.Now
	}
	nowTime := now()
	checks := make([]scm.Check, 0, len(policies))
	for _, p := range policies {
		if !p.Configuration.IsEnabled {
			continue
		}
		name := strings.TrimSpace(p.Configuration.Settings.DisplayName)
		if name == "" {
			name = strings.TrimSpace(p.Configuration.Type.DisplayName)
		}
		bucket := policyStatusToBucket(p.Status)
		var completedAt time.Time
		if bucket != scm.CheckBucketPending && bucket != "" {
			completedAt = nowTime
		}
		checks = append(checks, scm.Check{
			Name:        name,
			Bucket:      bucket,
			CompletedAt: completedAt,
		})
	}
	return checks, nil
}

func (h *Host) listPolicies(ctx context.Context, prNumber string) ([]azPolicy, error) {
	args := []string{"repos", "pr", "policy", "list", "--id", prNumber}
	args = append(args, h.orgArgs()...)
	args = append(args, "-o", "json")
	cmd := h.cmd(ctx, "az", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("az repos pr policy list: %s: %w", strings.TrimSpace(string(out)), err)
	}
	var policies []azPolicy
	if err := json.Unmarshal(out, &policies); err != nil {
		return nil, fmt.Errorf("parse policy list: %w", err)
	}
	return policies, nil
}

func (h *Host) FetchFailedCheckLogs(ctx context.Context, pr *scm.PR, _ string, _ string, failingNames []string) (string, error) {
	if len(failingNames) == 0 {
		return "", nil
	}
	policies, err := h.listPolicies(ctx, pr.Number)
	if err != nil {
		return "", nil
	}
	targets := make(map[string]struct{}, len(failingNames))
	for _, name := range failingNames {
		name = strings.TrimSpace(name)
		if name != "" {
			targets[name] = struct{}{}
		}
	}
	for _, p := range policies {
		if p.Status != "rejected" {
			continue
		}
		name := strings.TrimSpace(p.Configuration.Settings.DisplayName)
		if name == "" {
			name = strings.TrimSpace(p.Configuration.Type.DisplayName)
		}
		if len(targets) > 0 {
			if _, ok := targets[name]; !ok {
				continue
			}
		}
		if p.Context.BuildID == 0 {
			continue
		}
		logs := h.fetchBuildLogs(ctx, p.Context.BuildID)
		if logs != "" {
			return logs, nil
		}
	}
	return "", nil
}

func (h *Host) fetchBuildLogs(ctx context.Context, buildID int) string {
	args := []string{"pipelines", "runs", "show", "--id", strconv.Itoa(buildID)}
	args = append(args, h.projectArgs()...)
	args = append(args, h.orgArgs()...)
	args = append(args, "-o", "json")
	cmd := h.cmd(ctx, "az", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return ""
	}
	var payload struct {
		Logs struct {
			URL string `json:"url"`
		} `json:"logs"`
		BuildNumber string `json:"buildNumber"`
		Result      string `json:"result"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Azure thread JSON shape
type azThread struct {
	ID            int    `json:"id"`
	Status        string `json:"status"`
	ThreadContext *struct {
		FilePath       string `json:"filePath"`
		RightFileStart *struct {
			Line int `json:"line"`
		} `json:"rightFileStart"`
	} `json:"threadContext"`
	Comments []struct {
		Author struct {
			DisplayName string `json:"displayName"`
		} `json:"author"`
		CommentType string `json:"commentType"`
		Content     string `json:"content"`
	} `json:"comments"`
}

func (h *Host) GetReviewThreads(ctx context.Context, pr *scm.PR) ([]scm.ReviewThread, error) {
	repoID, err := h.repoIDFn(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve repo ID for threads: %w", err)
	}
	args := []string{
		"devops", "invoke",
		"--area", "git",
		"--resource", "pullRequestThreads",
		"--route-parameters", "project=" + h.project, "repositoryId=" + repoID, "pullRequestId=" + pr.Number,
		"--org", h.org,
		"--api-version", "7.1",
		"-o", "json",
	}
	cmd := h.cmd(ctx, "az", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("az devops invoke pullRequestThreads: %s: %w", strings.TrimSpace(string(out)), err)
	}
	var payload struct {
		Value []azThread `json:"value"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		// Some az versions return a bare array instead of {value: [...]}
		var threads []azThread
		if err2 := json.Unmarshal(out, &threads); err2 != nil {
			return nil, fmt.Errorf("parse pullRequestThreads: %w", err)
		}
		payload.Value = threads
	}
	result := make([]scm.ReviewThread, 0, len(payload.Value))
	for _, t := range payload.Value {
		if len(t.Comments) == 0 {
			continue
		}
		c := t.Comments[0]
		if c.CommentType == "system" {
			continue
		}
		thread := scm.ReviewThread{
			ID:       strconv.Itoa(t.ID),
			Author:   strings.TrimSpace(c.Author.DisplayName),
			Body:     strings.TrimSpace(c.Content),
			Resolved: isThreadResolved(t.Status),
		}
		if t.ThreadContext != nil {
			thread.File = strings.TrimSpace(t.ThreadContext.FilePath)
			if t.ThreadContext.RightFileStart != nil {
				thread.Line = t.ThreadContext.RightFileStart.Line
			}
		}
		result = append(result, thread)
	}
	return result, nil
}

func (h *Host) GetReviewPass(ctx context.Context, pr *scm.PR, reviewerIdentity string) (*scm.ReviewPass, error) {
	policies, err := h.listPolicies(ctx, pr.Number)
	if err != nil {
		return nil, err
	}
	pass := &scm.ReviewPass{}
	for _, p := range policies {
		name := strings.TrimSpace(p.Configuration.Settings.DisplayName)
		if name == "" {
			continue
		}
		// Match by policy display name or reviewer identity.
		// The caller passes the configured policy_name; if empty, match any
		// Build policy whose comments come from the reviewer identity.
		if reviewerIdentity != "" && !strings.EqualFold(name, reviewerIdentity) {
			continue
		}
		pass.Ran = true
		pass.ForSHA = strings.TrimSpace(p.Context.LastMergeSourceCommitID)
		switch strings.ToLower(strings.TrimSpace(p.Status)) {
		case "approved", "rejected", "notapplicable":
			pass.Complete = true
		case "queued", "running":
			pass.Complete = false
		default:
			pass.Complete = false
		}
		if !p.Context.BuildIsNotCurrent {
			// Build is current - this is the latest pass
			return pass, nil
		}
	}
	return pass, nil
}

func isThreadResolved(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "active", "pending", "":
		return false
	default:
		return true
	}
}

func normalizePRState(raw string) scm.PRState {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "active":
		return scm.PRStateOpen
	case "completed":
		return scm.PRStateMerged
	case "abandoned":
		return scm.PRStateClosed
	default:
		return scm.PRState(strings.ToUpper(raw))
	}
}

func normalizeMergeableState(raw string) scm.MergeableState {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "succeeded":
		return scm.MergeableOK
	case "conflicts", "rejectedbypolicy":
		return scm.MergeableConflict
	case "queued", "notset", "":
		return scm.MergeablePending
	default:
		return scm.MergeablePending
	}
}

func policyStatusToBucket(status string) scm.CheckBucket {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "approved":
		return scm.CheckBucketPass
	case "rejected":
		return scm.CheckBucketFail
	case "queued", "running":
		return scm.CheckBucketPending
	case "notapplicable", "broken":
		return scm.CheckBucketSkip
	default:
		return ""
	}
}
