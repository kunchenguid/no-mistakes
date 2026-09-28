package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func (h *Host) GetChecksForHead(ctx context.Context, pr *scm.PR, expectedHead string) ([]scm.Check, error) {
	if strings.TrimSpace(expectedHead) == "" || h.projectPath == "" {
		return nil, fmt.Errorf("missing exact head or GitLab project")
	}
	pipelineID, err := h.recheckMRHead(ctx, pr, expectedHead)
	if err != nil {
		return nil, err
	}
	endpoint := "projects/" + url.PathEscape(h.projectPath) + "/pipelines"
	out, err := h.cmd(ctx, "glab", "api", fmt.Sprintf("%s/%d", endpoint, pipelineID)).Output()
	if err != nil {
		return nil, fmt.Errorf("read current GitLab MR pipeline: %w", err)
	}
	var p struct {
		ID     int    `json:"id"`
		SHA    string `json:"sha"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(out, &p); err != nil {
		return nil, fmt.Errorf("unreadable GitLab pipeline: %w", err)
	}
	if p.ID != pipelineID || p.SHA != expectedHead || p.Status == "" {
		return nil, fmt.Errorf("current MR pipeline is not bound to expected head %s", expectedHead)
	}
	bucket := scm.CheckBucketPending
	if p.Status == "success" {
		bucket = scm.CheckBucketPass
	} else if p.Status == "failed" {
		bucket = scm.CheckBucketFail
	}
	checks := []scm.Check{{Name: fmt.Sprintf("pipeline %d", p.ID), Bucket: bucket, State: p.Status}}
	// Retain the ordinary job verdicts as well as pipeline-level verdicts. A
	// trigger-only root can have bridges instead of jobs; both are required reads.
	var jobs []scm.Check
	for _, kind := range []string{"jobs", "bridges"} {
		out, err := h.cmd(ctx, "glab", "api", "--paginate", fmt.Sprintf("%s/%d/%s", endpoint, pipelineID, kind)).Output()
		if err != nil {
			return nil, fmt.Errorf("read GitLab pipeline %s: %w", kind, err)
		}
		parsed, err := recheckJobs(out, kind == "bridges")
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, parsed...)
	}
	if len(jobs) == 0 {
		return nil, fmt.Errorf("current MR pipeline has no jobs or bridges")
	}
	checks = append(checks, jobs...)
	current, err := h.recheckMRHead(ctx, pr, expectedHead)
	if err != nil {
		return nil, err
	}
	if current != pipelineID {
		return nil, fmt.Errorf("MR pipeline changed during CI recheck")
	}
	return checks, nil
}

// Unlike the older CLI job parser, REST recheck reads require an actual array
// on every page. A missing, null, or corrupt later page cannot hide a check.
func recheckJobs(out []byte, bridges bool) ([]scm.Check, error) {
	decoder := json.NewDecoder(bytes.NewReader(out))
	pages := 0
	var checks []scm.Check
	for {
		var page []struct {
			gitlabJob
			Downstream *struct {
				ID     int    `json:"id"`
				SHA    string `json:"sha"`
				Status string `json:"status"`
			} `json:"downstream_pipeline"`
		}
		err := decoder.Decode(&page)
		if err == io.EOF {
			break
		}
		if err != nil || page == nil {
			return nil, fmt.Errorf("unreadable GitLab job page: %v", err)
		}
		pages++
		for _, job := range page {
			if job.ID <= 0 || job.Name == "" || job.Status == "" {
				return nil, fmt.Errorf("incomplete GitLab job result")
			}
			if bridges && (job.Status == "success" || job.Downstream != nil) && (job.Downstream == nil || job.Downstream.ID <= 0 || strings.TrimSpace(job.Downstream.SHA) == "" || job.Downstream.Status != "success") {
				return nil, fmt.Errorf("bridge %s has no verified successful downstream", job.Name)
			}
			checks = append(checks, jobsToChecks([]gitlabJob{job.gitlabJob})...)
		}
	}
	if pages == 0 {
		return nil, fmt.Errorf("GitLab returned no job pages")
	}
	return checks, nil
}

func (h *Host) recheckMRHead(ctx context.Context, pr *scm.PR, expectedHead string) (int, error) {
	out, err := h.cmd(ctx, "glab", "mr", "view", pr.Number, "--output", "json").Output()
	if err != nil {
		return 0, fmt.Errorf("read GitLab MR head: %w", err)
	}
	var mr struct {
		SHA          string `json:"sha"`
		State        string `json:"state"`
		HeadPipeline struct {
			ID  int    `json:"id"`
			SHA string `json:"sha"`
		} `json:"head_pipeline"`
	}
	if err := json.Unmarshal(bytesTrimToJSON(out), &mr); err != nil {
		return 0, err
	}
	if mr.State != "opened" || mr.SHA != expectedHead || mr.HeadPipeline.SHA != expectedHead || mr.HeadPipeline.ID <= 0 {
		return 0, fmt.Errorf("MR or pipeline is not open at expected head %s", expectedHead)
	}
	return mr.HeadPipeline.ID, nil
}
