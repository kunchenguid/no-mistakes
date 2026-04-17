// Package gitlab implements scm.Host backed by the glab CLI.
package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// CmdFactory builds an exec.Cmd in the caller's workdir with the caller's env.
type CmdFactory func(ctx context.Context, name string, args ...string) *exec.Cmd

// Host talks to GitLab through the glab CLI.
type Host struct {
	cmd          CmdFactory
	cliAvailable func() bool
}

// New builds a Host. cliAvailable reports whether the glab binary is
// resolvable on the caller's PATH (possibly overridden by env).
func New(cmd CmdFactory, cliAvailable func() bool) *Host {
	return &Host{cmd: cmd, cliAvailable: cliAvailable}
}

func (h *Host) Provider() scm.Provider { return scm.ProviderGitLab }

func (h *Host) Capabilities() scm.Capabilities {
	return scm.Capabilities{MergeableState: true, FailedCheckLogs: true}
}

func (h *Host) Available(ctx context.Context) error {
	if h.cliAvailable != nil && !h.cliAvailable() {
		return errors.New("glab CLI is not installed")
	}
	if err := h.cmd(ctx, "glab", "auth", "status").Run(); err != nil {
		return errors.New("glab CLI is not authenticated")
	}
	return nil
}

type mrPayload struct {
	IID                 int    `json:"iid"`
	WebURL              string `json:"web_url"`
	URL                 string `json:"url"`
	State               string `json:"state"`
	HasConflicts        bool   `json:"has_conflicts"`
	DetailedMergeStatus string `json:"detailed_merge_status"`
	MergeStatus         string `json:"merge_status"`
}

func (p mrPayload) toPR() *scm.PR {
	url := strings.TrimSpace(p.WebURL)
	if url == "" {
		url = strings.TrimSpace(p.URL)
	}
	pr := &scm.PR{URL: url}
	if p.IID > 0 {
		pr.Number = fmt.Sprintf("%d", p.IID)
	}
	return pr
}

func (h *Host) FindPR(ctx context.Context, branch, _ string) (*scm.PR, error) {
	cmd := h.cmd(ctx, "glab", "mr", "view", branch, "--output", "json")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, nil
	}
	mr, ok := parseMRPayload(out)
	if !ok || strings.TrimSpace(mr.WebURL) == "" && strings.TrimSpace(mr.URL) == "" {
		return nil, nil
	}
	return mr.toPR(), nil
}

func (h *Host) CreatePR(ctx context.Context, branch, base string, content scm.PRContent) (*scm.PR, error) {
	cmd := h.cmd(ctx, "glab", "mr", "create",
		"--source-branch", branch,
		"--target-branch", base,
		"--title", content.Title,
		"--description", content.Body,
		"--yes",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("glab mr create: %s: %w", strings.TrimSpace(string(out)), err)
	}
	url := extractMRURL(out)
	pr := &scm.PR{URL: url}
	if num, nerr := scm.ExtractPRNumber(url); nerr == nil {
		pr.Number = num
	}
	return pr, nil
}

func (h *Host) UpdatePR(ctx context.Context, pr *scm.PR, content scm.PRContent) (*scm.PR, error) {
	id := pr.Number
	if id == "" {
		id = pr.URL
	}
	cmd := h.cmd(ctx, "glab", "mr", "update", id,
		"--title", content.Title,
		"--description", content.Body,
		"--yes",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("glab mr update: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return pr, nil
}

func (h *Host) GetPRState(ctx context.Context, pr *scm.PR) (scm.PRState, error) {
	mr, err := h.viewMR(ctx, pr.Number)
	if err != nil {
		return "", err
	}
	return normalizePRState(mr.State), nil
}

func (h *Host) GetMergeableState(ctx context.Context, pr *scm.PR) (scm.MergeableState, error) {
	mr, err := h.viewMR(ctx, pr.Number)
	if err != nil {
		return "", err
	}
	if mr.HasConflicts {
		return scm.MergeableConflict, nil
	}
	// detailed_merge_status is preferred; merge_status is the legacy field.
	status := strings.ToLower(strings.TrimSpace(mr.DetailedMergeStatus))
	if status == "" {
		status = strings.ToLower(strings.TrimSpace(mr.MergeStatus))
	}
	switch status {
	case "mergeable", "can_be_merged":
		return scm.MergeableOK, nil
	case "broken_status", "cannot_be_merged":
		return scm.MergeableConflict, nil
	case "checking", "unchecked", "ci_still_running", "":
		return scm.MergeablePending, nil
	default:
		return scm.MergeableOK, nil
	}
}

func (h *Host) viewMR(ctx context.Context, id string) (mrPayload, error) {
	cmd := h.cmd(ctx, "glab", "mr", "view", id, "--output", "json")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return mrPayload{}, fmt.Errorf("glab mr view: %s: %w", strings.TrimSpace(string(out)), err)
	}
	mr, ok := parseMRPayload(out)
	if !ok {
		return mrPayload{}, fmt.Errorf("glab mr view: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	return mr, nil
}

func (h *Host) GetChecks(ctx context.Context, pr *scm.PR) ([]scm.Check, error) {
	// glab ci status --mr <id> --output json lists jobs for the MR's latest pipeline.
	// Not all glab versions support --mr; fall back to listing pipelines by branch via view.
	cmd := h.cmd(ctx, "glab", "ci", "status", "--mr", pr.Number, "--output", "json")
	out, err := cmd.CombinedOutput()
	if err != nil {
		if !isUnsupportedMRFlagError(out) {
			return nil, fmt.Errorf("glab ci status: %s: %w", strings.TrimSpace(string(out)), err)
		}
		return h.getChecksFallback(ctx, pr)
	}
	return parseGitlabJobs(out)
}

func isUnsupportedMRFlagError(out []byte) bool {
	msg := strings.ToLower(strings.TrimSpace(string(out)))
	return strings.Contains(msg, "unknown flag: --mr") || strings.Contains(msg, "unknown option: --mr")
}

func (h *Host) getChecksFallback(ctx context.Context, pr *scm.PR) ([]scm.Check, error) {
	// Try fetching the MR's pipeline and listing its jobs.
	cmd := h.cmd(ctx, "glab", "mr", "view", pr.Number, "--output", "json")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("glab mr view: %s: %w", strings.TrimSpace(string(out)), err)
	}
	var payload struct {
		HeadPipeline struct {
			ID int `json:"id"`
		} `json:"head_pipeline"`
	}
	trimmed := bytesTrimToJSON(out)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("glab mr view: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	if err := json.Unmarshal(trimmed, &payload); err != nil {
		return nil, fmt.Errorf("glab mr view: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	if payload.HeadPipeline.ID == 0 {
		return nil, nil
	}
	jobsCmd := h.cmd(ctx, "glab", "ci", "get", "--pipeline-id", fmt.Sprintf("%d", payload.HeadPipeline.ID), "--output", "json")
	jobsOut, err := jobsCmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("glab ci get: %s: %w", strings.TrimSpace(string(jobsOut)), err)
	}
	return parseGitlabJobs(jobsOut)
}

func (h *Host) FetchFailedCheckLogs(ctx context.Context, pr *scm.PR, _ string, _ string, failingNames []string) (string, error) {
	if len(failingNames) == 0 {
		return "", nil
	}
	// Get the MR's pipeline jobs, find a failed one whose name matches, trace it.
	viewCmd := h.cmd(ctx, "glab", "mr", "view", pr.Number, "--output", "json")
	viewOut, err := viewCmd.CombinedOutput()
	if err != nil {
		return "", nil
	}
	var payload struct {
		HeadPipeline struct {
			ID int `json:"id"`
		} `json:"head_pipeline"`
	}
	if trimmed := bytesTrimToJSON(viewOut); len(trimmed) == 0 || json.Unmarshal(trimmed, &payload) != nil || payload.HeadPipeline.ID == 0 {
		return "", nil
	}
	jobsCmd := h.cmd(ctx, "glab", "ci", "get", "--pipeline-id", fmt.Sprintf("%d", payload.HeadPipeline.ID), "--output", "json")
	jobsOut, err := jobsCmd.CombinedOutput()
	if err != nil {
		return "", nil
	}
	jobID := findFailedJobID(jobsOut, failingNames)
	if jobID == 0 {
		return "", nil
	}
	traceCmd := h.cmd(ctx, "glab", "ci", "trace", fmt.Sprintf("%d", jobID))
	traceOut, _ := traceCmd.Output()
	return strings.TrimSpace(string(traceOut)), nil
}

func parseMRPayload(out []byte) (mrPayload, bool) {
	trimmed := bytesTrimToJSON(out)
	if len(trimmed) == 0 {
		return mrPayload{}, false
	}
	var mr mrPayload
	if err := json.Unmarshal(trimmed, &mr); err != nil {
		return mrPayload{}, false
	}
	return mr, true
}

func bytesTrimToJSON(out []byte) []byte {
	// glab may emit a banner line before JSON; skip until '{'.
	idx := -1
	for i, b := range out {
		if b == '{' || b == '[' {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil
	}
	return out[idx:]
}

type gitlabJob struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Stage  string `json:"stage"`
}

func parseGitlabJobs(out []byte) ([]scm.Check, error) {
	trimmed := bytesTrimToJSON(out)
	if len(trimmed) == 0 {
		return nil, nil
	}
	// glab may return a single pipeline object with nested jobs, or a bare job array.
	var asArray []gitlabJob
	if err := json.Unmarshal(trimmed, &asArray); err == nil && len(asArray) > 0 {
		return jobsToChecks(asArray), nil
	}
	var asObject struct {
		Jobs []gitlabJob `json:"jobs"`
	}
	if err := json.Unmarshal(trimmed, &asObject); err == nil && len(asObject.Jobs) > 0 {
		return jobsToChecks(asObject.Jobs), nil
	}
	return nil, nil
}

func jobsToChecks(jobs []gitlabJob) []scm.Check {
	checks := make([]scm.Check, 0, len(jobs))
	for _, job := range jobs {
		checks = append(checks, scm.Check{Name: job.Name, Bucket: gitlabStatusBucket(job.Status)})
	}
	return checks
}

func findFailedJobID(out []byte, failingNames []string) int {
	trimmed := bytesTrimToJSON(out)
	if len(trimmed) == 0 {
		return 0
	}
	targets := map[string]struct{}{}
	for _, name := range failingNames {
		name = strings.TrimSpace(name)
		if name != "" {
			targets[name] = struct{}{}
		}
	}
	var asArray []gitlabJob
	jobs := asArray
	if err := json.Unmarshal(trimmed, &asArray); err == nil && len(asArray) > 0 {
		jobs = asArray
	} else {
		var asObject struct {
			Jobs []gitlabJob `json:"jobs"`
		}
		if err := json.Unmarshal(trimmed, &asObject); err == nil {
			jobs = asObject.Jobs
		}
	}
	for _, job := range jobs {
		if !strings.EqualFold(job.Status, "failed") {
			continue
		}
		if _, ok := targets[job.Name]; ok || len(targets) == 0 {
			return job.ID
		}
	}
	return 0
}

func gitlabStatusBucket(state string) scm.CheckBucket {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "success":
		return scm.CheckBucketPass
	case "failed":
		return scm.CheckBucketFail
	case "canceled", "cancelled":
		return scm.CheckBucketCancel
	case "skipped":
		return scm.CheckBucketSkip
	case "manual":
		return scm.CheckBucketSkip
	case "pending", "running", "created", "waiting_for_resource", "preparing", "scheduled":
		return scm.CheckBucketPending
	default:
		return ""
	}
}

func normalizePRState(raw string) scm.PRState {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "opened", "open":
		return scm.PRStateOpen
	case "merged":
		return scm.PRStateMerged
	case "closed", "locked":
		return scm.PRStateClosed
	default:
		return scm.PRState(strings.ToUpper(raw))
	}
}

func extractMRURL(raw []byte) string {
	text := strings.TrimSpace(string(raw))
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://") {
			return line
		}
	}
	trimmed := bytesTrimToJSON(raw)
	if len(trimmed) == 0 {
		return ""
	}
	var payload map[string]any
	if err := json.Unmarshal(trimmed, &payload); err != nil {
		return ""
	}
	for _, key := range []string{"web_url", "url", "webUrl"} {
		if value, ok := payload[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
